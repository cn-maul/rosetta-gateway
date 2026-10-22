package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	wireproto "github.com/cn-maul/rosetta-gateway/internal/protocol"
)

type Config struct {
	Listen       string `json:"listen"`
	DBPath       string `json:"db_path"`
	LogLevel     string `json:"log_level"`
	MasterKeyEnv string `json:"master_key_env"`
	// PublicOrigin 是浏览器实际访问网关时看到的 scheme://host[:port]。
	//
	// 审计 A7-13 / A10-P1-3：网关自身监听明文 HTTP，通常由 TLS 反代终止
	// HTTPS。此时浏览器发出的 Origin 是 https://gw.example，而进程内
	// r.TLS 恒为 nil；只按 r.TLS 推导请求 scheme 会把**正常的管理面写请求**
	// 判成跨源（403），Cookie 也因此无法带 Secure。
	//
	// 不能改用 X-Forwarded-Proto：那是客户端可伪造的头，而 SameOrigin 同时
	// 守着免鉴权的 bootstrap 写入口 —— 采信它等于把同源校验交给攻击者。
	// 所以反代部署必须由运维显式声明这个公开源，并保证反代只接受该源的流量。
	//
	// 空串 = 未配置，保持原有行为（按进程实际观察到 r.TLS 推导，Cookie
	// 不带 Secure），适配纯 IP/HTTP 的局域网部署。
	PublicOrigin string `json:"public_origin"`
	// TrustedProxyCIDRs 是允许提供客户端转发链的可信反向代理地址段。
	//
	// 为空时完全不采信 Forwarded / X-Forwarded-For，来源 IP 始终取
	// r.RemoteAddr。配置后，只有 RemoteAddr 命中这些前缀才解析转发链；
	// 再从右向左跳过可信代理，取第一个不可信地址作为真实客户端。
	//
	// 反代必须覆盖或追加这两个头，不能把客户端自带的同名头原样透传；
	// 否则「可信代理」仍然会把攻击者伪造的链交给网关。
	TrustedProxyCIDRs []string `json:"trusted_proxy_cidrs,omitempty"`
	// RequireStrongMasterKey 让「低熵主密钥」从告警升级为拒绝启动。
	//
	// 缺省 false = 只 WARN（保持既有行为）：大量部署点确实是有意用口令
	// 形态的主密钥（比如从密码管理器里取一条），直接改成硬失败等于
	// 让这些实例在升级后起不来 —— 那是最糟糕的升级故障。
	//
	// 置 true 后，低熵（< 32 字符，见 crypto.WeakMaterial）主密钥会让进程
	// 拒绝启动。低熵密钥保护的是**全部上游 API key**，拿到数据库的人可以
	// 离线字典爆破，所以对安全要求高的部署点应该显式打开这个开关。
	RequireStrongMasterKey bool `json:"require_strong_master_key"`
	// Timezone 是按天统计的日界时区（IANA 名，如 "Asia/Shanghai"）。
	//
	// # 为什么必须有这个字段（2026-10-09 修复的线上缺陷）
	//
	// 用量按天分桶用的是 SQLite 的 strftime(..., 'localtime')（store.dayExpr），
	// 而 'localtime' 取的是**进程的本地时区**。容器里没设 TZ、镜像里也没有
	// /etc/localtime 链接时，Go 的 time.Local 解析为 UTC —— 于是北京时间
	// 0 点到 8 点的调用全部被算进「昨天」（实测：用户的 909 条清晨调用
	// 全部落进前一天的桶）。
	//
	// 修复：启动时（main 里最早期）根据这个字段改写 time.Local，
	// 'localtime' 会跟着走（modernc SQLite 的实测行为，见下方
	// ApplyTimezone 的注释）。空串 = 沿用进程现有时区（TZ 环境变量或
	// 宿主机的 /etc/localtime），不强制覆盖 —— 裸机部署在东八区机器上
	// 的用户什么都不用改。
	Timezone  string    `json:"timezone"`
	Defaults  Defaults  `json:"defaults"`
	Bootstrap Bootstrap `json:"bootstrap"`

	// UsageRetentionDays 是 usage_records 明细保留天数，超期由归档剪枝
	// （store.PruneOldUsage）聚合进 usage_daily_rollups 后删除。
	//
	// # 为什么要做成配置（2026-10-11 性能审计）
	//
	// 此前是 store.DefaultRetentionDays 常量硬编码 30。实测增长曲线
	// （0.5µs/行）显示这个值同时决定两件事，方向相反：
	//
	//   - **用户级配额预检**（store.SumUserUsedTokens）对明细做全量 SUM，
	//     代价 O(保留期内该用户的行数)。4200 req/天 × 30 天 = 12.6 万行
	//     ≈ **63ms/次**，落在每个启用用户级配额的请求上。
	//   - **写放大**：每条明细要维护 5 个二级索引 + 2 个触发器。
	//
	// 用量小的部署把 30 改成 7，配额预检降到 ~15ms，且少留 23 天明细；
	// 用量大的部署若 30 天不够（该用户累计超 150 万行 ≈ 770ms），也要能调。
	// 硬编码 30 无法同时服务这两类部署 —— 这是本次改动的全部动机。
	//
	// **不下界的原因**：1 天是合法的（只要用量不大），此时配额预检仍走
	// 实时 SUM、仍有界。上界 3650 天防止明显笔误。
	UsageRetentionDays int `json:"usage_retention_days"`

	// AuditLogKeepRows 是 audit_log 保留的最大行数（不是天数）。
	//
	// 按行数而非按天：本表的唯一读路径是 `ORDER BY id DESC LIMIT ?`，
	// 管理面永远只看最近几十条，更早的行对可观测性零贡献。此前的实现
	// 只插不删，表随写操作次数无界增长（批量导入配置 + 自动重试会显著
	// 放大写入量）。
	//
	// 缺省 20000 行 ≈ 每行 100 字节也才 2MB，而覆盖掉的问题要连续写上
	// 几个月才出现；设成 0 则**关闭剪枝**（保留全部），供合规审计要求
	// 全量留存的部署点显式声明。
	AuditLogKeepRows int `json:"audit_log_keep_rows"`

	// Probes 是上游模型定时探测的配置（2026-10-14）。
	//
	// 探测请求复用 upstream.TestUpstreamModel（max_tokens=1、不经
	// usage_recorder），**不扣任何网关用户的钱**，但确实产生上游侧计费
	// （每个模型每天约 3 token 量级）。默认开 —— 用户明确要这个功能。
	Probes ProbeConfig `json:"probes"`

	// trustedProxies 是 TrustedProxyCIDRs 校验、去重后的运行时形态。
	// 不参与 JSON 编解码；调用方通过 TrustedProxyPrefixes 取只读副本。
	trustedProxies []netip.Prefix
}

// ProbeConfig 是定时探测的参数。
//
// 所有值都必须为正（interval/timeout/concurrency/down_threshold/window_size/
// retention_days）：负值会让 time.NewTicker 当场 panic（config.go:242-253
// 有一整段既往事故记录）。healthy_rate 是 0~1 的小数。
type ProbeConfig struct {
	Enabled       bool    `json:"enabled"`
	IntervalSec   int     `json:"interval_sec"`
	TimeoutMs     int     `json:"timeout_ms"`
	Concurrency   int     `json:"concurrency"`
	DownThreshold int     `json:"down_threshold"`
	WindowSize    int     `json:"window_size"`
	HealthyRate   float64 `json:"healthy_rate"`
	RetentionDays int     `json:"retention_days"`

	// configured 记录「probes 块在配置里出现过」，不参与 JSON 编解码。
	//
	// 为什么必须显式记录而不能用 `c.Probes == (ProbeConfig{})` 判断：
	// `{"probes":{"enabled":false}}` 反序列化后**恰好就是零值结构体**，
	// 与「整块没配」不可区分，于是 setDefaults 判定为「没配过 → 默认开启」
	// 并把用户明确写下的 false 改回 true —— 之后每 10 分钟对每个上游模型
	// 发一次真实推理并向上游付费，而配置文件里明明写着关闭。
	// 布尔值天然无法区分「显式 false」与「缺省」，只能靠 key 是否出现来判断。
	configured bool
}

// UnmarshalJSON 记录 key 是否出现过。
//
// 用 map[string]json.RawMessage 只为取 presence，字段本身仍交给默认解码：
// 这样新增探针字段时不需要在这里同步维护一份键名清单（漏一个就静默失效）。
func (p *ProbeConfig) UnmarshalJSON(b []byte) error {
	type plain ProbeConfig // 无方法，避免递归
	var v plain
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	*p = ProbeConfig(v)
	p.configured = len(keys) > 0
	return nil
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
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Endpoint string `json:"endpoint"`
	// ProxyURL 是该上游的出站代理（http/https/socks5）。空 = 直连。
	//
	// 库里存的是 network_id（见 store.Provider），而 bootstrap 走配置文件 ——
	// 那里没有「先建一条网络再引用」的时机可依赖（一条配置里的多个上游
	// 可能共用同一个代理）。所以配置侧保留直填地址，启动时按 URL 自动建出
	// 网络并让这些上游引用它：配置侧保持「填一次」，运行时仍是共享清单。
	ProxyURL    string                `json:"proxy_url"`
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
// 配置里**不再有任何管理凭据字段**。身份统一由 users 表承载：
// 全新部署启动时会建出一个待初始化的 admin 账号，登录页据此显示
// 「首次设置密码」表单。会话签名密钥也不在配置里 —— 它自动生成并持久化到
// <homeDir>/session_secret，运维无需干预。
//
// 这也是默认 listen 只绑回环的原因：引导窗口对能连到端口的人开放。
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
		// 默认只监听回环。网关对外提供 /v1 是常态，但**首次启动时第一个管理员的密码还没设**，
		// 此时绑 0.0.0.0 等于把「抢先完成首次设置密码」的权利交给局域网里第一个访问者。
		// 要对外服务就显式改成 0.0.0.0:<port>（启动日志会打印实际监听地址）。
		// 端口用 8666：与 Docker 侧对齐，且避开浏览器保留端口表（6666 是 IRC 段，
		// 浏览器会直接 ERR_UNSAFE_PORT，见 ports.go 黑名单与 DOCKER.md）。
		c.Listen = "127.0.0.1:8666"
	}
	if c.DBPath == "" {
		c.DBPath = "./data/gateway.db"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	// 超时类兜底值的口径（2026-10-13 复核）：
	// 这三项原是 120s / 60s / 30s，是按「短对话、响应很快」调的；对
	// 非流式长回复与带思考的上游偏紧，于是**正常请求**被判成超时并触发
	// 故障转移（用户侧表现是无故变慢 + 结果不对，而不是明确的报错）。
	//
	//	upstream_timeout_ms 120s → 300s：非流式没有增量输出可看，只能干等；
	//	  2 分钟连 8K tokens 的回复都跑不完（按 ~40 tok/s）。5 分钟覆盖绝大多数
	//	  非流式生成；再往上会让派生的总预算（dataplane.TotalRequestBudget）
	//	  直接撞上 30 分钟上限，推导值就永远显示同一个数，失去意义。
	//	stream_first_token_timeout_ms 30s → 60s：bootstrap 里的 deepseek-reasoner
	//	  这类带思考的上游，首字本来就要几十秒。
	//	stream_idle_timeout_ms 60s → 120s：已出字后两个事件间隔超过 60s 在慢速流上
	//	  并不罕见；调大只影响「真卡死」的发现速度，不影响健康流。
	//
	// 故障转移两项维持 3/3：链上第 4 个目标几乎不提升成功率却把最坏耗时再乘一截；
	// 连续失败 3 次已足够判定目标有问题。
	//
	// config.example.json / docker 默认文件由 defaults_consistency_test.go
	// 校验，前端 RUNTIME_DEFAULTS 由 defaultsgen 直接生成。这里的 setDefaults
	// 仍是唯一的作者源：界面回显的生效值在 DB 未配置时就来自这里。
	if c.Defaults.UpstreamTimeoutMs == 0 {
		c.Defaults.UpstreamTimeoutMs = 300000
	}
	if c.Defaults.StreamIdleTimeoutMs == 0 {
		c.Defaults.StreamIdleTimeoutMs = 120000
	}
	if c.Defaults.MaxRetries == 0 {
		c.Defaults.MaxRetries = 2
	}
	if c.Defaults.MaxRequestBodyBytes == 0 {
		c.Defaults.MaxRequestBodyBytes = 32 * 1024 * 1024
	}
	if c.Defaults.StreamFirstTokenTimeoutMs == 0 {
		c.Defaults.StreamFirstTokenTimeoutMs = 60000
	}
	if c.Defaults.FailoverMaxTargets == 0 {
		c.Defaults.FailoverMaxTargets = 3
	}
	if c.Defaults.FailoverFailureThreshold == 0 {
		c.Defaults.FailoverFailureThreshold = 3
	}
	// 缺省沿用 store.DefaultRetentionDays 的值，避免两处默认值漂移。
	if c.UsageRetentionDays == 0 {
		c.UsageRetentionDays = 30
	}
	// 审计日志保留行数：0 是「没配过」的缺省形态，填成 20000。
	// 显式关闭剪枝的部署写 -1，不会被这里改回（见 validate）。
	if c.AuditLogKeepRows == 0 {
		c.AuditLogKeepRows = 20000
	}

	// 探测配置默认值。Enabled 默认 true（用户明确要这个功能）。
	// 间隔 10 分钟、超时 30s、并发 4、连续 3 次判 down、窗口 10 次、
	// 成功率 80%、保留 30 天。
	//
	// 「完全没配」与「显式 enabled=false」必须可区分（见 ProbeConfig.configured
	// 的注释）：判据是 **probes 这个 key 有没有出现过**，不是它的值。
	// 曾用 `c.Probes == (ProbeConfig{})` 判断，而 `{"probes":{"enabled":false}}`
	// 反序列化后正好等于零值结构体，于是用户明确写的 false 被静默改回 true
	// —— 之后每 10 分钟对每个模型发一次真实推理并向上游付费。
	if !c.Probes.configured {
		c.Probes.Enabled = true
	}
	if c.Probes.IntervalSec == 0 {
		c.Probes.IntervalSec = 600
	}
	if c.Probes.TimeoutMs == 0 {
		c.Probes.TimeoutMs = 30000
	}
	if c.Probes.Concurrency == 0 {
		c.Probes.Concurrency = 4
	}
	if c.Probes.DownThreshold == 0 {
		c.Probes.DownThreshold = 3
	}
	if c.Probes.WindowSize == 0 {
		c.Probes.WindowSize = 10
	}
	if c.Probes.HealthyRate == 0 {
		c.Probes.HealthyRate = 0.8
	}
	if c.Probes.RetentionDays == 0 {
		c.Probes.RetentionDays = 30
	}
}

func (c *Config) validate() error {
	if c.Listen == "" {
		return errors.New("listen address is required")
	}
	if err := c.validatePublicOrigin(); err != nil {
		return err
	}
	if err := c.validateTrustedProxyCIDRs(); err != nil {
		return err
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
	const maxDurMillis = MaxDurationMillis

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
		// 上界 512 MiB（2026-10-12 修 P2-18）：这一项此前是全表**唯一**没有
		// 上界（max=0）的字段，其余时长字段都有 maxDurMillis 兜底。
		//
		// 没上界的后果不是「配大了浪费内存」这么轻：inwire 的三条入口
		//（chat / response / anthropic）都是 io.ReadAll 全量
		// 读进内存，MaxBytesReader 是唯一的闸门。误配成 0 以外的巨大值
		//（比如手滑多敲几个 0，或把 GB 当 MB 填）后，一个带超大 body 的
		// 请求就能让单进程网关 OOM —— 而这是**局域网内任意客户端**都能触发的。
		//
		// 512 MiB 远超任何真实请求（含长上下文与多图 base64），同时把
		// 「误配成天文数字」挡在门外。要再往上放得改这个常量，属于显式决策。
		{"max_request_body_bytes", c.Defaults.MaxRequestBodyBytes, 1, 512 * 1024 * 1024},
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

	// usage_retention_days 的下界是 1 而不是 0：setDefaults 已把 0 填成 30，
	// 走到这里的 0 只能是显式写了负数被填过之后仍不合法的情况 —— 直接挡掉。
	//
	// 刻意**不设下界 7**：保留 1~6 天对低用量部署是合理诉求（配额预检
	// 降到几毫秒），把它挡在门外会让本配置失去意义。上界 3650 防笔误。
	if c.UsageRetentionDays < 1 || c.UsageRetentionDays > 3650 {
		return fmt.Errorf("usage_retention_days: must be 1..3650, got %d", c.UsageRetentionDays)
	}
	// -1 是「关闭剪枝、永久留存」的显式声明；其余非正取值都是笔误
	// （0 走不到这里，setDefaults 已填成 20000）。
	if c.AuditLogKeepRows < -1 || c.AuditLogKeepRows > 10000000 {
		return fmt.Errorf("audit_log_keep_rows: must be -1 (keep all) or 1..10000000, got %d", c.AuditLogKeepRows)
	}

	// 探测配置校验。这些值会喂给 time.NewTicker / context.WithTimeout /
	// 并发信号量 —— 负值或零会让探测循环 panic 或整轮空转。
	// 与 defaults 的区间校验同一理由（config.go:242-253 的既往事故记录）。
	if c.Probes.Enabled {
		if c.Probes.IntervalSec < 1 {
			return fmt.Errorf("probes.interval_sec: must be >= 1, got %d", c.Probes.IntervalSec)
		}
		if c.Probes.TimeoutMs < 1 {
			return fmt.Errorf("probes.timeout_ms: must be >= 1, got %d", c.Probes.TimeoutMs)
		}
		if c.Probes.Concurrency < 1 {
			return fmt.Errorf("probes.concurrency: must be >= 1, got %d", c.Probes.Concurrency)
		}
		if c.Probes.DownThreshold < 1 {
			return fmt.Errorf("probes.down_threshold: must be >= 1, got %d", c.Probes.DownThreshold)
		}
		if c.Probes.WindowSize < 1 {
			return fmt.Errorf("probes.window_size: must be >= 1, got %d", c.Probes.WindowSize)
		}
		if c.Probes.HealthyRate <= 0 || c.Probes.HealthyRate > 1 {
			return fmt.Errorf("probes.healthy_rate: must be in (0,1], got %v", c.Probes.HealthyRate)
		}
		if c.Probes.RetentionDays < 1 {
			return fmt.Errorf("probes.retention_days: must be >= 1, got %d", c.Probes.RetentionDays)
		}
	}

	// log_level 是启动日志的可见性开关。非法值会让 logger 静默退回 info
	// （改错了却看不出效果），所以在这里挡掉。
	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level: must be one of debug|info|warn|error, got %q", c.LogLevel)
	}

	// timezone 在启动早期被 ApplyTimezone 消费，但校验仍放这里：
	// Load 之后到 main 应用它之间若隔着一段「别的初始化」，一个拼错的
	// 时区名会静默落到 UTC（LoadLocation 失败的常见处理），而按天分桶
	// 会**全部**落到错误的日界 —— 与本次修复的缺陷同一个症状。
	if c.Timezone != "" {
		if _, err := time.LoadLocation(strings.TrimSpace(c.Timezone)); err != nil {
			// fixed zone 形式（"UTC+8" 这类）不是 IANA 名，LoadLocation 不认；
			// 单独支持它会让「东八区」有三种写法（Asia/Shanghai / UTC+8 / +08:00），
			// 每种的解析路径都不同。只认 IANA，一种写法。
			return fmt.Errorf("timezone: invalid IANA name %q (%v)", c.Timezone, err)
		}
	}

	for i, p := range c.Bootstrap.Providers {
		if !slugRe.MatchString(p.Slug) {
			return fmt.Errorf("bootstrap.providers[%d].slug: must match [a-z0-9]{2,32}, got %q", i, p.Slug)
		}
		if p.Endpoint == "" {
			return fmt.Errorf("bootstrap.providers[%d].endpoint: required", i)
		}
		// 与管理 API 使用同一份协议白名单：配置入口
		// 就挡住拼写错误，而不是等到运行时 buildClient 才失败（那时错误会伪装成
		// 上游连接问题，指向下游而非配置）。空串合法，表示走默认/自动探测。
		p.Protocol = wireproto.Normalize(p.Protocol)
		c.Bootstrap.Providers[i].Protocol = p.Protocol
		if p.Protocol != "" && !wireproto.Valid(p.Protocol) {
			return fmt.Errorf("bootstrap.providers[%d].protocol: must be one of %s, got %q", i, wireproto.AllowedValues, p.Protocol)
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

// validatePublicOrigin 严格校验 public_origin，并把它规范化成
// scheme://host[:port]（无尾斜杠）。
//
// 只允许 http/https、必须有 host，且不能带 userinfo/query/fragment。
// 路径只接受空串或 "/" —— public_origin 是「源」，不是 URL 前缀；允许
// /admin 之类的路径会让 sameOrigin 与 Cookie 的语义含糊，且很容易被误读
// 成部署在子路径下（网关实际并不支持）。
func (c *Config) validatePublicOrigin() error {
	raw := strings.TrimSpace(c.PublicOrigin)
	if raw == "" {
		c.PublicOrigin = ""
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("public_origin: invalid URL %q: %w", raw, err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("public_origin: scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("public_origin: host is required, got %q", raw)
	}
	if u.User != nil {
		return fmt.Errorf("public_origin: userinfo is not allowed")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("public_origin: query and fragment are not allowed")
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("public_origin: path must be empty or '/', got %q", u.Path)
	}
	c.PublicOrigin = u.Scheme + "://" + strings.ToLower(u.Host)
	return nil
}

// validateTrustedProxyCIDRs 校验并规范化可信代理网段。
//
// 只接受 IP 或 CIDR，不做 DNS 解析：启动时解析域名会让安全边界随 DNS
// 应答变化，也无法解释「配置里写的是 hostname，实际信任了哪个网段」。
// 裸 IP 归一成 /32 或 /128；带 host bits 的 CIDR 归一成网络地址。
func (c *Config) validateTrustedProxyCIDRs() error {
	c.trustedProxies = nil
	if len(c.TrustedProxyCIDRs) == 0 {
		c.TrustedProxyCIDRs = nil
		return nil
	}

	normalized := make([]string, 0, len(c.TrustedProxyCIDRs))
	seen := make(map[netip.Prefix]struct{}, len(c.TrustedProxyCIDRs))
	for i, raw := range c.TrustedProxyCIDRs {
		s := strings.TrimSpace(raw)
		if s == "" {
			return fmt.Errorf("trusted_proxy_cidrs[%d]: must not be empty", i)
		}

		var p netip.Prefix
		if strings.Contains(s, "/") {
			parsed, err := netip.ParsePrefix(s)
			if err != nil {
				return fmt.Errorf("trusted_proxy_cidrs[%d]: invalid CIDR %q: %w", i, s, err)
			}
			p = parsed
		} else {
			addr, err := netip.ParseAddr(s)
			if err != nil {
				return fmt.Errorf("trusted_proxy_cidrs[%d]: %q is neither an IP nor a CIDR", i, s)
			}
			p = netip.PrefixFrom(addr, addr.BitLen())
		}
		p = p.Masked()
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		c.trustedProxies = append(c.trustedProxies, p)
		normalized = append(normalized, p.String())
	}
	c.TrustedProxyCIDRs = normalized
	return nil
}

// TrustedProxyPrefixes 返回可信反向代理网段的只读副本。
func (c *Config) TrustedProxyPrefixes() []netip.Prefix {
	if c == nil || len(c.trustedProxies) == 0 {
		return nil
	}
	out := make([]netip.Prefix, len(c.trustedProxies))
	copy(out, c.trustedProxies)
	return out
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

// MaxDurationMillis 是「毫秒 → time.Duration」的安全上界（24h）。
//
// 导出是为了让管理面的 provider 级 timeout_ms 用**同一个**常量：
// 该字段曾经只校验负数，于是 PATCH {"timeout_ms":9223372036854} 会被照单
// 全收（实测 200），而 internal/upstream 里的
// `time.Duration(sp.TimeoutMs) * time.Millisecond` 直接回绕成负数。
// 三处校验各写一份字面量，迟早会再漏一处。
const MaxDurationMillis = 86_400_000
