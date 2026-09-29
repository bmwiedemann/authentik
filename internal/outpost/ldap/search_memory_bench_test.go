package ldap

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"beryju.io/ldap"
	"goauthentik.io/internal/outpost/ldap/flags"
	"goauthentik.io/internal/outpost/ldap/search"
	"goauthentik.io/internal/outpost/ldap/search/memory"
	api "goauthentik.io/packages/client-go"
)

// Directory size for the memory-searcher benchmarks. Override with
// AK_LDAP_BENCH_USERS / AK_LDAP_BENCH_GROUPS to try a production-sized
// directory, for example 500000 / 2000.
func benchSize() (int, int) {
	users, groups := 20000, 200
	if v, err := strconv.Atoi(os.Getenv("AK_LDAP_BENCH_USERS")); err == nil && v > 0 {
		users = v
	}
	if v, err := strconv.Atoi(os.Getenv("AK_LDAP_BENCH_GROUPS")); err == nil && v > 0 {
		groups = v
	}
	return users, groups
}

// benchDirectory builds nUsers users and nGroups groups. Group 0 contains
// every user; every other group contains the users whose index is congruent
// to it modulo nGroups-1, so each user is a member of exactly two groups.
func benchDirectory(nUsers, nGroups int) *memDirectory {
	ts := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	isActive := true
	isSuperuser := false

	groupPks := make([]string, nGroups)
	partials := make([]api.PartialGroup, nGroups)
	members := make([][]api.PartialUser, nGroups)
	memberPks := make([][]int32, nGroups)
	for g := 0; g < nGroups; g++ {
		groupPks[g] = fmt.Sprintf("00000000-0000-0000-0000-%012d", g)
		partials[g] = memPartialGroup(groupPks[g], int32(10+g), fmt.Sprintf("group-%06d", g))
	}

	users := make([]api.User, nUsers)
	for i := 0; i < nUsers; i++ {
		username := fmt.Sprintf("user%06d", i)
		email := username + "@example.com"
		second := 1 + i%(nGroups-1)
		pu := api.PartialUser{Pk: int32(i + 1), Username: username, Name: "User " + username, IsActive: &isActive, Uid: username + "-uid"}
		members[0] = append(members[0], pu)
		memberPks[0] = append(memberPks[0], pu.Pk)
		members[second] = append(members[second], pu)
		memberPks[second] = append(memberPks[second], pu.Pk)
		users[i] = api.User{
			Pk:                 int32(i + 1),
			Username:           username,
			Name:               "User " + username,
			IsActive:           &isActive,
			DateJoined:         ts,
			Groups:             []string{groupPks[0], groupPks[second]},
			GroupsObj:          []api.PartialGroup{partials[0], partials[second]},
			RolesObj:           []api.Role{},
			Email:              &email,
			Uid:                username + "-uid",
			Uuid:               fmt.Sprintf("00000000-0000-0000-0001-%012d", i),
			PasswordChangeDate: ts,
			LastUpdated:        ts,
			Attributes:         map[string]any{"employeeId": i, "department": fmt.Sprintf("dept-%03d", second)},
		}
	}

	groups := make([]api.Group, nGroups)
	for g := 0; g < nGroups; g++ {
		groups[g] = api.Group{
			Pk:                groupPks[g],
			NumPk:             int32(10 + g),
			Name:              fmt.Sprintf("group-%06d", g),
			IsSuperuser:       &isSuperuser,
			ParentsObj:        []api.RelatedGroup{},
			Users:             memberPks[g],
			UsersObj:          members[g],
			RolesObj:          []api.Role{},
			InheritedRolesObj: []api.Role{},
			Children:          []string{},
			ChildrenObj:       []api.RelatedGroup{},
		}
	}
	dir := &memDirectory{}
	dir.set(users, groups)
	return dir
}

type benchCase struct {
	name   string
	bindDN string
	base   string
	scope  int
	filter string
	// wantEntries is checked once before timing so a broken searcher does
	// not produce a misleadingly fast benchmark.
	wantEntries int
}

func benchSearcher(b *testing.B) (*memory.MemorySearcher, *ProviderInstance, int, int) {
	b.Helper()
	nUsers, nGroups := benchSize()
	dir := benchDirectory(nUsers, nGroups)
	client, closeServer := memNewAPIClient(b, dir)
	b.Cleanup(closeServer)
	pi := memProviderInstance(client)
	searchDN := "cn=ldapsearch," + memTestUserDN
	pi.SetFlags(searchDN, &flags.UserFlags{UserPk: 1, CanSearch: true})
	selfDN := "cn=user000042," + memTestUserDN
	pi.SetFlags(selfDN, &flags.UserFlags{UserPk: 43, CanSearch: false})
	return memory.NewMemorySearcher(pi, nil), pi, nUsers, nGroups
}

func runBenchSearch(b *testing.B, searcher *memory.MemorySearcher, c benchCase) {
	b.Helper()
	client, server := net.Pipe()
	defer func() {
		_ = client.Close()
		_ = server.Close()
	}()
	do := func() int {
		req, span := search.NewRequest(c.bindDN, ldap.SearchRequest{
			BaseDN: c.base,
			Scope:  c.scope,
			Filter: c.filter,
		}, client)
		defer span.Finish()
		res, err := searcher.Search(req)
		if err != nil {
			b.Fatalf("search failed: %v", err)
		}
		// The searcher returns candidates; the LDAP server library applies the
		// filter afterwards. Include that pass so the number reflects what a
		// client actually waits for.
		filter, err := ldap.CompileFilter(c.filter)
		if err != nil {
			b.Fatalf("compile filter: %v", err)
		}
		matched := 0
		for _, e := range res.Entries {
			ok, _ := ldap.ServerApplyFilter(filter, e)
			if ok {
				matched++
			}
		}
		return matched
	}
	if got := do(); got != c.wantEntries {
		b.Fatalf("%s: expected %d matching entries, got %d", c.name, c.wantEntries, got)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		do()
	}
}

func BenchmarkMemorySearch(b *testing.B) {
	searcher, _, nUsers, nGroups := benchSearcher(b)
	searchDN := "cn=ldapsearch," + memTestUserDN
	selfDN := "cn=user000042," + memTestUserDN
	cases := []benchCase{
		{"user-basedn", searchDN, selfDN, ldap.ScopeBaseObject, "(objectClass=*)", 1},
		{"user-cn", searchDN, memTestUserDN, ldap.ScopeWholeSubtree, "(cn=user000042)", 1},
		{"user-mail", searchDN, memTestUserDN, ldap.ScopeWholeSubtree, "(mail=user000042@example.com)", 1},
		{"user-memberof", searchDN, memTestUserDN, ldap.ScopeWholeSubtree, "(memberOf=cn=group-000007," + memTestGroupDN + ")", (nUsers + nGroups - 2 - 6) / (nGroups - 1)},
		{"group-cn", searchDN, memTestGroupDN, ldap.ScopeWholeSubtree, "(cn=group-000007)", 1},
		{"group-member", searchDN, memTestGroupDN, ldap.ScopeWholeSubtree, "(member=" + selfDN + ")", 2},
		{"self-basedn-nosearch", selfDN, selfDN, ldap.ScopeBaseObject, "(objectClass=*)", 1},
		{"enum-users", searchDN, memTestUserDN, ldap.ScopeWholeSubtree, "(objectClass=user)", nUsers},
		{"enum-groups", searchDN, memTestGroupDN, ldap.ScopeWholeSubtree, "(objectClass=group)", nGroups},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			runBenchSearch(b, searcher, c)
		})
	}
}
