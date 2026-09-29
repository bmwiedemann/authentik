package ldap

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"beryju.io/ldap"
	"github.com/stretchr/testify/assert"
	"goauthentik.io/internal/outpost/ldap/flags"
	"goauthentik.io/internal/outpost/ldap/search"
	"goauthentik.io/internal/outpost/ldap/search/direct"
	api "goauthentik.io/packages/client-go"
)

// directNewAPIClient serves an empty directory and records the query of every
// users and groups list request.
func directNewAPIClient(t *testing.T) (*api.APIClient, func() map[string][]url.Values) {
	t.Helper()
	var mu sync.Mutex
	queries := map[string][]url.Values{}
	record := func(kind string, w http.ResponseWriter, r *http.Request, body any) {
		mu.Lock()
		queries[kind] = append(queries[kind], r.URL.Query())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/core/users/", func(w http.ResponseWriter, r *http.Request) {
		record("users", w, r, api.PaginatedUserList{Pagination: memNoMorePages(0), Results: []api.User{}, Autocomplete: map[string]interface{}{}})
	})
	mux.HandleFunc("/api/v3/core/groups/", func(w http.ResponseWriter, r *http.Request) {
		record("groups", w, r, api.PaginatedGroupList{Pagination: memNoMorePages(0), Results: []api.Group{}, Autocomplete: map[string]interface{}{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	akURL, _ := url.Parse(srv.URL)
	cfg := api.NewConfiguration()
	cfg.Host = akURL.Host
	cfg.Scheme = akURL.Scheme
	cfg.HTTPClient = srv.Client()
	cfg.Servers = api.ServerConfigurations{{URL: "api/v3"}}
	return api.NewAPIClient(cfg), func() map[string][]url.Values {
		mu.Lock()
		defer mu.Unlock()
		return queries
	}
}

func directSearch(t *testing.T, searcher *direct.DirectSearcher, baseDN string, scope int, filter string) {
	t.Helper()
	client, server := net.Pipe()
	defer func() {
		_ = client.Close()
		_ = server.Close()
	}()
	req, span := search.NewRequest("cn=ldapsearch,"+memTestUserDN, ldap.SearchRequest{BaseDN: baseDN, Scope: scope, Filter: filter}, client)
	defer span.Finish()
	_, err := searcher.Search(req)
	assert.NoError(t, err)
}

// TestDirectSearcherBaseDNPushdown pins that reading a single user or group
// by its DN fetches only that object instead of the whole directory.
func TestDirectSearcherBaseDNPushdown(t *testing.T) {
	client, queries := directNewAPIClient(t)
	pi := memProviderInstance(client)
	pi.SetFlags("cn=ldapsearch,"+memTestUserDN, &flags.UserFlags{UserPk: 1, CanSearch: true})
	searcher := direct.NewDirectSearcher(pi)

	directSearch(t, searcher, "cn=Alice,"+memTestUserDN, ldap.ScopeBaseObject, "(objectClass=*)")
	q := queries()
	if assert.Len(t, q["users"], 1) {
		assert.Equal(t, "Alice", q["users"][0].Get("username"))
	}
	assert.Empty(t, q["groups"])

	directSearch(t, searcher, "cn=admins,"+memTestGroupDN, ldap.ScopeBaseObject, "(objectClass=*)")
	q = queries()
	if assert.Len(t, q["groups"], 1) {
		assert.Equal(t, "admins", q["groups"][0].Get("name"))
	}

	// A subtree search from ou=users with a filter that can't be narrowed
	// still fetches everything.
	directSearch(t, searcher, memTestUserDN, ldap.ScopeWholeSubtree, "(uid=x)")
	q = queries()
	if assert.Len(t, q["users"], 2) {
		assert.Empty(t, q["users"][1].Get("username"))
	}
}
