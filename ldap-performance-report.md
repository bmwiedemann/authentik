# authentik LDAP outpost at 500,000 users: measurements and fixes

## Summary

Measured with 500k users, 2,020 groups and 1.7M memberships. As shipped, and in its
default configuration (`search_mode=direct`, `bind_mode=direct`), the LDAP outpost is
not usable at this scale for anything but a bind plus a `cn=`/`mail=` lookup:

| Operation, as shipped                                   | Cost                                        |
| ------------------------------------------------------- | ------------------------------------------- |
| Bind (direct)                                           | 3.5 s, 7 sequential API calls               |
| Lookup by `cn=` / `mail=` (direct)                      | 0.3–0.4 s                                   |
| Lookup by `uid=`, base-DN read, any OR, `objectClass=*` | download of all users: **days**             |
| Members of a group (`memberOf=`) (direct)               | 84 s (p95 138 s)                            |
| Group with 500k members, or `member=` touching it       | 22–24 s, outpost RSS 2.2 GB                 |
| Enumerate groups (direct)                               | 85 s                                        |
| Any lookup in `search_mode=cached`                      | 2.0–2.3 s CPU and 1.9 GB garbage, each      |
| Cached-mode full refresh (default every 5 min)          | 10,000 pages × 18–22 s ≈ **2.5 days**       |

The causes are a handful of independent problems, none intrinsic to the design. The
seven commits on branch `ldap-perf` (all low-risk, no API or schema change) bring
lookups in cached mode from seconds to microseconds, cut the server-side cost of a
users page from 4.2 s of database time to under 1 ms, and remove a bind that
**succeeds** when the access check fails. With them, `search_mode=cached` plus
`bind_mode=cached` is production-ready for lookups and binds at 500k users. What
remains is structural — page-number pagination (58 min initial load), memory (8.4 GB
outpost RSS), per-row avatar resolution, and a 210 ms brand lookup on every API call —
and needs configuration today and small core changes to fix properly.

### Before and after (end to end, p50 per operation, `ldapsearch`-equivalent client)

| Case                         | direct, as shipped         | direct + core fix        | cached + all fixes |
| ---------------------------- | -------------------------- | ------------------------ | ------------------ |
| bind, sequential             | 3.5 s                      | 2.9 s                    | 2.8 s (first), **0 ms** repeated |
| bind, 20 parallel            | 3.7/s, p95 7.3 s           | 4.7/s, p95 4.6 s         | 4.6/s, p95 5.2 s   |
| bind, wrong password         | 1.5 s                      | 1.2 s                    | 1.4 s              |
| user `(cn=x)`                | 341 ms                     | 263 ms                   | **<1 ms**          |
| user `(&(objectClass=user)(cn=x))` | 443 ms               | 263 ms                   | <1 ms              |
| user `(mail=x)`              | 403 ms                     | 329 ms                   | <1 ms              |
| user by base DN              | full download (days)       | full download            | <1 ms              |
| user `(uid=x)`               | full download              | full download            | 2.6 s (full scan)  |
| user `(\|(uid=x)(mail=x))`   | full download              | full download            | 2.8 s (full scan)  |
| group `(cn=grp-0001)` (300 members) | 273 ms              | 265 ms                   | <1 ms              |
| group `(cn=dept-007)` (2,500 members) | 387 ms            | 370 ms                   | 3 ms               |
| group `(cn=all-staff)` (500k members) | 22.7 s            | 22.2 s                   | 435 ms             |
| groups `(member=cn=x,…)`     | 24.3 s                     | 23.6 s                   | 52 ms              |
| users `(memberOf=cn=grp-0001,…)` | 83.9 s (p95 138 s)     | 2.0 s                    | 2 ms               |
| enumerate all users          | full download              | full download            | 4.9 s              |
| enumerate all groups         | 84.8 s                     | 82.7 s                   | 216 ms             |

"direct + all fixes" is identical to "direct + core fix" within noise and is omitted:
in direct mode every search is bounded by API cost, and the outpost-side fixes
change what is fetched only for the cases marked "full download" (base-DN reads are
now a single-object fetch, see finding 3; the others cannot be narrowed). The
"repeated bind" cell in the as-shipped column was invalid (bench-tool bug, fixed
before the later runs).

## Test setup

- authentik 2026.11.0-rc1 (this checkout), release build of the Rust front proxy,
  gunicorn 2 workers × 4 threads, Postgres 18 in a container, all on one host
  (Ryzen 7 9700X, 16 threads, 60 GB).
- `seed_users.py`: 500,000 users, 2,020 groups — `all-staff` (every user), 5
  `big-XX` (50k each), 200 `dept-XXX` (2,500 each, nested under 20 `division-XX`),
  1,794 `grp-XXXX` (10–500 random members); 1,712,147 memberships.
- Outpost built from source, talking to `http://localhost:9000`. Load from a small Go
  client (`bench/main.go`), matrix in `bench/run.sh`; server-side costs from
  `pg_stat_statements`, `EXPLAIN ANALYZE` and cProfile in the Django test client.
- `BenchmarkMemorySearch` (added in this branch) exercises the cached searcher
  against a fake API at 20k and 500k users.
- The cached-mode refresh was measured with `avatars: initials` and page size 100
  (finding 5 and 9); with the defaults it does not complete in a day.

## Findings

### 1. Cached search mode rebuilt the whole directory on every search — fixed

`search/memory/memory.go` turned **every** cached user and group into an
`ldap.Entry`, including all `member`/`memberOf` DN strings, before the LDAP library
applied the filter, so a base-DN read of one user cost the same as enumerating all
500k. `BenchmarkMemorySearch`, 500k users / 2,000 groups:

| Case                       | Before           | After           |
| -------------------------- | ---------------- | --------------- |
| user by base DN            | 2.05 s, 1.88 GB  | 42 µs, 10 KB    |
| `(cn=user000042)`          | 2.30 s, 1.91 GB  | 23 µs           |
| `(mail=…)`                 | 2.27 s           | 53 µs           |
| `(memberOf=cn=group…)`     | 2.28 s           | 1.4 ms          |
| group `(cn=…)`             | 87 ms, 114 MB    | 85 µs           |
| group `(member=cn=user…)`  | 94 ms            | 38 ms (a 500k-member group is among the results) |
| enumerate all users        | 2.25 s           | 2.30 s (unchanged by design) |

Fix (`00b24a0c6e`): the snapshot keeps lower-cased maps (username, email, group
name) and derives a candidate set from the base DN or from equality clauses on
`cn`, `sAMAccountName`, `mail`, `memberOf`, `member` (AND: any narrowing child;
OR: all children). The library still applies the full filter, so candidates only
need to be a superset. A user without search permission gets their groups from
their own group list instead of a scan over every group's members.

### 2. Users list: the superuser annotation cost 84 ms per row — fixed

`UserViewSet.get_queryset` annotates `_annotated_is_superuser` with an `Exists`
subquery correlated on the *group* side through two join paths joined by OR;
Postgres could not use the membership index and walked the memberships of every
superuser group and its descendants, for every row:

| Request                                  | Before   | After   |
| ---------------------------------------- | -------- | ------- |
| page of 50 users, database time          | 4,183 ms | 0.6 ms  |
| `?username=user000042`                   | 117 ms   | 44 ms   |
| `?groups_by_name=grp-0001` (page of 50)  | 33 s     | 0.6 s   |
| `?page=2000&page_size=100`               | 215 s    | 0.8 s   |

The subquery sits in the scan's target list, so it also ran for every row that
`OFFSET` skipped — hence 215 s for page 2000. Fix (`e3e0b2c7ef`): compute the set
of superuser-conferring groups once, uncorrelated, and probe the user's own
memberships against it. Existing tests pass, including
`test_filter_type_no_distinct`.

### 3. Direct mode: most filters become a full download — partly fixed

`utils/utils_user.go` translated only `=` on `cn`, `displayName`, `mail`,
`memberOf`; `name` was an empty case; names were case-sensitive. Everything else —
`uid=`, `sAMAccountName=`, `(objectClass=*)`, OR, wildcards, and a base-scoped read
of one DN — fetched all 500k users in 10,000 pages of 50. At 18–22 s per page (as
shipped) that is days; the calibration search was killed after 12 pages.

Fixed (`b020b5d1eb`, `6c35d0ab26`): case-insensitive names, `name`,
`sAMAccountName`, and base-DN reads fetch just the named user or group.
**Cannot be pushed down:** `uid` is a SHA-256 of the user's pk, not the username,
so `(uid=…)` can never be narrowed; OR and wildcards have no API equivalent. Such
clients need `search_mode=cached`, where they are a 2.6 s full scan.

### 4. Bind: 7 sequential API calls of ≥ 250 ms each

A direct bind is 5 flow-executor calls (GET, POST identification with password,
GET, POST, GET redirect), then `check_access` (policy engine, `use_cache=False`,
~730 ms here), then `users/me/` (computes groups, roles, permissions and settings;
the outpost reads only `pk`). Server-side 250–730 ms each: bind p50 2.8–3.5 s,
4–5 binds/s at 20 parallel, 1.2–1.5 s for a wrong password. Per call, ~210 ms is
the brand lookup (finding 6) and ~45 ms Django; PBKDF2 (~1M iterations) once.

Lever today: `bind_mode=cached` answers a repeated bind with the same credentials
from a TTL cache with no API call (0 ms in the table).

### 5. Users list: avatars resolved per row

A warm page of 50 users costs 0.46 s server-side, 0.24 s of it in `get_avatar`
(3 Postgres-cache round trips per user). A user not seen in 8 h additionally costs an
outbound `HEAD` to gravatar.com (~100 ms here, 5 s timeout): 500k `HEAD`s per
refresh cycle for a consumer that never reads `avatar`. `avatars: initials` (or
`none`) on the tenant removes this; skipping avatar resolution for list requests
that don't need it would be the code fix.

### 6. Every outpost API call pays ~210 ms in the brand lookup

`get_brand_for_request` runs a 20-way `LEFT JOIN` (`select_related` on all brand
flows). Postgres executes it in 0.4 ms (`pg_stat_statements`), a prepared execution
takes 0.5 ms, but an unprepared execution costs 230 ms whenever the request's `Host`
is anything but `localhost` (measured through Django, raw psycopg, simple and
extended protocol; planner settings don't change it). Outposts usually reach the
core through an internal hostname (`authentik-server:9000`), so every API call — 7
per bind, one per page — pays it. A core issue: cache the brand per host for a few
seconds, or use prepared statements for this query. Reproduction: `brand_raw.py`.

### 7. Correctness bugs that surface under load — fixed (`3023b6e152`)

- **Bind fails open.** `bind/direct/bind.go` dereferenced the `check_access`
  response before checking the error. On a failed request (timeout, 5xx) it
  panicked; `recover` in `LDAPServer.Bind` returned the zero result code, which is
  `LDAPResultSuccess`: a correct password bypassed the application's access policy
  exactly when the core is overloaded. Now `OperationsError`; tests
  `TestDirectBindAccessCheckError`, `TestBindRecoverFailsClosed`.
- `ak.Paginator` retried a failed page forever without incrementing or backing off.
  Now bounded retries with backoff, error returned with the partial result.
- The memory searcher replaced a complete snapshot with a partial one on any fetch
  error and allowed concurrent full reloads (timer, websocket update, SIGUSR1). Now
  keeps the previous snapshot and serialises reloads.
- `GroupsForUser` sized its slice by `Groups` but iterated `GroupsObj`;
  `UserEntry` mutated attribute slices shared through the snapshot; the session
  binder derived its TTL from a possibly nil cookie.

### 8. Groups embed every member; memory

`/core/groups/?include_users=true` serialises every member of every group on the
page: `all-staff` is a 22 s response; in direct mode the outpost's RSS grew to
2.2–3.0 GB from such responses. In cached mode every membership is held twice as a
full object (`GroupsObj` on the user, `UsersObj` on the group): **8.4 GB RSS**
after the load, 12.7 GB after two full enumerations (Go returns memory lazily). A
leaner representation (member pks, or a members endpoint) is an API change.

### 9. Cached-mode refresh is a full, paged re-download

With the core fix, `avatars: initials` and page size 100 (the tenant's default
`pagination_max_page_size`), the initial load of 500k users + 2,020 groups took
**58 min** (5,034 user pages, 328 ms mean database time each; `COUNT(*)` on every
page, 15 ms). The cost is dominated by `OFFSET`: page 1 takes 0.33 s, page 2500
0.65 s, page 5000 0.79 s end to end. With 1000-row pages: 1.0 s (page 1) to 1.5 s
(page 500), i.e. roughly 11 min for the same walk. The default `refresh_interval`
of 5 min is far below either.

Fetching users and groups concurrently and without roles (`7bd0f279c9`) measured
56 min and 6.1 GB RSS after the load, against 58 min and 8.4 GB: the user walk
dominates, so concurrency gains little; leaving roles out of the payload is what
saves memory. Only a different pagination strategy or a delta refresh changes the
order of magnitude.

## Verification

- `go test ./internal/outpost/...`, `go vet`, `gofmt`, `golangci-lint` (0 issues);
  Python: `ruff`, `black`, `mypy --strict` on the changed file,
  `test_users_api.py` + `test_groups_api.py` pass.
- New tests: fail-closed bind (2), paginator retry/abort (2), filter pushdown
  tables (user 16 cases, group 7), direct base-DN pushdown, and the benchmarks.
- LDIF comparison of 18 representative searches (users by `cn`/`CN`/
  `sAMAccountName`/`mail`, base-DN reads, `memberOf`, groups by `cn`/`member`,
  virtual groups, and a user without search permission) between the as-shipped
  outpost in direct mode and the fixed outpost in cached mode, normalised for
  attribute and entry order: 16 identical. The two differences are both in the
  fixed outpost's favour: `(member=<child group DN>)` now returns the parent
  group (as shipped, the request was skipped and nothing was returned; direct
  mode still behaves that way), and a non-searching user's group entries now list
  child groups as members like direct mode does (`10f146e293`).
- Direct mode after the fixes: a base-scoped read of a user DN is 256 ms and of
  a group DN 299 ms, instead of a download of the whole directory.

## Recommendations

For operators today:

1. `search_mode=cached` and `bind_mode=cached` for directories beyond a few thousand
   users; with this branch lookups are microseconds, without it seconds each.
2. `avatars: initials` (or `none`) on the tenant when outposts are the main
   consumer of the users list.
3. Raise the tenant's `pagination_max_page_size` and the outpost's
   `AUTHENTIK_LDAP__PAGE_SIZE` to 1000: refresh ~5× faster. Set the outpost
   `refresh_interval` well above the measured load time.
4. Give the outpost a memory budget of ~10 GB at 500k users, or fewer memberships.
5. Point clients at `cn` / `sAMAccountName` / `mail`, never `uid`.

For the code base, beyond this branch (each is an API or design decision):

- Keyset (cursor) pagination or a streaming export for users/groups: the page walk
  is O(n²) and issues a `COUNT(*)` per page.
- Delta refresh in the cached searcher (`last_updated` since the last fetch)
  instead of a full reload every interval.
- Member pks instead of full `PartialUser` objects in the group list, or a members
  endpoint; would also cut the outpost's memory by more than half.
- Server-side paged results (RFC 2696): the outpost advertises the control but
  ignores it, so enumerations are one huge response.
- A cheaper `users/me/` for the bind (only `pk` is used) and skipping avatar
  resolution for list requests that don't need it.
- Per-host brand cache (finding 6): −210 ms on every outpost API call.

## Files

- Branch `ldap-perf`, commits: `3023b6e152` fail-closed/retry fixes,
  `b020b5d1eb` filter pushdown, `e3e0b2c7ef` superuser annotation,
  `00b24a0c6e` indexed cached searcher, `addba84ab7` benchmarks,
  `7bd0f279c9` concurrent fetch without roles, `6c35d0ab26` base-DN pushdown,
  `10f146e293` child groups for non-searching users.
- Scratchpad: `seed_users.py`, `seed_provider.py`, `bench/` (`main.go`,
  `run.sh`, `table.py`, results `*.jsonl`), `run_cached.sh`, `ldif_diff.sh`,
  `brand_raw.py`, `gobench-500k-*.txt`.
