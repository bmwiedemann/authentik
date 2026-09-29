package utils

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"beryju.io/ldap"
	"github.com/stretchr/testify/assert"
	api "goauthentik.io/packages/client-go"
)

// filterTestClient returns an API client whose requests are answered with an
// empty list, and a function returning the query of the last request made.
func filterTestClient(t *testing.T) (*api.APIClient, func() url.Values) {
	t.Helper()
	var last url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pagination":{"next":0,"previous":0,"count":0,"current":1,"total_pages":1,"start_index":0,"end_index":0},"results":[],"autocomplete":{}}`))
	}))
	t.Cleanup(srv.Close)
	akURL, _ := url.Parse(srv.URL)
	cfg := api.NewConfiguration()
	cfg.Host = akURL.Host
	cfg.Scheme = akURL.Scheme
	cfg.HTTPClient = srv.Client()
	cfg.Servers = api.ServerConfigurations{{URL: "api/v3"}}
	return api.NewAPIClient(cfg), func() url.Values { return last }
}

func TestParseFilterForUser(t *testing.T) {
	const groupDN = "cn=admins,ou=groups,dc=ldap,dc=goauthentik,dc=io"
	cases := []struct {
		filter string
		want   map[string]string
		skip   bool
	}{
		{"(cn=alice)", map[string]string{"username": "alice"}, false},
		{"(CN=alice)", map[string]string{"username": "alice"}, false},
		{"(sAMAccountName=alice)", map[string]string{"username": "alice"}, false},
		{"(name=Alice)", map[string]string{"name": "Alice"}, false},
		{"(displayName=Alice)", map[string]string{"name": "Alice"}, false},
		{"(mail=alice@example.com)", map[string]string{"email": "alice@example.com"}, false},
		{"(memberOf=" + groupDN + ")", map[string]string{"groups_by_name": "admins"}, false},
		{"(memberof=" + groupDN + ")", map[string]string{"groups_by_name": "admins"}, false},
		{"(&(objectClass=user)(cn=alice))", map[string]string{"username": "alice"}, false},
		{"(&(cn=alice)(mail=alice@example.com))", map[string]string{"username": "alice", "email": "alice@example.com"}, false},
		// uid is a hash, not the username, so it can't be narrowed
		{"(uid=alice)", map[string]string{}, false},
		// OR and substring filters can't be expressed as API filters
		{"(|(cn=alice)(mail=alice@example.com))", map[string]string{}, false},
		{"(cn=ali*)", map[string]string{}, false},
		{"(objectClass=*)", map[string]string{}, false},
		// a virtual group can't be a group filter, so the request is skipped
		{"(memberOf=cn=alice,ou=virtual-groups,dc=ldap,dc=goauthentik,dc=io)", nil, true},
		{"(memberOf=cn=alice,OU=Virtual-Groups,dc=ldap,dc=goauthentik,dc=io)", nil, true},
	}
	for _, c := range cases {
		t.Run(c.filter, func(t *testing.T) {
			client, lastQuery := filterTestClient(t)
			packet, err := ldap.CompileFilter(c.filter)
			assert.NoError(t, err)
			req, skip := ParseFilterForUser(client.CoreAPI.CoreUsersList(t.Context()), packet, false)
			assert.Equal(t, c.skip, skip)
			if c.skip {
				return
			}
			_, _, err = req.Execute()
			assert.NoError(t, err)
			q := lastQuery()
			for k, v := range c.want {
				assert.Equal(t, v, q.Get(k), k)
			}
			for _, k := range []string{"username", "name", "email", "groups_by_name"} {
				if _, ok := c.want[k]; !ok {
					assert.Empty(t, q.Get(k), k)
				}
			}
		})
	}
}

func TestParseFilterForGroup(t *testing.T) {
	const userDN = "cn=alice,ou=users,dc=ldap,dc=goauthentik,dc=io"
	cases := []struct {
		filter string
		want   map[string]string
		skip   bool
	}{
		{"(cn=admins)", map[string]string{"name": "admins"}, false},
		{"(CN=admins)", map[string]string{"name": "admins"}, false},
		{"(member=" + userDN + ")", map[string]string{"members_by_username": "alice"}, false},
		{"(MEMBER=" + userDN + ")", map[string]string{"members_by_username": "alice"}, false},
		{"(&(objectClass=group)(cn=admins))", map[string]string{"name": "admins"}, false},
		// a group's memberOf are its parent groups, which have no API filter
		{"(memberOf=cn=parent,ou=groups,dc=ldap,dc=goauthentik,dc=io)", map[string]string{}, false},
		{"(member=cn=other,ou=groups,dc=ldap,dc=goauthentik,dc=io)", nil, true},
	}
	for _, c := range cases {
		t.Run(c.filter, func(t *testing.T) {
			client, lastQuery := filterTestClient(t)
			packet, err := ldap.CompileFilter(c.filter)
			assert.NoError(t, err)
			req, skip := ParseFilterForGroup(client.CoreAPI.CoreGroupsList(t.Context()), packet, false)
			assert.Equal(t, c.skip, skip)
			if c.skip {
				return
			}
			_, _, err = req.Execute()
			assert.NoError(t, err)
			q := lastQuery()
			for k, v := range c.want {
				assert.Equal(t, v, q.Get(k), k)
			}
			for _, k := range []string{"name", "members_by_username"} {
				if _, ok := c.want[k]; !ok {
					assert.Empty(t, q.Get(k), k)
				}
			}
		})
	}
}
