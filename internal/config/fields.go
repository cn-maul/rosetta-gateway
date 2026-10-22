package config

// configFieldSpec is the hand-maintained inventory for every JSON-visible
// field rooted at Config. config_fields_test.go compares it with reflection so
// adding, deleting, renaming, or retagging a field requires an explicit update
// here.
//
// ReadBy entries use "<repo-relative path>:<symbol>". They are pointers to the
// production read sites, not a second source of truth for runtime behavior.
// Frontend records the user-visible entry point, or "none" for startup-only
// configuration.
type configFieldSpec struct {
	Path     string
	ReadBy   []string
	Frontend string
}

const (
	frontendNone            = "none"
	frontendSettingsRuntime = "Settings > runtime defaults"
	frontendSettingsUsage   = "Settings > usage archive"
	frontendProvider        = "Providers > effective timeout/retry"
	frontendModelsHealth    = "Models > health"
)

var configFieldInventory = []configFieldSpec{
	{
		Path:     "listen",
		ReadBy:   []string{"internal/gatewayapp/run.go:Run", "internal/gatewayapp/serve.go:Serve"},
		Frontend: frontendNone,
	},
	{
		Path:     "db_path",
		ReadBy:   []string{"internal/gatewayapp/run.go:Run"},
		Frontend: frontendNone,
	},
	{
		Path:     "log_level",
		ReadBy:   []string{"internal/gatewayapp/run.go:Run"},
		Frontend: frontendNone,
	},
	{
		Path:     "master_key_env",
		ReadBy:   []string{"internal/gatewayapp/run.go:Run"},
		Frontend: frontendNone,
	},
	{
		Path:     "public_origin",
		ReadBy:   []string{"internal/api/api.go:BuildApp"},
		Frontend: frontendNone,
	},
	{
		Path:     "trusted_proxy_cidrs",
		ReadBy:   []string{"internal/config/config.go:TrustedProxyPrefixes", "internal/api/api.go:BuildApp"},
		Frontend: frontendNone,
	},
	{
		Path:     "require_strong_master_key",
		ReadBy:   []string{"internal/gatewayapp/run.go:Run"},
		Frontend: frontendNone,
	},
	{
		Path:     "require_tls_for_public_listen",
		ReadBy:   []string{"internal/gatewayapp/run.go:checkTLSContract"},
		Frontend: frontendNone,
	},
	{
		Path:     "timezone",
		ReadBy:   []string{"internal/gatewayapp/run.go:Run"},
		Frontend: frontendNone,
	},
	{
		Path:     "defaults",
		ReadBy:   []string{"internal/dataplane/policy.go:NonStreamTimeout", "internal/admin/settings_handler.go:Get"},
		Frontend: frontendSettingsRuntime,
	},
	{
		Path:     "bootstrap",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB", "internal/upstream/upstream.go:BuildFromConfig"},
		Frontend: frontendNone,
	},
	{
		Path:     "usage_retention_days",
		ReadBy:   []string{"internal/gatewayapp/serve.go:startPruner", "internal/admin/usage_handler.go:Prune"},
		Frontend: frontendSettingsUsage,
	},
	{
		Path:     "audit_log_keep_rows",
		ReadBy:   []string{"internal/gatewayapp/serve.go:startPruner"},
		Frontend: frontendNone,
	},
	{
		Path:     "probes",
		ReadBy:   []string{"internal/gatewayapp/serve.go:startProbe", "internal/probe/runner.go:Run", "internal/admin/probe_handler.go:Timeline"},
		Frontend: frontendModelsHealth,
	},
	{
		Path:     "defaults.upstream_timeout_ms",
		ReadBy:   []string{"internal/dataplane/policy.go:NonStreamTimeout", "internal/admin/settings_handler.go:Get", "internal/admin/provider_handler.go:Create"},
		Frontend: frontendSettingsRuntime,
	},
	{
		Path:     "defaults.stream_idle_timeout_ms",
		ReadBy:   []string{"internal/dataplane/policy.go:StreamIdleTimeout", "internal/admin/settings_handler.go:Get"},
		Frontend: frontendSettingsRuntime,
	},
	{
		Path:     "defaults.max_retries",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB", "internal/upstream/upstream.go:BuildFromConfig", "internal/admin/provider_handler.go:Create"},
		Frontend: frontendProvider,
	},
	{
		Path:     "defaults.max_request_body_bytes",
		ReadBy:   []string{"internal/api/api.go:BuildApp", "internal/dataplane/handler.go:Handle"},
		Frontend: frontendNone,
	},
	{
		Path:     "defaults.stream_first_token_timeout_ms",
		ReadBy:   []string{"internal/dataplane/policy.go:FirstTokenTimeout", "internal/admin/settings_handler.go:Get"},
		Frontend: frontendSettingsRuntime,
	},
	{
		Path:     "defaults.failover_max_targets",
		ReadBy:   []string{"internal/dataplane/policy.go:FailoverMaxTargets", "internal/admin/settings_handler.go:Get"},
		Frontend: frontendSettingsRuntime,
	},
	{
		Path:     "defaults.failover_failure_threshold",
		ReadBy:   []string{"internal/dataplane/policy.go:FailoverFailureThreshold", "internal/admin/settings_handler.go:Get"},
		Frontend: frontendSettingsRuntime,
	},
	{
		Path:     "defaults.max_concurrent_streams",
		ReadBy:   []string{"internal/dataplane/handler.go:NewHandler"},
		Frontend: frontendNone,
	},
	{
		Path:     "defaults.stream_admission_timeout_ms",
		ReadBy:   []string{"internal/dataplane/handler.go:NewHandler"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.providers",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB", "internal/upstream/upstream.go:BuildFromConfig"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.routes",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.providers[].slug",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.providers[].name",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.providers[].protocol",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB", "internal/upstream/upstream.go:BuildFromConfig"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.providers[].endpoint",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB", "internal/upstream/upstream.go:BuildFromConfig"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.providers[].proxy_url",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.providers[].credentials",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.providers[].models",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.providers[].credentials[].label",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.providers[].credentials[].api_key_env",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.providers[].credentials[].api_key",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.routes[].public_name",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.routes[].provider",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB"},
		Frontend: frontendNone,
	},
	{
		Path:     "bootstrap.routes[].model",
		ReadBy:   []string{"internal/gatewayapp/bootstrap.go:bootstrapDB"},
		Frontend: frontendNone,
	},
	{
		Path:     "probes.enabled",
		ReadBy:   []string{"internal/gatewayapp/serve.go:startProbe"},
		Frontend: frontendModelsHealth,
	},
	{
		Path:     "probes.interval_sec",
		ReadBy:   []string{"internal/probe/runner.go:Run", "internal/admin/probe_handler.go:Timeline"},
		Frontend: frontendModelsHealth,
	},
	{
		Path:     "probes.timeout_ms",
		ReadBy:   []string{"internal/probe/runner.go:Run"},
		Frontend: frontendModelsHealth,
	},
	{
		Path:     "probes.concurrency",
		ReadBy:   []string{"internal/probe/runner.go:Run"},
		Frontend: frontendModelsHealth,
	},
	{
		Path:     "probes.down_threshold",
		ReadBy:   []string{"internal/admin/probe_handler.go:classify"},
		Frontend: frontendModelsHealth,
	},
	{
		Path:     "probes.window_size",
		ReadBy:   []string{"internal/probe/runner.go:Run", "internal/admin/probe_handler.go:Timeline"},
		Frontend: frontendModelsHealth,
	},
	{
		Path:     "probes.healthy_rate",
		ReadBy:   []string{"internal/admin/probe_handler.go:classify"},
		Frontend: frontendModelsHealth,
	},
	{
		Path:     "probes.retention_days",
		ReadBy:   []string{"internal/probe/runner.go:Run"},
		Frontend: frontendModelsHealth,
	},
}
