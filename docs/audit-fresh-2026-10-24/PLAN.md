# Rosetta Gateway Fresh Audit Plan

## Scope

- Repository: `C:\Users\louis\Desktop\project\rosetta-gateway`
- Baseline: `e43c266c98cb8121be020fdac95dfe15fcdae2d9`
- Branch: `main`
- Mode: read-only code audit. The only writable files are under this directory.
- No sub-agents.
- Do not use `docs/audit-2026-10-20` or `docs/audit-2026-10-22.md` as evidence.
- Treat `DESIGN.md` and `README.md` as the product contract, then verify the
  implementation directly.

## Method

For every module:

1. Enumerate the production files and exported/important symbols in scope.
2. Trace callers, state transitions, and failure paths.
3. Compare the behavior with the documented contract.
4. Record only reachable findings, with `file:line` evidence and a concrete
   trigger.
5. Record open questions separately from findings.
6. Update `PROGRESS.md` before moving to the next module.

No source, configuration, generated output, test, or documentation outside
`docs/audit-fresh-2026-10-24/` may be changed.

## Module Order

| ID | Module | Main packages/files |
|---|---|---|
| M00 | Baseline, build, CI, release | repository root, `.github/`, scripts, Docker files |
| M01 | Startup and lifecycle | `cmd/gateway`, `internal/gatewayapp`, background workers |
| M02 | Config, snapshot, routing, catalog | `internal/config`, `internal/snapshot`, `internal/routing`, `internal/catalog` |
| M03 | Inbound protocols | `internal/inwire`, `internal/ingress` |
| M04 | Outbound protocols | `internal/outwire`, upstream protocol adapters |
| M05 | Dataplane and failover | `internal/dataplane` |
| M06 | Upstream pool, probing, networking | `internal/upstream`, `internal/probe` |
| M07 | Storage, schema, DAO, transactions | `internal/store` |
| M08 | Money, quota, balance | billing/quota/balance paths |
| M09 | Usage and audit persistence | `internal/usage`, audit DAO, archive paths |
| M10 | Identity and authorization | `internal/auth`, `internal/authhttp`, `internal/identity`, `internal/userauth`, `internal/access` |
| M11 | HTTP middleware and routing | `internal/api`, `internal/server`, `internal/httpx` |
| M12 | Admin mutation APIs | create/update/delete handlers |
| M13 | Admin query APIs | stats, usage, history, probe queries |
| M14 | Billing and usage pipeline | request accounting, settlement, recovery |
| M15 | Frontend foundation | `web/src` API, router, shared state, generated contracts |
| M16 | Frontend pages | `web/src/views` |
| M17 | Frontend components | `web/src/components` |
| M18 | WebUI embedding, deployment, CI | `internal/webui`, Docker, scripts |
| M19 | Test quality | production-critical test coverage and false-green risks |
| M20 | Documentation and final deduplication | contract drift, stale claims, final report |

## Per-Module Completion Criteria

- Scope file list is recorded in `MODULES.md`.
- All material entry points and failure paths are traced.
- Each finding has an ID, severity, status, evidence, trigger, impact, and
  remediation direction.
- Candidate observations without enough evidence are marked as open questions,
  not findings.
- Verification limitations are explicit.
- `PROGRESS.md` is updated.

## Severity

- `P0`: immediate loss of confidentiality, integrity, or availability in a
  normal deployment path.
- `P1`: serious production failure or security exposure with a realistic
  trigger.
- `P2`: correctness, recovery, compatibility, or operational defect with a
  bounded workaround.
- `P3`: documentation, metadata, consistency, or low-impact maintainability
  defect.
