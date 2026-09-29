package memory

import (
	"strings"

	"beryju.io/ldap"
	goldap "github.com/go-ldap/ldap/v3"
	ber "github.com/nmcclain/asn1-ber"
	"goauthentik.io/internal/outpost/ldap/constants"
	"goauthentik.io/internal/outpost/ldap/server"
	"goauthentik.io/internal/outpost/ldap/utils"
	api "goauthentik.io/packages/client-go"
)

// newSnapshot indexes the fetched directory. All keys are lower-cased, as
// LDAP attribute values in DNs and equality filters are matched
// case-insensitively.
func newSnapshot(users []api.User, groups []api.Group) *snapshot {
	s := &snapshot{
		users:           users,
		groups:          groups,
		usersByPk:       make(map[int32]int, len(users)),
		usersByUsername: make(map[string]int, len(users)),
		usersByEmail:    make(map[string][]int, len(users)),
		groupsByName:    make(map[string]int, len(groups)),
	}
	for i, u := range users {
		s.usersByPk[u.Pk] = i
		s.usersByUsername[strings.ToLower(u.Username)] = i
		if u.Email != nil && *u.Email != "" {
			email := strings.ToLower(*u.Email)
			s.usersByEmail[email] = append(s.usersByEmail[email], i)
		}
	}
	for i, g := range groups {
		s.groupsByName[strings.ToLower(g.Name)] = i
	}
	return s
}

// firstRDN returns the attribute name and value of the first RDN of dn, and
// the value of the second RDN (the OU in this outpost's tree, if any).
func firstRDN(dn string) (attr string, value string, ou string, ok bool) {
	parsed, err := goldap.ParseDN(dn)
	if err != nil || len(parsed.RDNs) == 0 || len(parsed.RDNs[0].Attributes) == 0 {
		return "", "", "", false
	}
	if len(parsed.RDNs) > 1 && len(parsed.RDNs[1].Attributes) > 0 {
		ou = parsed.RDNs[1].Attributes[0].Value
	}
	return parsed.RDNs[0].Attributes[0].Type, parsed.RDNs[0].Attributes[0].Value, ou, true
}

// equalityMatch returns the attribute name (lower-cased) and value of an
// equality filter node.
func equalityMatch(f *ber.Packet) (string, string, bool) {
	if f.Tag != ldap.FilterEqualityMatch || len(f.Children) < 2 {
		return "", "", false
	}
	k, ok := f.Children[0].Value.(string)
	if !ok {
		return "", "", false
	}
	v, ok := f.Children[1].Value.(string)
	if !ok {
		return "", "", false
	}
	return strings.ToLower(k), v, true
}

// indexSet collects indexes without duplicates, preserving order.
type indexSet struct {
	seen map[int]struct{}
	list []int
}

func (is *indexSet) add(i int) {
	if is.seen == nil {
		is.seen = map[int]struct{}{}
	}
	if _, ok := is.seen[i]; ok {
		return
	}
	is.seen[i] = struct{}{}
	is.list = append(is.list, i)
}

// narrow walks a filter and returns the indexes of the objects that can
// match it, using lookup for each equality node. It returns false when the
// filter can't be narrowed and every object is a candidate. The result is
// always a superset of the actual matches; the LDAP server library applies
// the full filter afterwards.
func narrow(f *ber.Packet, lookup func(attr string, value string) ([]int, bool)) ([]int, bool) {
	switch f.Tag {
	case ldap.FilterEqualityMatch:
		attr, value, ok := equalityMatch(f)
		if !ok {
			return nil, false
		}
		return lookup(attr, value)
	case ldap.FilterAnd:
		// Any single narrowing child is a superset of the conjunction.
		for _, child := range f.Children {
			if idx, ok := narrow(child, lookup); ok {
				return idx, true
			}
		}
		return nil, false
	case ldap.FilterOr:
		// Every child has to narrow, otherwise the union is everything.
		var set indexSet
		for _, child := range f.Children {
			idx, ok := narrow(child, lookup)
			if !ok {
				return nil, false
			}
			for _, i := range idx {
				set.add(i)
			}
		}
		return set.list, true
	}
	return nil, false
}

// userCandidates returns the users that can match a search below baseDN with
// the given filter, or false if all users are candidates.
func (s *snapshot) userCandidates(baseDN string, filter *ber.Packet, si server.LDAPServerInstance) ([]api.User, bool) {
	// A base DN below ou=users or ou=virtual-groups names exactly one user.
	if utils.HasSuffixNoCase(baseDN, ","+si.GetBaseUserDN()) || utils.HasSuffixNoCase(baseDN, ","+si.GetBaseVirtualGroupDN()) {
		if _, cn, _, ok := firstRDN(baseDN); ok {
			return s.usersAt(s.lookupUsername(cn)), true
		}
	}
	if filter == nil {
		return nil, false
	}
	idx, ok := narrow(filter, func(attr string, value string) ([]int, bool) {
		switch attr {
		case "cn", "samaccountname":
			return s.lookupUsername(value), true
		case "mail":
			return s.usersByEmail[strings.ToLower(value)], true
		case "memberof":
			_, name, ou, ok := firstRDN(value)
			if !ok {
				return nil, false
			}
			switch {
			case strings.EqualFold(ou, constants.OUGroups):
				gi, ok := s.groupsByName[strings.ToLower(name)]
				if !ok {
					return nil, true
				}
				idx := make([]int, 0, len(s.groups[gi].Users))
				for _, pk := range s.groups[gi].Users {
					if ui, ok := s.usersByPk[pk]; ok {
						idx = append(idx, ui)
					}
				}
				return idx, true
			case strings.EqualFold(ou, constants.OUVirtualGroups):
				return s.lookupUsername(name), true
			}
			return nil, false
		}
		return nil, false
	})
	if !ok {
		return nil, false
	}
	return s.usersAt(idx), true
}

// groupCandidates returns the groups that can match a search below baseDN
// with the given filter, or false if all groups are candidates.
func (s *snapshot) groupCandidates(baseDN string, filter *ber.Packet, si server.LDAPServerInstance) ([]api.Group, bool) {
	if utils.HasSuffixNoCase(baseDN, ","+si.GetBaseGroupDN()) {
		if _, cn, _, ok := firstRDN(baseDN); ok {
			return s.groupsAt(s.lookupGroupName(cn)), true
		}
	}
	if filter == nil {
		return nil, false
	}
	idx, ok := narrow(filter, func(attr string, value string) ([]int, bool) {
		switch attr {
		case "cn", "samaccountname":
			return s.lookupGroupName(value), true
		case "member":
			_, name, ou, ok := firstRDN(value)
			if !ok {
				return nil, false
			}
			switch {
			case strings.EqualFold(ou, constants.OUUsers):
				// The user's own group list is the set of groups they're a
				// member of.
				ui, ok := s.usersByUsername[strings.ToLower(name)]
				if !ok {
					return nil, true
				}
				idx := make([]int, 0, len(s.users[ui].GroupsObj))
				for _, g := range s.users[ui].GroupsObj {
					if gi, ok := s.groupsByName[strings.ToLower(g.Name)]; ok {
						idx = append(idx, gi)
					}
				}
				return idx, true
			case strings.EqualFold(ou, constants.OUGroups):
				// A child group is a member of its parents.
				gi, ok := s.groupsByName[strings.ToLower(name)]
				if !ok {
					return nil, true
				}
				idx := make([]int, 0, len(s.groups[gi].ParentsObj))
				for _, p := range s.groups[gi].ParentsObj {
					if pi, ok := s.groupsByName[strings.ToLower(p.Name)]; ok {
						idx = append(idx, pi)
					}
				}
				return idx, true
			}
			return nil, false
		}
		return nil, false
	})
	if !ok {
		return nil, false
	}
	return s.groupsAt(idx), true
}

// groupsOfUser returns the groups the user is a member of, each reduced to
// the user as its only member. This is what a user without permission to
// search the directory is allowed to see.
func (s *snapshot) groupsOfUser(pk int32) []api.Group {
	ui, ok := s.usersByPk[pk]
	if !ok {
		return nil
	}
	u := s.users[ui]
	member := api.PartialUser{Pk: u.Pk, Username: u.Username, Name: u.Name, IsActive: u.IsActive, Uid: u.Uid}
	groups := make([]api.Group, 0, len(u.GroupsObj))
	for _, g := range u.GroupsObj {
		gi, ok := s.groupsByName[strings.ToLower(g.Name)]
		if !ok {
			continue
		}
		full := s.groups[gi]
		fg := api.NewGroup(full.Pk, full.NumPk, full.Name, []api.RelatedGroup{}, []api.PartialUser{member}, []api.Role{}, nil, []string{}, []api.RelatedGroup{})
		fg.SetUsers([]int32{pk})
		fg.SetAttributes(full.Attributes)
		if full.IsSuperuser != nil {
			fg.SetIsSuperuser(*full.IsSuperuser)
		}
		groups = append(groups, *fg)
	}
	return groups
}

func (s *snapshot) lookupUsername(username string) []int {
	if i, ok := s.usersByUsername[strings.ToLower(username)]; ok {
		return []int{i}
	}
	return nil
}

func (s *snapshot) lookupGroupName(name string) []int {
	if i, ok := s.groupsByName[strings.ToLower(name)]; ok {
		return []int{i}
	}
	return nil
}

func (s *snapshot) usersAt(idx []int) []api.User {
	users := make([]api.User, 0, len(idx))
	for _, i := range idx {
		users = append(users, s.users[i])
	}
	return users
}

func (s *snapshot) groupsAt(idx []int) []api.Group {
	groups := make([]api.Group, 0, len(idx))
	for _, i := range idx {
		groups = append(groups, s.groups[i])
	}
	return groups
}
