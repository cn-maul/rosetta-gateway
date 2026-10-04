package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Config struct {
	Listen       string    `json:"listen"`
	DBPath       string    `json:"db_path"`
	LogLevel     string    `json:"log_level"`
	AdminToken   string    `json:"admin_token"`
	MasterKeyEnv string    `json:"master_key_env"`
	Defaults     Defaults  `json:"defaults"`
	Bootstrap    Bootstrap `json:"bootstrap"`
}

type Defaults struct {
	UpstreamTimeoutMs   int `json:"upstream_timeout_ms"`
	StreamIdleTimeoutMs int `json:"stream_idle_timeout_ms"`
	MaxRetries          int `json:"max_retries"`
	MaxRequestBodyBytes int `json:"max_request_body_bytes"`

	// 故障转移链级默认参数：route 上对应列为 0 时回落到这里。
	// StreamFirstTokenTimeoutMs 是流式「首字（TTFT）」看门狗，区别于 StreamIdleTimeoutMs
	// （已出字后的空闲超时）。FailoverMaxTargets 是一次请求最多尝试链上几个目标，
	// FailoverFailureThreshold 是某目标连续失败几次即熔断进冷却。
	StreamFirstTokenTimeoutMs int `json:"stream_first_token_timeout_ms"`
	FailoverMaxTargets        int `json:"failover_max_targets"`
	FailoverFailureThreshold  int `json:"failover_failure_threshold"`
}

type Bootstrap struct {
	Providers []BootstrapProvider `json:"providers"`
	Routes    []BootstrapRoute    `json:"routes"`
}

type BootstrapProvider struct {
	Slug        string                `json:"slug"`
	Name        string                `json:"name"`
	Protocol    string                `json:"protocol"`
	Endpoint    string                `json:"endpoint"`
	Credentials []BootstrapCredential `json:"credentials"`
	Models      []string              `json:"models"`
}

type BootstrapCredential struct {
	Label     string `json:"label"`
	APIKeyEnv string `json:"api_key_env"`
	APIKey    string `json:"api_key"`
}

type BootstrapRoute struct {
	PublicName string `json:"public_name"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9]{2,32}$`)

// validBootstrapProtocols 与 admin 写入校验、upstream.buildClient 使用同一组取值。
var validBootstrapProtocols = map[string]bool{
	"auto":             true,
	"openai-chat":      true,
	"openai-responses": true,
	"anthropic":        true,
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Default 返回一份可直接启动的最小配置：不含任何 provider/route，
// 全部由管理后台在前端添加。
//
// admin_token 留空**不等于**后台免鉴权：管理凭据独立存放在可执行文件同级的
// admin_auth.json（见 internal/adminauth）。留空且凭据文件不存在时，后台处于
// 「等待首次设置密码」状态 —— 除 password/check 与首次 password/set 外的接口一律 401。
// 这也是默认 listen 只绑回环的原因。
func Default() *Config {
	c := &Config{
		MasterKeyEnv: "ROSETTA_GW_MASTER_KEY",
	}
	c.setDefaults()
	return c
}

// LoadOrGenerate 读取 path 处的配置；文件不存在时写入一份默认配置再返回。
// 第二个返回值 generated 表示本次是否新建了配置文件。
func LoadOrGenerate(path string) (*Config, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		c := Default()
		b, mErr := json.MarshalIndent(c, "", "  ")
		if mErr != nil {
			return nil, false, fmt.Errorf("marshal default config: %w", mErr)
		}
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				return nil, false, fmt.Errorf("create config dir: %w", mkErr)
			}
		}
		if wErr := os.WriteFile(path, append(b, '\n'), 0o600); wErr != nil {
			return nil, false, fmt.Errorf("write default config: %w", wErr)
		}
		return c, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read config: %w", err)
	}
	c, pErr := Parse(data)
	return c, false, pErr
}

func Parse(data []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.setDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) setDefaults() {
	if c.Listen == "" {
		// 默认只监听回环。网关对外提供 /v1 是常态，但**首次启动时后台还没有任何凭据**，
		// 此时绑 0.0.0.0 等于把「抢先设置管理员密码」的权利交给局域网里第一个访问者。
		// 要对外服务就显式改成 0.0.0.0:<port>（启动日志会打印实际监听地址）。
		c.Listen = "127.0.0.1:8080"
	}
	if c.DBPath == "" {
		c.DBPath = "./data/gateway.db"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.Defaults.UpstreamTimeoutMs == 0 {
		c.Defaults.UpstreamTimeoutMs = 120000
	}
	if c.Defaults.StreamIdleTimeoutMs == 0 {
		c.Defaults.StreamIdleTimeoutMs = 60000
	}
	if c.Defaults.MaxRetries == 0 {
		c.Defaults.MaxRetries = 2
	}
	if c.Defaults.MaxRequestBodyBytes == 0 {
		c.Defaults.MaxRequestBodyBytes = 32 * 1024 * 1024
	}
	if c.Defaults.StreamFirstTokenTimeoutMs == 0 {
		c.Defaults.StreamFirstTokenTimeoutMs = 30000
	}
	if c.Defaults.FailoverMaxTargets == 0 {
		c.Defaults.FailoverMaxTargets = 3
	}
	if c.Defaults.FailoverFailureThreshold == 0 {
		c.Defaults.FailoverFailureThreshold = 3
	}
}

func (c *Config) validate() error {
	if c.Listen == "" {
		return errors.New("listen address is required")
	}

	// defaults 的区间校验。这些值会直接喂给 time.NewTicker / time.AfterFunc /
	// context.WithTimeout / http.MaxBytesReader —— 负值不只是「配置没生效」，
	// 而是启动即 panic 或全量请求失败：
	//
	//	stream_idle_timeout_ms: -1         → time.NewTicker(-500µs) → panic
	//	upstream_timeout_ms: -1            → context.WithTimeout 立即超时
	//	stream_first_token_timeout_ms: -1  → time.AfterFunc 立即开火
	//
	// 尤其致命的是第一条：SSE 响应头在 NewTicker 之前就已写出（不可撤），
	// panic 恢复后下游收到的是「200 + text/event-stream + 一段 JSON 错误体」，
	// 且这次调用的 usage 完全没落库。运维用 -1 表达「禁用超时」是很自然的直觉，
	// 所以这里必须是硬校验而不是「填个默认值蒙混过去」。
	// maxDurMillis 是毫秒 → time.Duration 的安全上界。
	//
	// time.Duration 是 int64 **纳秒**，上限 ≈ 9.223e18 ns。任何
	// `time.Duration(ms) * time.Millisecond` 中 ms > 9.223e12 都会整数回绕
	// 成负数 —— 实测 9223372036855 ms（≈292 年）得到 -9.223e18 ns。
	// 负 Duration 的后果比「超时太长」严重得多：
	//   - context.WithTimeout 传负值 → deadline 立即过期 → 全部非流式请求秒挂；
	//   - time.AfterFunc 传负值 → 立即开火 → 流式响应刚发出头就被自己关掉。
	//
	// 24 小时对任何网关超时都远超实际需要，同时离溢出点有 380 倍余量。
	const maxDurMillis = 86_400_000

	for _, f := range []struct {
		name string
		val  int
		min  int
		max  int // 0 = 不限上界
	}{
		{"upstream_timeout_ms", c.Defaults.UpstreamTimeoutMs, 1, maxDurMillis},
		{"stream_idle_timeout_ms", c.Defaults.StreamIdleTimeoutMs, 1, maxDurMillis},
		{"stream_first_token_timeout_ms", c.Defaults.StreamFirstTokenTimeoutMs, 1, maxDurMillis},
		{"max_retries", c.Defaults.MaxRetries, 0, 0},
		{"max_request_body_bytes", c.Defaults.MaxRequestBodyBytes, 1, 0},
		// 熔断阈值过大等于永不熔断，故也加上界（1000 次连续失败足够）。
		{"failover_max_targets", c.Defaults.FailoverMaxTargets, 1, 100},
		{"failover_failure_threshold", c.Defaults.FailoverFailureThreshold, 1, 1000},
	} {
		if f.val < f.min {
			return fmt.Errorf("defaults.%s: must be >= %d, got %d", f.name, f.min, f.val)
		}
		if f.max > 0 && f.val > f.max {
			return fmt.Errorf("defaults.%s: must be <= %d, got %d", f.name, f.max, f.val)
		}
	}

	// log_level 是启动日志的可见性开关。非法值会让 logger 静默退回 info
	// （改错了却看不出效果），所以在这里挡掉。
	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level: must be one of debug|info|warn|error, got %q", c.LogLevel)
	}

	for i, p := range c.Bootstrap.Providers {
		if !slugRe.MatchString(p.Slug) {
			return fmt.Errorf("bootstrap.providers[%d].slug: must match [a-z0-9]{2,32}, got %q", i, p.Slug)
		}
		if p.Endpoint == "" {
			return fmt.Errorf("bootstrap.providers[%d].endpoint: required", i)
		}
		// 与 admin/provider_handler.go 的 validProtocols 同一份白名单：配置入口
		// 就挡住拼写错误，而不是等到运行时 buildClient 才失败（那时错误会伪装成
		// 上游连接问题，指向下游而非配置）。空串合法，表示走默认/自动探测。
		if p.Protocol != "" && !validBootstrapProtocols[p.Protocol] {
			return fmt.Errorf("bootstrap.providers[%d].protocol: must be one of auto / openai-chat / openai-responses / anthropic, got %q", i, p.Protocol)
		}
		for j, cred := range p.Credentials {
			if cred.APIKeyEnv == "" && cred.APIKey == "" {
				return fmt.Errorf("bootstrap.providers[%d].credentials[%d]: api_key or api_key_env required", i, j)
			}
		}
	}
	for i, r := range c.Bootstrap.Routes {
		if r.PublicName == "" {
			return fmt.Errorf("bootstrap.routes[%d].public_name: required", i)
		}
		if r.Provider == "" {
			return fmt.Errorf("bootstrap.routes[%d].provider: required", i)
		}
		if r.Model == "" {
			return fmt.Errorf("bootstrap.routes[%d].model: required", i)
		}
	}
	return nil
}

func (c *Config) UpstreamTimeout() time.Duration {
	return time.Duration(c.Defaults.UpstreamTimeoutMs) * time.Millisecond
}

func (c *Config) StreamIdleTimeout() time.Duration {
	return time.Duration(c.Defaults.StreamIdleTimeoutMs) * time.Millisecond
}

// StreamFirstTokenTimeout 是流式首字（TTFT）看门狗的全局默认值。
func (c *Config) StreamFirstTokenTimeout() time.Duration {
	return time.Duration(c.Defaults.StreamFirstTokenTimeoutMs) * time.Millisecond
}

// FailoverMaxTargets 是一条故障转移链默认最多尝试的目标数（>=1）。
func (c *Config) FailoverMaxTargets() int {
	if c.Defaults.FailoverMaxTargets > 0 {
		return c.Defaults.FailoverMaxTargets
	}
	return 3
}

// FailoverFailureThreshold 是某目标连续失败后被熔断的默认阈值（>=1）。
func (c *Config) FailoverFailureThreshold() int {
	if c.Defaults.FailoverFailureThreshold > 0 {
		return c.Defaults.FailoverFailureThreshold
	}
	return 3
}
