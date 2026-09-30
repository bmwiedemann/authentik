# LDAP outpost performance at 500k users: measure, report, fix the easy wins

## Context
`task.txt` asks two things. How fast is authentik's LDAP outpost today with 500,000 accounts that use groups? And how can it be made meaningfully faster without intrusive changes?
- Agreed deliverable: benchmark on a local dev stack, a written report, and low-risk fixes, one commit each.
- Tools not on the host (`psql`, `uv`, `pnpm`, …) can be installed in a distrobox (`tumbleweed`), and Postgres can run through podman `scripts/compose.yml`.

### What code analysis already shows (to be confirmed with measurements)
**Architecture:**
- The Go outpost is in `internal/outpost/ldap/`. It reads the Django core only through the REST API.
- `search_mode` and `bind_mode` are each `direct` or `cached` (`authentik/providers/ldap/models.py`). Both default to `direct`.

**Bind:**
- A direct bind costs about 5–6 sequential HTTP calls:
  - flow executor GET, then a POST per stage;
  - `/outposts/ldap/{pk}/check_access/`, which runs the policy engine with `use_cache=False`;
  - `/core/users/me/`, which computes groups, roles, permissions and settings, although only `pk` is used.
- The server also checks a PBKDF2 password hash with about 1M iterations, which is CPU-bound.
- Cached bind (`bind/memory/memory.go`) removes all of that on a cache hit.

**Search, `direct` mode:**
- Only a narrow set of filters is pushed down to the API (`utils/utils_user.go`, `utils/utils_group.go`):
  - only `=` matches, or an `&` of them;
  - attribute names are case-sensitive;
  - `name` does nothing (empty case);
  - `memberOf` never matches on the group side.
- Everything else (`uid`, `objectClass`, `|`, wildcards, …) falls back to downloading the whole directory.
  - Page size is 50 (server cap 100), so 500k users means 5–10k sequential requests.
  - Each page runs an OFFSET query plus a `COUNT(*)`, plus an avatar/Gravatar lookup for every user.

**Search, `cached` mode (`search/memory/memory.go`):**
- Refresh does a full re-download every 5 minutes and twice at startup. Refreshes can run concurrently.
- Errors are ignored, so a partial snapshot silently replaces the cached one.
- Every search rebuilds an `ldap.Entry` for **all** users and groups, and all member DN strings, before the library filters linearly. Even a single-user base-scope lookup costs O(500k).

**Core API:**
- The groups list returns every member of every group, fully serialised with attributes.
- There is no index on `email` (only on `UPPER(email)`), yet the filter is an exact `email=`.

**Correctness bugs found along the way (fixed first):**
- **Fail-open bind.** At `bind/direct/bind.go:62-65`, `access.Access.Passing` is dereferenced before `err` is checked. If check_access errors, this panics. `recover()` in `bind.go` then returns `(0, nil)`, which is `LDAPResultSuccess`, so the access policy is bypassed. The same failure mode hits the nil `flags.Session` case in `bind/memory/memory.go`.
- **Infinite retry in `ak.Paginator`.** On an error for any page after the first, it loops forever with no backoff (`internal/outpost/ak/api_utils.go:58-65`).
- **Wrong slice length in `GroupsForUser`.** It sizes the slice by `len(user.Groups)` but iterates `GroupsObj` (`internal/outpost/ldap/utils.go:11`).
- **Shared cache mutated in place.** `UserEntry` edits the snapshot's attribute slices (`entries.go:21-25`), which is a data race.

## Phase 1: Test environment and seed data
1. Environment:
   - Start Postgres with `podman compose -f scripts/compose.yml up -d postgresql`.
   - In the `tumbleweed` distrobox: `make install`, `make gen-dev-config`, `make dev-reset`, `make run`.
2. Seed script at `scratchpad/seed_ldap.py`, run with `uv run python manage.py shell < …`:
   - 500k users, created with `User.objects.bulk_create` and a precomputed password hash so hashing isn't repeated;
   - about 2,000 groups with a realistic size spread (most 10–500 members, a few with 50k, one "all-staff" group with 500k);
   - some nested groups;
   - `UserGroup` rows created with `bulk_create`.
3. Create the LDAP provider, application, service account (with `search_full_directory`) and an outpost through the API or a blueprint.
   - Run the outpost from source: `go run ./cmd/ldap` with `AUTHENTIK_HOST` and `AUTHENTIK_TOKEN`.

## Phase 2: Baseline measurements
Store the scripts in `scratchpad/bench/`. Each case runs with `ldapsearch` / a small Go load tool, collecting wall time, outpost RSS and CPU, core CPU, and SQL (`pg_stat_statements`).

| Case | Modes |
|---|---|
| Bind, sequential and 20 in parallel | direct, cached |
| Single-user lookups: `(cn=x)`, `(uid=x)`, `(&(objectClass=user)(cn=x))`, `(mail=x)` | direct, cached |
| Group lookup `(cn=grp)`, `(member=cn=x,…)`, `(memberOf=cn=grp,…)` | direct, cached |
| Full enumeration `(objectClass=*)` (SSSD-style) | direct, cached |
| Cached-mode refresh: duration, peak RSS, requests, DB load | cached |

Also run Go microbenchmarks with an httptest fake API, reusing the scaffolding in `internal/outpost/ldap/search_memory_test.go`: `BenchmarkMemorySearch_*` and `BenchmarkUserEntry`.

## Phase 3: Low-risk fixes (each its own commit, each measured before and after)
Ordered by value per risk. None changes the API schema or the data model, except the optional index in step 7.

1. **Correctness and safety (Go):**
   - check `err` before `access`;
   - make bind's `recover` return `LDAPResultOperationsError`;
   - handle a nil session;
   - paginator: increment the page and add a retry cap with backoff, or abort on error;
   - memory `fetch()`: keep the old snapshot when fetching fails, and add a mutex so only one refresh runs at a time;
   - fix the `GroupsForUser` slice length;
   - copy instead of mutating snapshot attributes.
2. **Filter pushdown (Go, `utils/utils_user.go`, `utils/utils_group.go`):**
   - match attribute names case-insensitively;
   - map `uid` and `sAMAccountName` to `username`, and fix `name`;
   - ignore `objectClass` clauses inside AND when the class matches, instead of giving up;
   - push `memberOf` down for groups (the API already has `members_by_username`, and children/parents via existing query params where available).
   - The library still re-applies the full filter, so pushdown only ever narrows the result and stays safe.
3. **Memory searcher indexing (`search/memory/memory.go`):**
   - build the snapshot once with maps (lowercased username → user, email → user, group name → group) and precomputed DN strings;
   - for base-scope DN lookups and simple equality filters on indexed attributes, build entries only for the candidates;
   - otherwise fall back to the current full scan.
4. **Fetch efficiency:**
   - raise the outpost page size to the server maximum;
   - fetch users and groups concurrently;
   - pass `include_roles=false`, since the outpost doesn't use roles (check against `entries.go` before relying on this).
5. **Core API (Python, small):**
   - Skip avatar computation for outpost/service-account list requests, or cache it better. Needs a design decision, see Phase 4 in the report.
   - Skip `COUNT(*)` on pages after the first? Probably too intrusive; document it instead.
6. **Bind:** replace `/core/users/me/` with data the flow already returns, or with `users/{pk}` without the expensive fields, if the pk is available from the flow/JWT. Also document that `bind_mode=cached` is the main lever.
7. **Optional index:** add a plain `email` btree index on User (a new migration), only if the benchmarks show a sequential scan matters.

Each fix needs a unit test:
- Go table tests for `ParseFilterForUser` / `ParseFilterForGroup`;
- a test that the bind fails closed when check_access returns 500;
- a paginator error test;
- memory-searcher index tests.

## Phase 4: Report
Write `scratchpad/ldap-performance-report.md` (offered as an Artifact to share). It covers:
- the setup;
- baseline numbers against numbers after the fixes;
- a ranked bottleneck list;
- recommended configuration: `cached` search and bind, page size, refresh interval, `pagination_max_page_size`;
- larger, *intrusive* options that are out of scope:
  - keyset pagination / a dedicated bulk-export endpoint;
  - delta sync;
  - server-side LDAP paged-results support;
  - streaming group members instead of embedding them.
- the security bug (fail-open bind), clearly highlighted.

Per `AGENTS.md`, no PRs or issues. Commits go on a branch only if the user asks.

## Verification
- `make go-test`, plus the new Go unit tests and benchmarks (`go test -bench . ./internal/outpost/ldap/...`).
- `make test authentik/providers/ldap authentik/core` for any Python change. Run `make gen` only if a serializer or param changes (not expected).
- `make lint` (golangci-lint, ruff, mypy).
- Re-run the Phase 2 benchmark suite against the seeded 500k stack and compare it with the baseline.
- E2E sanity: `ldapsearch` binds and searches return the same entries before and after the change (diff the LDIF output for a set of representative filters).
