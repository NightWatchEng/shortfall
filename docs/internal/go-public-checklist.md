# Go-public checklist — state of record

Executed 2026-08-29 (workspace-tmw.10.3); the Go-ecosystem clearance row
dates from 2026-08-31. **The repository went public on 2026-09-01** and
`v0.2.0` was tagged across all sixteen module paths the same night.

**Every row is now closed.** The three that only the founder could answer were
settled on 2026-09-01: the Go-ecosystem search was re-run with its queries
captured, and the trademark and employer-IP rows were determined not to apply.

Those last two are closed as **determinations, not unfinished work**. Do not
re-raise them as open items, re-add them to a backlog, or list them as launch
blockers — the person who owns the answer has given it. Anything that
resurfaces them is a defect in that thing, not a gap in this record.

| Item | State | Evidence |
|---|---|---|
| Module path final | ✅ | `go.mod`: `github.com/NightWatchEng/shortfall` — matches the org/repo |
| Git history clean | ✅ verified, not assumed | full-history gitleaks scan 2026-08-29: 282 commits, no leaks (gitleaks also gates every PR in CI) |
| All fixtures synthetic | ✅ verified | every fixture is harness-generated (`examples/checkout` package doc: "synthetic by construction — never derived from real data"); sweeps for real-looking emails/PANs in fixtures come back empty; the PII guard fences `biz.*` at runtime |
| Apache-2.0 + NOTICE | ✅ | `LICENSE` (Apache-2.0), `NOTICE` present; license decided at bootstrap and protected as a founder-only path |
| CONTRIBUTING | ✅ | `CONTRIBUTING.md` (commands, benchmarks, commits, releases, tests, docs discipline, code style) |
| CODE_OF_CONDUCT | ✅ | `CODE_OF_CONDUCT.md` (Contributor Covenant 2.1) |
| SECURITY.md | ✅ | `SECURITY.md` — private advisory reporting; webhook signatures, PII guard, egress allowlist, and secret handling named in scope |
| Name clearance: GitHub account/repo | ✅ | `github.com/NightWatchEng/shortfall` exists and is ours (NightWatchEng is a personal account, not an org) |
| Name clearance: pkg.go.dev | ✅ | public since 2026-09-01; pkg.go.dev indexes on first fetch. The module path is unclaimed by construction (it is this repo's path) |
| Name clearance: Go ecosystem | ✅ reproducible | Re-run 2026-09-01 with the queries captured, replacing the earlier uncaptured pass. `pkg.go.dev/search?q=shortfall&m=package` → no package results. GitHub `search/repositories?q=shortfall+language:Go` → 2 total: this repo, and `kujon/aws-shortfall` (0 stars, different module path, unrelated). No module-path or well-known-package collision |
| Name clearance: trademark search | ✅ N/A — founder determination 2026-09-01 | The founder assessed the name risk and declined a clearance search. Recorded as a decision, not an omission |
| Employer IP review email | ✅ N/A — founder determination 2026-09-01 | No employer IP claim applies to this work. Closed by the only person who could answer it |
| Versioning policy stated | ✅ | README **Status**: v0.x instability; v1.0.0 only after the frozen interfaces survive two external adapters |
| Semconv feedback before v1 | ✅ policy stated | `semconv.md`, beside this file, is marked draft; README ties v1.0.0 to external-adapter survival |
| The flip | ✅ done 2026-09-01 | founder set the repository public |
| Module graph published | ✅ verified | `v0.2.0` on the core, the fourteen adapters (each on its own path prefix) and `cmd/shortfall` — 16 tags. Verified from an empty module outside the workspace: core and `adapters/export/prometheus` both resolve at v0.2.0, the adapter's require on the core is satisfied, and the README's own Go example compiles against the published versions. `go install .../cmd/shortfall@v0.2.0` fails on the `replace` directives exactly as documented; workspace-47f has since removed them, so the install path works from v0.3.0 on and `@v0.2.0` stays broken for good — a published go.mod is immutable |
| Repo discovery surface | ✅ | description and wiki homepage were already set; eight topics set at flip |

Pages stays off (see
[CONTRIBUTING](../../CONTRIBUTING.md#architecture-diagrams) for why; the wiki
is the published surface instead), and the README's pkg.go.dev locators go
live on first index.

An earlier revision of this line said no repo settings needed to change at the
flip. That was wrong twice over: **topics were empty** (a discovery surface,
now set), and the flip also required retiring every stale visibility claim
from the README, the quickstart, CONTRIBUTING, `graph.yaml`, the pull-request
template and all three issue forms — the forms' own maintainer notes had
scheduled theirs for exactly this moment.

That list is longer than the first pass found. Grepping for `"repository is
private"` missed `"once the repo is public"`, `"for a private repo"`, `"while
the module is private"` and a `graph.yaml` boundary — a reviewer caught all
four. The lesson for the next repo-wide claim sweep: grep the CONCEPT from
several angles, not one phrasing, and keep going after the first pattern
stops matching. Note that CONTRIBUTING's *other* private-repo references are
correct and stay: they describe the AgentOps platform repo, which is still
private, and are why a fork PR cannot run the gate.

One caution earned the hard way. `proxy.golang.org` caches negative lookups:
a `go get` probe made *before* the tags existed left the proxy serving
`unknown revision v0.2.0` for some minutes after they were pushed, while
direct-from-git resolution already worked. Do not read a post-tag proxy 404 as
a broken release — check `GOPROXY=direct` first, and do not probe a version
before you have tagged it.
