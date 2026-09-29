package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"beryju.io/ldap"
	"github.com/getsentry/sentry-go"
	ber "github.com/nmcclain/asn1-ber"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"goauthentik.io/internal/config"
	"goauthentik.io/internal/outpost/ak"
	"goauthentik.io/internal/outpost/ldap/constants"
	"goauthentik.io/internal/outpost/ldap/flags"
	"goauthentik.io/internal/outpost/ldap/group"
	"goauthentik.io/internal/outpost/ldap/metrics"
	"goauthentik.io/internal/outpost/ldap/search"
	"goauthentik.io/internal/outpost/ldap/search/direct"
	"goauthentik.io/internal/outpost/ldap/server"
	"goauthentik.io/internal/outpost/ldap/utils"
	api "goauthentik.io/packages/client-go"
)

// snapshot is an immutable view of the directory. Every fetch replaces it
// wholesale, so a request must copy out whatever it needs and never retain a
// pointer into one: such a pointer would keep the entire stale snapshot alive
// for as long as it is held.
type snapshot struct {
	users     []api.User
	groups    []api.Group
	usersByPk map[int32]int // index into users
	// Lookup indexes keyed by lower-cased value, so that base-DN lookups and
	// simple equality filters don't build an entry for every object in the
	// directory. See candidates.go.
	usersByUsername map[string]int
	usersByEmail    map[string][]int
	groupsByName    map[string]int
}

type MemorySearcher struct {
	si  server.LDAPServerInstance
	log *log.Entry
	ds  *direct.DirectSearcher

	cache atomic.Pointer[snapshot]
	// fetchMutex serialises full reloads. The refresh interval, websocket
	// updates and SIGUSR1 can all trigger one, and they must not run at the
	// same time against a large directory.
	fetchMutex sync.Mutex
}

func NewMemorySearcher(si server.LDAPServerInstance, existing search.Searcher) *MemorySearcher {
	ms := &MemorySearcher{
		si:  si,
		log: log.WithField("logger", "authentik.outpost.ldap.searcher.memory"),
		ds:  direct.NewDirectSearcher(si),
	}
	if existing != nil {
		if ems, ok := existing.(*MemorySearcher); ok {
			ems.si = si
			ems.fetch()
			ems.log.Debug("re-initialized memory searcher")
			return ems
		}
	}
	ms.fetch()
	ms.log.Debug("initialized memory searcher")
	return ms
}

func (ms *MemorySearcher) fetch() {
	ms.fetchMutex.Lock()
	defer ms.fetchMutex.Unlock()
	start := time.Now()
	// A failed or partial fetch must not replace a complete snapshot, or a
	// single API error would empty the directory until the next refresh.
	users, err := ak.Paginator(ms.si.GetAPIClient().CoreAPI.CoreUsersList(context.TODO()).IncludeGroups(true), ak.PaginatorOptions{
		PageSize: config.Get().LDAP.PageSize,
		Logger:   ms.log,
	})
	if err != nil {
		ms.log.WithError(err).Warning("failed to fetch users, keeping previous snapshot")
		return
	}
	groups, err := ak.Paginator(ms.si.GetAPIClient().CoreAPI.CoreGroupsList(context.TODO()).IncludeUsers(true).IncludeChildren(true).IncludeParents(true), ak.PaginatorOptions{
		PageSize: config.Get().LDAP.PageSize,
		Logger:   ms.log,
	})
	if err != nil {
		ms.log.WithError(err).Warning("failed to fetch groups, keeping previous snapshot")
		return
	}
	ms.log.WithField("users", len(users)).WithField("groups", len(groups)).WithField("took", time.Since(start).String()).Info("fetched directory")
	ms.cache.Store(newSnapshot(users, groups))
}

func (ms *MemorySearcher) SearchBase(req *search.Request) (ldap.ServerSearchResult, error) {
	return ms.ds.SearchBase(req)
}

func (ms *MemorySearcher) SearchSubschema(req *search.Request) (ldap.ServerSearchResult, error) {
	return ms.ds.SearchSubschema(req)
}

func (ms *MemorySearcher) Search(req *search.Request) (ldap.ServerSearchResult, error) {
	accsp := sentry.StartSpan(req.Context(), "authentik.providers.ldap.search.check_access")
	baseDN := ms.si.GetBaseDN()

	if len(req.BindDN) < 1 {
		metrics.RequestsRejected.With(prometheus.Labels{
			"outpost_name": ms.si.GetOutpostName(),
			"type":         "search",
			"reason":       "empty_bind_dn",
			"app":          ms.si.GetAppSlug(),
		}).Inc()
		return ldap.ServerSearchResult{ResultCode: ldap.LDAPResultInsufficientAccessRights}, fmt.Errorf("Search Error: Anonymous BindDN not allowed %s", req.BindDN)
	}
	if !utils.HasSuffixNoCase(req.BindDN, ","+baseDN) {
		metrics.RequestsRejected.With(prometheus.Labels{
			"outpost_name": ms.si.GetOutpostName(),
			"type":         "search",
			"reason":       "invalid_bind_dn",
			"app":          ms.si.GetAppSlug(),
		}).Inc()
		return ldap.ServerSearchResult{ResultCode: ldap.LDAPResultInsufficientAccessRights}, fmt.Errorf("Search Error: BindDN %s not in our BaseDN %s", req.BindDN, ms.si.GetBaseDN())
	}

	flag := ms.si.GetFlags(req.BindDN)
	if flag == nil || (flag.UserInfo == nil && flag.UserPk == flags.InvalidUserPK) {
		req.Log().Debug("User info not cached")
		metrics.RequestsRejected.With(prometheus.Labels{
			"outpost_name": ms.si.GetOutpostName(),
			"type":         "search",
			"reason":       "user_info_not_cached",
			"app":          ms.si.GetAppSlug(),
		}).Inc()
		return ldap.ServerSearchResult{ResultCode: ldap.LDAPResultInsufficientAccessRights}, errors.New("access denied")
	}
	accsp.Finish()

	snap := ms.cache.Load()
	if snap == nil {
		// No fetch has completed yet; treat the directory as empty.
		snap = &snapshot{}
	}

	entries := make([]*ldap.Entry, 0)

	scope := req.Scope
	needUsers, needGroups := ms.si.GetNeededObjects(scope, req.BaseDN, req.FilterObjectClass)

	if scope >= 0 && strings.EqualFold(req.BaseDN, baseDN) {
		if utils.IncludeObjectClass(req.FilterObjectClass, constants.GetDomainOCs()) {
			rootEntries, _ := ms.SearchBase(req)
			for _, e := range rootEntries.Entries {
				e.DN = ms.si.GetBaseDN()
				entries = append(entries, e)
			}
		}

		scope -= 1 // Bring it from WholeSubtree to SingleLevel and so on
	}

	var users *[]api.User
	var groups []*group.LDAPGroup
	var err error

	// The filter is only used to narrow the candidates; the LDAP server
	// library applies it in full to the returned entries. A filter that
	// doesn't parse therefore just means every object is a candidate.
	var filter *ber.Packet
	if needUsers || needGroups {
		if parsed, ferr := ldap.CompileFilter(req.Filter); ferr == nil {
			filter = parsed
		}
	}

	if needUsers {
		if flag.CanSearch {
			if candidates, ok := snap.userCandidates(req.BaseDN, filter, ms.si); ok {
				users = &candidates
			} else {
				users = &snap.users
			}
		} else {
			if idx, ok := snap.usersByPk[flag.UserPk]; ok {
				u := []api.User{snap.users[idx]}
				users = &u
			} else {
				req.Log().WithField("pk", flag.UserPk).Warning("User with pk is not in local cache")
				err = fmt.Errorf("failed to get userinfo")
			}
		}
	}

	if needGroups {
		var candidates []api.Group
		if flag.CanSearch {
			var ok bool
			if candidates, ok = snap.groupCandidates(req.BaseDN, filter, ms.si); !ok {
				candidates = snap.groups
			}
		} else {
			// If the user cannot search, we're going to only return
			// the groups they're in _and_ only return themselves
			// as a member.
			candidates = snap.groupsOfUser(flag.UserPk)
		}
		groups = make([]*group.LDAPGroup, 0, len(candidates))
		for _, g := range candidates {
			groups = append(groups, group.FromAPIGroup(g, ms.si))
		}
	}

	if err != nil {
		return ldap.ServerSearchResult{ResultCode: ldap.LDAPResultOperationsError}, err
	}

	if scope >= 0 && (strings.EqualFold(req.BaseDN, ms.si.GetBaseDN()) || utils.HasSuffixNoCase(req.BaseDN, ms.si.GetBaseUserDN())) {
		singleu := utils.HasSuffixNoCase(req.BaseDN, ","+ms.si.GetBaseUserDN())

		if !singleu && utils.IncludeObjectClass(req.FilterObjectClass, constants.GetContainerOCs()) {
			entries = append(entries, utils.GetContainerEntry(req.FilterObjectClass, ms.si.GetBaseUserDN(), constants.OUUsers))
			scope -= 1
		}

		if scope >= 0 && users != nil && utils.IncludeObjectClass(req.FilterObjectClass, constants.GetUserOCs()) {
			for _, u := range *users {
				entry := ms.si.UserEntry(u)
				if strings.EqualFold(req.BaseDN, entry.DN) || !singleu {
					entries = append(entries, entry)
				}
			}
		}

		scope += 1 // Return the scope to what it was before we descended
	}

	if scope >= 0 && (strings.EqualFold(req.BaseDN, ms.si.GetBaseDN()) || utils.HasSuffixNoCase(req.BaseDN, ms.si.GetBaseGroupDN())) {
		singleg := utils.HasSuffixNoCase(req.BaseDN, ","+ms.si.GetBaseGroupDN())

		if !singleg && utils.IncludeObjectClass(req.FilterObjectClass, constants.GetContainerOCs()) {
			entries = append(entries, utils.GetContainerEntry(req.FilterObjectClass, ms.si.GetBaseGroupDN(), constants.OUGroups))
			scope -= 1
		}

		if scope >= 0 && groups != nil && utils.IncludeObjectClass(req.FilterObjectClass, constants.GetGroupOCs()) {
			for _, g := range groups {
				if strings.EqualFold(req.BaseDN, g.DN) || !singleg {
					entries = append(entries, g.Entry())
				}
			}
		}

		scope += 1 // Return the scope to what it was before we descended
	}

	if scope >= 0 && (strings.EqualFold(req.BaseDN, ms.si.GetBaseDN()) || utils.HasSuffixNoCase(req.BaseDN, ms.si.GetBaseVirtualGroupDN())) {
		singlevg := utils.HasSuffixNoCase(req.BaseDN, ","+ms.si.GetBaseVirtualGroupDN())

		if !singlevg && utils.IncludeObjectClass(req.FilterObjectClass, constants.GetContainerOCs()) {
			entries = append(entries, utils.GetContainerEntry(req.FilterObjectClass, ms.si.GetBaseVirtualGroupDN(), constants.OUVirtualGroups))
			scope -= 1
		}

		if scope >= 0 && users != nil && utils.IncludeObjectClass(req.FilterObjectClass, constants.GetVirtualGroupOCs()) {
			for _, u := range *users {
				entry := group.FromAPIUser(u, ms.si).Entry()
				if strings.EqualFold(req.BaseDN, entry.DN) || !singlevg {
					entries = append(entries, entry)
				}
			}
		}
	}

	return ldap.ServerSearchResult{Entries: entries, Referrals: []string{}, Controls: []ldap.Control{}, ResultCode: ldap.LDAPResultSuccess}, nil
}
