# authentik LDAP outpost at 500,000 users: measurements and fixes

*Revised 2026-10-02. The first version's end-to-end numbers were inflated by a
~208 ms stall in the benchmark's container networking (see "Measurement
artifact" below). All end-to-end numbers here are from a clean re-run; the
microbenchmarks and database-side timings were not affected and are unchanged.*

## Summary

Measured with 500k users, 2,020 groups and 1.7M memberships. As shipped, and in its
default configuration (`search_mode=direct`, `bind_mode=direct`), the LDAP outpost
handles binds and simple `cn=`/`mail=` lookups acceptably, but anything involving
group membership, enumeration or a filter it can't translate is unusable:

| Operation, as shipped                                   | Cost                                         |
| ------------------------------------------------------- | -------------------------------------------- |
| Bind (direct)                                           | 0.7 s, 8 sequential API calls                |
| Lookup by `cn=` / `mail=` (direct)                      | 0.1–0.16 s                                   |
| Lookup by `uid=`, base-DN read, any OR, `objectClass=*` | download of all users: **~12 h**             |
| Members of a group (`memberOf=`) (direct)               | 60 s (p95 73 s)                              |
| Group with 500k members, or `member=` touching it       | 22–23 s, outpost RSS 2–3 GB                  |
| Enumerate groups (direct)                               | 73 s                                         |
| Any lookup in `search_mode=cached`                      | 2.0–2.3 s CPU and 1.9 GB garbage, each       |
| Cached-mode full refresh (default every 5 min)          | 10,000 pages × ~4 s database time ≈ **12 h** |

The "12 h" figures are extrapolated from the per-page database time (4.2 s for a page
of 50 users as shipped, see finding 2), not run end to end.

The causes are a handful of independent problems, none intrinsic to the design. The
eight commits on branch `ldap-perf` (all low-risk, no API or schema change) bring
lookups in cached mode from seconds to microseconds, cut the database cost of a
users page from 4.2 s to under 1 ms, and remove a bind that **succeeds** when the
access check fails. With them, `search_mode=cached` plus `bind_mode=cached` is
production-ready for lookups and binds at 500k users. What remains is structural —
page-number pagination (38 min initial load), memory (5.7 GB outpost RSS after the
load), per-row avatar resolution — and needs configuration today and core changes
to fix properly.

### Before and after (end to end, p50 per operation)

| Case                                  | as shipped, direct | patched, direct | patched, cached   |
| ------------------------------------- | ------------------ | --------------- | ----------------- |
| bind, sequential                      | 0.71 s             | 1.05 s          | 1.37 s first; **0 ms** repeated |
| bind, 20 parallel                     | 5.2/s, p95 4.0 s   | 5.5/s, p95 4.0 s | 4.8/s, p95 5.0 s |
| bind, same user repeatedly            | 0.95 s             | 0.95 s          | **0 ms**          |
| bind, wrong password                  | 0.70 s             | 0.70 s          | 0.79 s            |
| user `(cn=x)`                         | 108 ms             | 49 ms           | **<1 ms**         |
| user `(&(objectClass=user)(cn=x))`    | 110 ms             | 50 ms           | <1 ms             |
| user `(mail=x)`                       | 159 ms             | 108 ms          | <1 ms             |
| user by base DN                       | full download      | 47 ms           | <1 ms             |
| group by base DN (`cn=grp-0001,…`)    | full download      | 56 ms           | <1 ms             |
| user `(uid=x)`                        | full download      | full download   | 2.7 s (full scan) |
| user `(\|(uid=x)(mail=x))`            | full download      | full download   | 2.9 s (full scan) |
| group `(cn=grp-0001)` (300 members)   | 53 ms              | 51 ms           | <1 ms             |
| group `(cn=dept-007)` (2,500 members) | 154 ms             | 153 ms          | 2 ms              |
| group `(cn=all-staff)` (500k members) | 22.3 s             | 21.2 s          | 426 ms            |
| groups `(member=cn=x,…)`              | 23.2 s             | 22.9 s          | 62 ms             |
| users `(memberOf=cn=grp-0001,…)`      | 59.6 s (p95 73 s)  | 1.1 s (p95 15 s) | 1 ms             |
| enumerate all users                   | full download      | full download   | 4.7 s             |
| enumerate all groups                  | 73.3 s             | 72.3 s          | 222 ms            |

"full download" cases were skipped in direct mode (they don't finish). The
patched base-DN reads were measured separately after the matrix (same server,
provider switched to direct mode; the server log confirms a single
`username=` / `name=` request each). The
sequential bind spread (0.71–1.37 s) is run-to-run noise rather than an effect of
the patches: the bind path is identical in all three columns except for the
fail-closed error check, and the server-side step times (finding 4) are the same
within 1%. The `memberOf` p95 in the patched direct column is the first, cold
request.

## Test setup

- authentik 2026.11.0-rc1 (`main` at `ddf1c0c557` vs. branch `ldap-perf` at
  `10f146e293`), release build of the Rust front proxy, gunicorn 2 workers ×
  4 threads, Postgres 18 in a rootless podman container, all on one host
  (Ryzen 7 9700X, 16 threads, 60 GB).
- `seed_users.py`: 500,000 users, 2,020 groups — `all-staff` (every user), 5
  `big-XX` (50k each), 200 `dept-XXX` (2,500 each, nested under 20 `division-XX`),
  1,794 `grp-XXXX` (10–500 random members); 1,712,147 memberships.
- Outpost built from source, talking to `http://localhost:9000`. Load from a small Go
  client (`bench/main.go`), matrix in `bench/run.sh`, whole before/after run in
  `rebench.sh`; server-side costs from the request log, `pg_stat_statements`,
  `EXPLAIN ANALYZE` and cProfile.
- `BenchmarkMemorySearch` (added in this branch) exercises the cached searcher
  against a fake API at 20k and 500k users.
- Cached mode was measured with `avatars: initials` and page size 100 (findings 5
  and 9); direct mode with the defaults.

### Measurement artifact (found and fixed for the re-run)

The first run showed ~208 ms on many API calls. Its cause was rootless podman's
port forwarder (`pasta`), not authentik: when one read fills ≥ 90% of pasta's
forwarding pipe, it tells the kernel more data follows, and if the message ends
there the last packet is held back ~200 ms. This machine's pasta had 8 KiB pipes
(the per-user pipe-buffer budget was used up by older containers), so every
database message of 7.4–8 KiB stalled, including the brand-lookup query. After
stopping the old containers and restarting Postgres the stall is gone (verified
with a size sweep). A fix for pasta is in `~/git/passt` (branch
`splice-more-flush`, commit `e385821`); the patch mail for passt-dev is
`passt-mail/0001-tcp_splice-Push-data-held-back-by-SPLICE_F_MORE-once.patch` in the
second session's scratchpad. Anyone benchmarking through rootless podman port
forwards should check for this before trusting round-trip times.

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

| Request (database execution time)        | Before   | After   |
| ---------------------------------------- | -------- | ------- |
| page of 50 users                         | 4,183 ms | 0.6 ms  |
| `?groups_by_name=grp-0001` (page of 50)  | ~33 s    | ~0.6 s  |
| `?page=2000&page_size=100`               | ~215 s   | ~0.8 s  |

The subquery sits in the scan's target list, so it also ran for every row that
`OFFSET` skipped — hence minutes for deep pages. This is what makes the as-shipped
`memberOf` search take 60 s and the full refresh ~12 h. Fix (`e3e0b2c7ef`):
compute the set of superuser-conferring groups once, uncorrelated, and probe the
user's own memberships against it. Existing tests pass, including
`test_filter_type_no_distinct`.

### 3. Direct mode: most filters become a full download — partly fixed

`utils/utils_user.go` translated only `=` on `cn`, `displayName`, `mail`,
`memberOf`; `name` was an empty case; names were case-sensitive. Everything else —
`uid=`, `sAMAccountName=`, `(objectClass=*)`, OR, wildcards, and a base-scoped read
of one DN — fetched all 500k users in 10,000 pages of 50.

Fixed (`b020b5d1eb`, `6c35d0ab26`): case-insensitive names, `name`,
`sAMAccountName`, and base-DN reads fetch just the named user or group.
**Cannot be pushed down:** `uid` is a SHA-256 of the user's pk, not the username,
so `(uid=…)` can never be narrowed; OR and wildcards have no API equivalent. Such
clients need `search_mode=cached`, where they are a ~2.7 s full scan.

### 4. Bind: 8 sequential API calls, ~0.9 s server time

Server-side time per step, median over all binds of the run (sequential and
20-parallel, so slightly above a quiet single bind), identical before and after
the patches:

| # | Request                                              | Server time |
| - | ---------------------------------------------------- | ----------- |
| 1 | `GET` flow executor — start the flow, plan it         | ~390 ms     |
| 2 | `POST` identification (username)                      | ~40 ms      |
| 3 | `GET` next challenge                                   | ~55 ms      |
| 4 | `POST` password (PBKDF2, ~1M iterations)               | ~170 ms     |
| 5 | `GET` redirect                                         | ~55 ms      |
| 6 | `GET` final step (login)                               | ~95 ms      |
| 7 | `GET` `check_access` (policy engine, uncached)         | ~45 ms      |
| 8 | `GET` `users/me/` (outpost uses only `pk`)             | ~50 ms      |
|   | **total**                                              | **~0.9 s**  |

Starting the flow is the largest single step. The password hash is the second;
it's inherent to verifying the password. A wrong password stops after step 4
(0.7 s). `bind_mode=cached` answers a repeated bind with the same credentials
from a TTL cache with no API call (0 ms). Not profiled further yet: why step 1
costs ~390 ms. That is the next place to look for a faster first bind.

### 5. Users list: avatars resolved per row

A warm page of 50 users costs 0.46 s server-side, 0.24 s of it in `get_avatar`
(3 Postgres-cache round trips per user). A user not seen in 8 h additionally costs an
outbound `HEAD` to gravatar.com (~100 ms here, 5 s timeout): 500k `HEAD`s per
refresh cycle for a consumer that never reads `avatar`. `avatars: initials` (or
`none`) on the tenant removes this; skipping avatar resolution for list requests
that don't need it would be the code fix.

### 6. Brand lookup: ~17 ms per request (previously misreported as ~210 ms)

`get_brand_for_request` runs a 20-way `LEFT JOIN` (`select_related` on all brand
flows): ~16 ms planning plus 0.4 ms execution, on every request — ~0.13 s per bind.
The ~210 ms in the first version of this report was the pasta artifact above.
Worth fixing but minor: cache the brand per host for a few seconds, or load its
flows lazily (~1 ms).

### 7. Correctness bugs that surface under load — fixed (`3023b6e152`, `10f146e293`)

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
  binder derived its TTL from a possibly nil cookie; a non-searching user's group
  entries in cached mode lacked child groups.

### 8. Groups embed every member; memory

`/core/groups/?include_users=true` serialises every member of every group on the
page: `all-staff` is a 21–22 s response; in direct mode the outpost's RSS grew to
2–3 GB from such responses. In cached mode every membership is held twice as a
full object (`GroupsObj` on the user, `UsersObj` on the group): **5.7 GB RSS**
after the load, 14.3 GB after the benchmark matrix with two full enumerations (Go
returns memory lazily). A leaner representation (member pks, or a members
endpoint) is an API change.

### 9. Cached-mode refresh is a full, paged re-download

With the fixes, `avatars: initials` and page size 100 (the tenant's default
`pagination_max_page_size`), the initial load of 500k users + 2,020 groups took
**38 min**. The cost is dominated by `OFFSET`: a page of 100 takes 0.09 s at
page 1, 0.49 s at page 2500 and 1.23 s at page 5000, so the walk is O(n²). Larger
pages help proportionally (fewer pages, same per-row cost). The default
`refresh_interval` of 5 min is far below the load time. Only a different
pagination strategy or a delta refresh changes the order of magnitude.

## Verification

- `go test ./internal/outpost/...`, `go vet`, `gofmt`, `golangci-lint` (0 issues);
  Python: `ruff`, `black`, `mypy --strict` on the changed file,
  `test_users_api.py` + `test_groups_api.py` pass.
- New tests: fail-closed bind (2), paginator retry/abort (2), filter pushdown
  tables (user 16 cases, group 7), direct base-DN pushdown, child groups for
  non-searching users, and the benchmarks.
- LDIF comparison of 18 representative searches between the as-shipped outpost in
  direct mode and the patched outpost in cached mode, normalised for attribute and
  entry order: 16 identical. Both differences are in the patched outpost's favour:
  `(member=<child group DN>)` now returns the parent group (as shipped, nothing),
  and a non-searching user's group entries now list child groups like direct mode
  does.
- Direct mode after the fixes: a base-scoped read of a user or group DN is a single
  API fetch (47 / 56 ms) instead of a download of the whole directory.

## Recommendations

For operators today:

1. `search_mode=cached` and `bind_mode=cached` for directories beyond a few thousand
   users; with this branch lookups are microseconds, without it seconds each.
2. `avatars: initials` (or `none`) on the tenant when outposts are the main
   consumer of the users list.
3. Raise the tenant's `pagination_max_page_size` and the outpost's
   `AUTHENTIK_LDAP__PAGE_SIZE` (e.g. 1000), and set the outpost `refresh_interval`
   well above the measured load time.
4. Budget ~6–15 GB of memory for the outpost at 500k users.
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
- Profile the flow start (bind step 1, ~390 ms); drop the `users/me/` call from the
  bind (only `pk` is used); skip avatar resolution for list requests that don't
  need it; cache the brand per host.

## Files

- Branch `ldap-perf`, commits: `3023b6e152` fail-closed/retry fixes,
  `b020b5d1eb` filter pushdown, `e3e0b2c7ef` superuser annotation,
  `00b24a0c6e` indexed cached searcher, `addba84ab7` benchmarks,
  `7bd0f279c9` concurrent fetch without roles, `6c35d0ab26` base-DN pushdown,
  `10f146e293` child groups for non-searching users.
- Scratchpads: `seed_users.py`, `seed_provider.py`, `bench/` (`main.go`,
  `run.sh`, `table.py`), `ldif_diff.sh`, `gobench-500k-*.txt` (first session);
  `rebench.sh`, `rebench.log`, `bench/results-{main-direct,perf-direct,perf-cached}.jsonl`,
  `server-{main,perf}.log`, `passt-mail/` (second session).
