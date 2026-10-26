# Module Coverage

Status values: `pending`, `in_progress`, `complete`.

## M00 - Baseline, Build, CI, Release

- Status: `complete`
- Files reviewed: `go.mod`, `Dockerfile`, `.dockerignore`,
  `.gitattributes`, `.gitignore`, `.zcodeignore`, `build.ps1`,
  `gateway.ps1`, `scripts/check-repo-hygiene.ps1`,
  `.github/workflows/ci.yml`, `.github/workflows/docker.yml`,
  `web/package.json`, version/route/DTO generators.
- Verification: static repository inspection only.

## M01 - Startup and Lifecycle

- Status: `complete`
- Files reviewed: `cmd/gateway/main.go`, `internal/gatewayapp/run.go`,
  `internal/gatewayapp/serve.go`, `internal/gatewayapp/healthz.go`,
  `internal/gatewayapp/bootstrap.go`, `cmd/gateway/bootstrap_admin.go`,
  `internal/runtime/reloader.go`, `internal/probe/runner.go`,
  `internal/usage/recorder.go`, `internal/api/healthz.go`, relevant store
  close/ping/probe DAOs and lifecycle tests.
- Verification: static source and test inspection only.

## M02 - Config, Snapshot, Routing, Catalog

- Status: `complete`
- Files reviewed: `internal/config/config.go`, `internal/config/fields.go`,
  `internal/config/fields_test.go`, `internal/config/timezone.go`,
  `internal/config/ports.go`, `internal/config/generate.go`,
  `internal/config/defaultsgen/main.go`, `internal/config/config_test.go`,
  `internal/config/defaults_consistency_test.go`,
  `internal/config/trusted_proxy_test.go`,
  `internal/config/public_origin_test.go`, `internal/snapshot/snapshot.go`,
  `internal/snapshot/rebuild.go`, `internal/snapshot/rebuild_test.go`,
  `internal/snapshot/model_allow_test.go`,
  `internal/snapshot/runtime_contract_test.go`, `internal/routing/routing.go`,
  bootstrap/snapshot/lifecycle seams in `internal/gatewayapp`, and
  `internal/catalog/models.go`.
- Verification: static source and test inspection only.

## M03 - Inbound Protocols

- Status: `complete`
- Files reviewed: `internal/inwire/openai_chat.go`,
  `internal/inwire/openai_chat_test.go`,
  `internal/inwire/openai_chat_audit_test.go`,
  `internal/inwire/anthropic.go`, `internal/inwire/anthropic_test.go`,
  `internal/inwire/openai_responses.go`,
  `internal/inwire/openai_responses_test.go`,
  `internal/inwire/responses_chat_replay.go`,
  `internal/inwire/responses_chat_replay_test.go`,
  `internal/inwire/decode.go`, `internal/inwire/decode_test.go`,
  `internal/inwire/repro_reasoning_effort_test.go`,
  `internal/ingress/codec.go`, `internal/ingress/request.go`,
  `internal/ingress/estimate.go`, and the relevant `DESIGN.md` protocol
  sections.
- Verification: static source and test inspection only.

## M04 - Outbound Protocols

- Status: `complete`
- Files reviewed: `internal/outwire/sink.go`,
  `internal/outwire/openai_chat.go`, `internal/outwire/anthropic.go`,
  `internal/outwire/openai_responses.go`, `internal/outwire/errors.go`,
  `internal/outwire/write_error.go`,
  `internal/outwire/write_deadline.go`, `internal/outwire/toolindex.go`,
  all `internal/outwire/*_test.go`, the corresponding ingress codec boundary,
  and the relevant `DESIGN.md` sections for streaming, errors, and usage.
- Verification: static source and test inspection only.

## M05 - Dataplane and Failover

- Status: `complete`
- Files reviewed: `internal/dataplane/outcome.go`,
  `internal/dataplane/policy.go`, `internal/dataplane/replay.go`,
  `internal/dataplane/rate.go`, `internal/dataplane/balance.go`,
  `internal/dataplane/cache_affinity.go`, `internal/dataplane/handler.go`,
  material `internal/dataplane/*_test.go` paths,
  `cmd/gateway/failover_test.go`, `cmd/gateway/audit_regression_test.go`,
  `cmd/gateway/request_budget_test.go`, relevant structured-output tests,
  `internal/store/key_dao.go`, `internal/store/user_dao.go`,
  `internal/ratelimit/limiter.go`, and relevant usage-recorder settlement.
- Verification: static source and test inspection only.

## M06 - Upstream Pool, Probing, Networking

- Status: `complete`
- Files reviewed: `internal/upstream/upstream.go`,
  `internal/upstream/balance.go`, `internal/upstream/proxy_probe.go`,
  `internal/upstream/opencode.go`, `internal/upstream/trace.go`,
  `internal/upstream/pool_test.go`, `internal/upstream/probe_slot_test.go`,
  `internal/upstream/live_targets_test.go`,
  `internal/upstream/cooldown_race_test.go`,
  `internal/upstream/protocol_test.go`, `internal/upstream/proxy_test.go`,
  `internal/upstream/balance_test.go`, `internal/probe/runner.go`,
  `internal/probe/runner_test.go`, credential cooldown DAO paths,
  runtime reload failure projection, snapshot provider-failure projection,
  dataplane probe claim/release paths, and relevant `DESIGN.md` sections.
- Verification: static source and test inspection only.

## M07 - Storage, Schema, DAO, Transactions

- Status: `complete`
- Files reviewed: `internal/store/store.go`, `internal/store/schema.go`,
  `internal/store/tx.go`, `internal/store/reservation_recovery.go`,
  `internal/store/password_setup_dao.go`, `internal/store/user_dao.go`,
  `internal/store/key_dao.go`, `internal/store/balance_dao.go`,
  `internal/store/topup_dao.go`, `internal/store/usage_dao.go`,
  `internal/store/usage_source.go`, `internal/store/usage_archive.go`,
  `internal/store/usage_by_user_dao.go`, `internal/store/usage_contract.go`,
  `internal/store/pricing.go`, `internal/store/model_dao.go`,
  `internal/store/model_group_dao.go`, `internal/store/route_dao.go`,
  `internal/store/route_target_dao.go`, `internal/store/provider_dao.go`,
  `internal/store/credential_dao.go`, `internal/store/group_dao.go`,
  `internal/store/network_dao.go`, `internal/store/probe_dao.go`,
  `internal/store/audit_dao.go`, `internal/store/settings_dao.go`,
  `internal/store/recent_performance_dao.go`, material `internal/store/*_test.go`
  files, and the relevant `DESIGN.md` data-model and archival sections.
- Verification: static source and test inspection only.

## M08 - Money, Quota, Balance

- Status: `complete`
- Files reviewed: `internal/billing/billing.go`,
  `internal/billinghttp/billinghttp.go`,
  `internal/dataplane/balance.go`, `internal/dataplane/rate.go` and the
  admission/finalization sections of `internal/dataplane/handler.go`,
  `internal/store/balance_dao.go`, `internal/store/topup_dao.go`,
  `internal/store/usage_dao.go`, `internal/store/pricing.go`,
  `internal/store/reservation_recovery.go`, quota/balance sections of
  `internal/store/key_dao.go`, `internal/store/user_dao.go`, and
  `internal/store/schema.go`, balance/top-up sections of
  `internal/admin/user_handler.go`, `internal/admin/stats_handler.go`, and
  `internal/admin/user_admin_handler.go`, `internal/usage/recorder.go`,
  `cmd/gateway/billing.go`, `cmd/gateway/usage.go`, and relevant balance,
  pending, top-up, quota, reconciliation, pricing, and stress tests.
- Verification: static source and test inspection only.

## M09 - Usage and Audit Persistence

- Status: `complete`
- Files reviewed: `internal/usage/recorder.go`,
  `internal/store/usage_dao.go`, `internal/store/usage_source.go`,
  `internal/store/usage_archive.go`, `internal/store/usage_by_user_dao.go`,
  `internal/store/usage_contract.go`, `internal/store/audit_dao.go`,
  `internal/admin/usage_handler.go`,
  `internal/admin/usage_by_user_handler.go`,
  `internal/admin/audit_handler.go`, `internal/server/autoreload.go`,
  `internal/api/api.go`, `internal/gatewayapp/serve.go`,
  `cmd/gateway/usage.go`, usage archive/boundary/batch/cost-pending tests,
  audit DAO tests, AutoReload tests, and usage abort/retry tests.
- Verification: static source and test inspection only.

## M10 - Identity and Authorization

- Status: `complete`
- Files reviewed: `internal/access/access.go`, `internal/access/access_test.go`,
  `internal/auth/auth.go`, `internal/auth/errors.go`,
  `internal/auth/auth_test.go`, `internal/auth/adminlock_test.go`,
  `internal/authhttp/auth.go`, `internal/identity/identity.go`,
  `internal/userauth/session.go`, `internal/userauth/password.go`,
  `internal/server/user_auth.go`, `internal/server/server.go`,
  `internal/guard/throttle.go`, `internal/httpx/httpx.go`,
  `internal/admin/user_handler.go`, `internal/admin/user_admin_handler.go`,
  `internal/admin/password_setup_handler.go`, `internal/admin/helpers.go`,
  `internal/store/user_dao.go`, `internal/store/password_setup_dao.go`,
  `internal/store/schema.go`, `internal/api/api.go`, `cmd/gateway/main.go`,
  `cmd/gateway/bootstrap_admin.go`, and authentication/authorization tests
  including `internal/server/throttle_test.go`,
  `internal/server/session_throttle_concurrency_test.go`,
  `internal/server/csrf_test.go`, `internal/server/csrf_middleware_test.go`,
  `internal/server/public_origin_test.go`, `internal/server/authz_matrix_test.go`,
  `internal/admin/scope_test.go`, `internal/admin/password_setup_handler_test.go`,
  `internal/admin/bootstrap_handler_test.go`, `cmd/gateway/adminlock_test.go`,
  and `cmd/gateway/bootstrap_admin_test.go`.
- Verification: static source and test inspection only. The
  authorization-matrix test invokes a nested `go test`, but it was not
  executed during this audit.

## M11 - HTTP Middleware and Routing

- Status: `complete`
- Files reviewed: `internal/api/api.go`, `internal/api/healthz.go`,
  `internal/server/server.go`, `internal/server/autoreload.go`,
  `internal/server/key_policy.go`, `internal/server/bridge.go`,
  `internal/server/autoreload_test.go`,
  `internal/server/autoreload_retry_test.go`,
  `internal/server/public_origin_test.go`, `internal/server/csrf_test.go`,
  `internal/server/csrf_middleware_test.go`,
  `internal/server/authz_matrix_test.go`, `internal/httpx/httpx.go`,
  `cmd/gateway/routes.go`, `cmd/gateway/routes_test.go`,
  `cmd/gateway/route_contract_test.go`, `internal/admin/helpers.go`,
  `internal/admin/config_transfer_handler.go`,
  representative admin decode callers, `internal/dataplane/handler.go`,
  `internal/dataplane/diagnostics.go`, `internal/ingress/codec.go`,
  `internal/outwire/openai_chat.go`, `internal/outwire/openai_responses.go`,
  and `internal/outwire/anthropic.go`.
- Verification: static source and test inspection only. No HTTP request,
  test, race, or build command was executed.

## M12 - Admin Mutation APIs

- Status: `complete`
- Files reviewed: `internal/admin/provider_handler.go`,
  `internal/admin/network_handler.go`, `internal/admin/credential_handler.go`,
  `internal/admin/route_handler.go`, `internal/admin/model_handler.go`,
  `internal/admin/model_group_handler.go`, `internal/admin/group_handler.go`,
  `internal/admin/user_handler.go`, `internal/admin/user_admin_handler.go`,
  `internal/admin/key_handler.go`, `internal/admin/settings_handler.go`,
  `internal/admin/config_transfer_handler.go`, relevant mutation methods in
  `internal/store/provider_dao.go`, `internal/store/network_dao.go`,
  `internal/store/credential_dao.go`, `internal/store/route_dao.go`,
  `internal/store/route_target_dao.go`, `internal/store/model_dao.go`,
  `internal/store/model_group_dao.go`, `internal/store/group_dao.go`,
  `internal/store/user_dao.go`, `internal/store/key_dao.go`,
  `internal/store/settings_dao.go`, `internal/store/usage_dao.go`, and
  `internal/store/schema.go`; handler, DAO, transaction, uniqueness, and
  configuration-transfer tests covering those paths.
- Verification: static source and test inspection only. No source, database,
  test, or HTTP command was executed.

## M13 - Admin Query APIs

- Status: `complete`
- Files reviewed: `internal/admin/stats_handler.go`,
  `internal/admin/usage_handler.go`,
  `internal/admin/usage_by_user_handler.go`,
  `internal/admin/audit_handler.go`, `internal/admin/probe_handler.go`,
  `internal/admin/route_catalog_handler.go`,
  `internal/admin/route_handler.go`,
  `internal/admin/model_handler.go`, `internal/admin/helpers.go`,
  `internal/store/usage_source.go`, `internal/store/usage_dao.go`,
  `internal/store/usage_by_user_dao.go`, `internal/store/probe_dao.go`,
  `internal/store/audit_dao.go`, relevant handler/DAO/authorization tests,
  and the frontend callers in `web/src/api.ts`, `web/src/views/UsageStats.vue`,
  `web/src/views/Wallet.vue`, and `web/src/views/range.ts`.
- Verification: static source and test inspection only. No source, database,
  test, or HTTP command was executed.

## M14 - Billing and Usage Pipeline

- Status: `complete`
- Files reviewed: `internal/billing/billing.go`,
  `internal/billinghttp/billinghttp.go`, `internal/dataplane/rate.go`,
  `internal/dataplane/balance.go`, admission/finalization and pricing
  sections of `internal/dataplane/handler.go`,
  `internal/usage/recorder.go`, `internal/store/pricing.go`,
  `internal/store/usage_dao.go`, `internal/store/balance_dao.go`,
  `internal/store/topup_dao.go`, quota/usage sections of
  `internal/store/key_dao.go` and `internal/store/user_dao.go`,
  `internal/store/schema.go`, `internal/gatewayapp/serve.go`,
  `cmd/gateway/billing.go`, `cmd/gateway/balance_test.go`,
  `internal/store/balance_dao_test.go`, `internal/store/balance_settle_test.go`,
  `internal/store/cost_pending_test.go`, and the billing/quota/usage tests
  covering pricing, settlement, reconciliation, and recovery.
- Verification: static source and test inspection only.

## M15 - Frontend Foundation

- Status: `complete`
- Files reviewed: `web/src/api.ts`, `web/src/main.ts`, `web/src/router.ts`,
  `web/src/App.vue`, `web/src/types.ts`, `web/src/ui.ts`, `web/src/fmt.ts`,
  `web/src/range.ts`, `web/src/password.ts`, `web/src/protocols.ts`,
  `web/src/modelPrice.ts`, `web/src/routeTrace.ts`,
  `web/src/composables/useQueryText.ts`, `web/src/composables/useNetworks.ts`,
  `web/src/generated/routes.ts`, `web/src/generated/dto-contract.ts`,
  `web/src/generated/defaults.ts`, `web/src/views/Login.vue`,
  `web/src/views/PasswordSetup.vue`, `web/src/views/AccountSettings.vue`,
  `web/tests/apiContract.test.mjs`, `web/tests/dtoContract.test.mjs`, and
  backend login/bootstrap/session handlers used to verify the client contract.
- Verification: static source and test inspection only. No frontend build,
  typecheck, test, browser, HTTP, or backend command was executed.

## M16 - Frontend Pages

- Status: `complete`
- Files reviewed: `web/src/views/Overview.vue`,
  `web/src/views/UsageStats.vue`, `web/src/views/UsageDetail.vue`,
  `web/src/views/Wallet.vue`, `web/src/views/AccessControl.vue`,
  `web/src/views/Keys.vue`, `web/src/views/UsersGroups.vue`,
  `web/src/views/Providers.vue`, `web/src/views/ProviderDetail.vue`,
  `web/src/views/Routes.vue`, `web/src/views/Models.vue`,
  `web/src/views/Networks.vue`, `web/src/views/Settings.vue`,
  `web/src/views/ModelGroups.vue`, `web/src/views/Upstream.vue`,
  `web/src/components/CredentialPanel.vue`,
  `web/src/components/HeartbeatBar.vue`, and the API/type contracts used by
  those pages.
- Verification: static source and test inspection only. No frontend build,
  typecheck, test, browser, HTTP, Docker, database, or backend command was
  executed.

## M17 - Frontend Components

- Status: `complete`
- Files reviewed: `web/src/components/AppModal.vue`,
  `web/src/components/Tabs.vue`, `web/src/components/DataTable.vue`,
  `web/src/components/ModelPicker.vue`,
  `web/src/components/DetailDrawer.vue`,
  `web/src/components/UpstreamModelPanel.vue`,
  `web/src/components/UsageByGroup.vue`,
  `web/src/components/HeartbeatBar.vue`,
  `web/src/components/CredentialPanel.vue`, and their material callers in
  `web/src/views/AccessControl.vue`, `web/src/views/UsageStats.vue`,
  `web/src/views/Settings.vue`, `web/src/views/Upstream.vue`,
  `web/src/views/Wallet.vue`, `web/src/views/Keys.vue`,
  `web/src/views/Models.vue`, `web/src/views/ProviderDetail.vue`, and
  `web/src/views/UsageDetail.vue`.
- Verification: static source and test-discovery inspection only. No frontend
  build, typecheck, test, browser, accessibility tool, HTTP, Docker, database,
  or backend command was executed.

## M18 - WebUI Embedding, Deployment, CI

- Status: `complete`
- Files reviewed: `internal/webui/embed.go`, `internal/webui/dist/index.html`,
  `internal/webui/csp_test.go`, `internal/webui/version_test.go`,
  `internal/webui/lockfile_version_test.go`, `internal/api/api.go`,
  `internal/api/healthz.go`, `internal/server/server.go`,
  `web/sync-embed.mjs`, `web/vite.config.ts`, `web/package.json`,
  `Dockerfile`, `.dockerignore`, `docker/docker-entrypoint.sh`,
  `docker/config.default.json`, `.github/workflows/ci.yml`,
  `.github/workflows/docker.yml`, `DOCKER.md`, `README.md`, `build.ps1`,
  and `gateway.ps1`.
- Verification: static source, generated-asset, documentation, and test
  inspection only. No build, test, Docker, HTTP, database, or browser command
  was executed.

## M19 - Test Quality

- Status: `complete`
- Files reviewed: `.github/workflows/ci.yml`,
  `.github/workflows/docker.yml`, `cmd/gateway/perf_test.go`,
  `cmd/gateway/perf_leak_test.go`, `cmd/gateway/perf_batch_test.go`,
  `cmd/gateway/perf_split_test.go`, `internal/store/stress_billing_test.go`,
  `internal/server/authz_matrix_test.go`, `web/tests/apiContract.test.mjs`,
  `web/tests/dtoContract.test.mjs`, and `web/tests/routeTrace.test.mjs`.
- Verification: static workflow, test-selection, and test-source inspection
  only. No test, race, build, frontend, HTTP, Docker, database, or browser
  command was executed.

## M20 - Documentation and Final Deduplication

- Status: `complete`
- Files reviewed: `README.md`, `DESIGN.md`, `DOCKER.md`, `CONTRIBUTING.md`,
  `docker/docker-entrypoint.sh`, `internal/config/config.go`,
  `internal/config/timezone.go`, `internal/gatewayapp/run.go`,
  `internal/gatewayapp/serve.go`, and material implementation references from
  the completed module reviews.
- Verification: static documentation and implementation inspection only. No
  link checker, documentation build, process shutdown, container, test, or
  source command was executed.
