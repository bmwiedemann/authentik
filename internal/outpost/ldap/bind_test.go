package ldap

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"beryju.io/ldap"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"goauthentik.io/internal/outpost/ak"
	"goauthentik.io/internal/outpost/ldap/bind"
	"goauthentik.io/internal/outpost/ldap/bind/direct"
	"goauthentik.io/internal/outpost/ldap/flags"
	api "goauthentik.io/packages/client-go"
)

// bindNewAPIClient fakes just enough of the authentik API for a bind: the
// flow executor immediately answers with a redirect (a passed flow) and the
// access check answers with the given status code.
func bindNewAPIClient(t *testing.T, accessCheckStatus int) (*api.APIClient, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/flows/executor/test-flow/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"component": "xak-flow-redirect",
			"to":        "/",
		})
	})
	mux.HandleFunc("/api/v3/outposts/ldap/1/check_access/", func(w http.ResponseWriter, r *http.Request) {
		if accessCheckStatus != http.StatusOK {
			w.WriteHeader(accessCheckStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"has_search_permission": true,
			"access":                map[string]any{"passing": true, "messages": []string{}, "log_messages": []any{}},
		})
	})
	mux.HandleFunc("/api/v3/core/users/me/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user": map[string]any{
				"pk": 1, "username": "alice", "name": "Alice", "is_active": true, "is_superuser": false,
				"is_current": true, "groups": []any{}, "roles": []any{}, "avatar": "", "uid": "alice-uid",
				"settings": map[string]any{}, "type": "internal", "system_permissions": []string{},
			},
			"users": []any{},
		})
	})
	srv := httptest.NewServer(mux)

	akURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("failed to parse test server url: %v", err)
	}
	cfg := api.NewConfiguration()
	cfg.Host = akURL.Host
	cfg.Scheme = akURL.Scheme
	cfg.HTTPClient = srv.Client()
	cfg.Servers = api.ServerConfigurations{{URL: "api/v3"}}
	return api.NewAPIClient(cfg), srv.Close
}

func bindProviderInstance(client *api.APIClient) *ProviderInstance {
	pi := memProviderInstance(client)
	pi.authenticationFlowSlug = "test-flow"
	pi.providerPk = 1
	pi.appSlug = "test-app"
	return pi
}

func bindDo(t *testing.T, binder bind.Binder, dn string) (ldap.LDAPResultCode, error) {
	t.Helper()
	client, server := net.Pipe()
	defer func() {
		_ = client.Close()
		_ = server.Close()
	}()
	req, span := bind.NewRequest(ldap.BindRequest{BindDN: dn, Password: "password"}, client)
	defer span.Finish()
	username, err := binder.GetUsername(req.BindDN)
	if err != nil {
		t.Fatalf("failed to get username: %v", err)
	}
	return binder.Bind(username, req)
}

// TestDirectBindAccessCheckError pins that an error from the access check
// rejects the bind. It used to dereference the nil response, panic, and be
// turned into a successful bind by the recover in LDAPServer.Bind.
func TestDirectBindAccessCheckError(t *testing.T) {
	client, closeServer := bindNewAPIClient(t, http.StatusInternalServerError)
	defer closeServer()
	pi := bindProviderInstance(client)

	code, err := bindDo(t, direct.NewDirectBinder(pi), memTestAliceDN)
	assert.NoError(t, err)
	assert.Equal(t, ldap.LDAPResultCode(ldap.LDAPResultOperationsError), code)
	assert.Nil(t, pi.GetFlags(memTestAliceDN).Session)
	assert.False(t, pi.GetFlags(memTestAliceDN).CanSearch)
}

func TestDirectBindAccessCheckPassing(t *testing.T) {
	client, closeServer := bindNewAPIClient(t, http.StatusOK)
	defer closeServer()
	pi := bindProviderInstance(client)

	code, err := bindDo(t, direct.NewDirectBinder(pi), memTestAliceDN)
	assert.NoError(t, err)
	assert.Equal(t, ldap.LDAPResultCode(ldap.LDAPResultSuccess), code)
	assert.True(t, pi.GetFlags(memTestAliceDN).CanSearch)
	assert.Equal(t, int32(1), pi.GetFlags(memTestAliceDN).UserPk)
}

type panickingBinder struct{}

func (panickingBinder) GetUsername(dn string) (string, error) { return dn, nil }
func (panickingBinder) Bind(username string, req *bind.Request) (ldap.LDAPResultCode, error) {
	// Dereference a nil pointer, like the access-check bug did.
	var f *flags.UserFlags
	if f.Session == nil {
		return ldap.LDAPResultInvalidCredentials, nil
	}
	return ldap.LDAPResultSuccess, nil
}
func (panickingBinder) Unbind(username string, req *bind.Request) (ldap.LDAPResultCode, error) {
	return ldap.LDAPResultSuccess, nil
}
func (panickingBinder) TimerFlowCacheExpiry(context.Context) {}

// TestBindRecoverFailsClosed pins that a panic inside a binder is reported as
// an error instead of the zero result code, which is LDAPResultSuccess.
func TestBindRecoverFailsClosed(t *testing.T) {
	ls := &LDAPServer{
		ac:          &ak.APIController{},
		log:         log.WithField("logger", "authentik.outpost.ldap.test"),
		connections: map[string]net.Conn{},
	}
	ls.providers = []*ProviderInstance{{
		BaseDN:          memTestBaseDN,
		UserDN:          memTestUserDN,
		binder:          panickingBinder{},
		s:               ls,
		log:             ls.log,
		boundUsersMutex: &sync.RWMutex{},
		boundUsers:      map[string]*flags.UserFlags{},
	}}
	client, server := net.Pipe()
	defer func() {
		_ = client.Close()
		_ = server.Close()
	}()
	code, err := ls.Bind(ldap.BindRequest{BindDN: memTestAliceDN, Password: "password"}, client)
	assert.Error(t, err)
	assert.Equal(t, ldap.LDAPResultCode(ldap.LDAPResultOperationsError), code)
}
