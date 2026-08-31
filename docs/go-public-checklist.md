# Go-public checklist — state of record

Executed 2026-08-29 (workspace-tmw.10.3). The repository-side items are
done with evidence; two items are personal actions only the founder can
take, and **the visibility flip itself is the founder's alone**.

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
| Name clearance: pkg.go.dev | ⏳ at flip | pkg.go.dev indexes on first public fetch; the module path is unclaimed by others by construction (it is this repo's path) |
| Name clearance: trademark search | 👤 **founder** | a real clearance search is a legal judgment; not executed by automation |
| Employer IP review email | 👤 **founder** | a personal action ("one email now beats a problem later") |
| Versioning policy stated | ✅ | README **Status**: v0.x instability; v1.0.0 only after the frozen interfaces survive two external adapters |
| Semconv feedback before v1 | ✅ policy stated | `docs/semconv.md` is marked draft; README ties v1.0.0 to external-adapter survival |
| The flip | 👤 **founder** | private → public is, and stays, the founder's call |

When flipping: no other repo settings need to change — Pages stays off
(see [CONTRIBUTING](../CONTRIBUTING.md#architecture-diagrams) for why; the
wiki is the published surface instead) — and the README's pkg.go.dev
locators become live on first index.
