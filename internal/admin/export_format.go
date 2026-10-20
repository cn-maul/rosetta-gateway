package admin

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// 供应商 + 模型 的导入导出格式，以及口令包裹层。
//
// ## 为什么导出必须加密
//
// 导出体**总是**包含上游凭据 —— 它是这份数据里唯一「泄了就能花钱」的东西。
// 导出文件最常见的去向是微信、邮件、网盘、Git 仓库 —— 任何一个都比数据库
// 更容易扩散，而数据库有 master.key 保护、导出文件没有。所以这里把**整个
// 文件**用口令加密，让「拿到文件」不等于「拿到凭据」。
//
// 「带不带凭据」与「明文还是密文」曾经是两个独立选项（2026-10-07 收敛）：
// 实际使用里明文模式没有想象中安全 —— 文件已经在自己磁盘上不假，但它
// 一旦被顺手转发/上传，凭据就裸奔了；且两种产物格式让导入侧要兼容两套
// 形态、用户要在界面上理解两个维度的组合。现在只有一种产物：**必带凭据
// 的 .json.enc**，导入侧对无口令的旧明文文件仍兼容。
//
// 加密层刻意复用 PBKDF2 + AES-256-GCM（与登录口令哈希同一族）：
// 口令由人手输，没有密钥分发问题，也没有「密钥跟文件走一起」的自欺欺人。

// exportFormatVersion 是当前导出体使用的版本。
//
// v2（2026-10-14）：新增 model_groups 切片与 modelExport.group_name。
// 若加这些字段却不升版本，**老版本网关会成功导入并静默丢掉全部分组**
// —— 正是下面的版本校验要拦的那类失败。
//
// v3（2026-10-20）：新增 networks 切片与 providerExport.network_name。
// 代理地址从「每个上游各存一份 URL」改成「按名字引用一份共享清单」，
// 不升版本的话老网关会把 network_name 读成空 —— 导入成功、代理静默失效。
const exportFormatVersion = 3

// exportFormatMinVersion 是仍接受导入的最老版本。
//
// # 为什么是「区间」而不是「== 当前版本」
//
// 校验的不对称在于：新代码读旧文件是安全的（v1 是 v2 的严格子集，缺的字段
// 解出零值 = 没有分组 = 与 v1 的行为逐字一致），旧代码读新文件才危险。
// 所以：
//   - 低于 min → 拒（这个版本的本代码压根没见过）；
//   - 高于当前 → **必须拒**：那是未来/更新的字段布局，本代码把 v2 的字段
//     当 v1 读会把东西读成垃圾或静默丢失（用户 NAS 上的 v1.5.3 读到 v2
//     文件就会走到这条，明确报错胜过丢分组）。
//   - 区间内（含 v1）→ 接受。v1 备份能照常导入本版本，这正是自用部署最
//     需要的向后兼容 —— 严格 == 会让升级后所有旧备份一夜之间导不进去。
const exportFormatMinVersion = 1

// configExport 是供应商 + 模型的导出体。
//
// 注意这里**不含** provider 的 id：id 是本库的内部主键，跨库无意义。
// 导入时按slug 匹配现有provider —— slug 才是对外稳定标识（路由、
// 日志、界面都引用它）。
type configExport struct {
	Version    int   `json:"version"`
	ExportedAt int64 `json:"exported_at"`
	// Encrypted 为真表示整个文件被口令加密，Body 是 base64 密文而非明文 JSON。
	Encrypted bool   `json:"encrypted,omitempty"`
	Body      string `json:"body,omitempty"`
	// KDF / Cipher 声明这份密文用的算法。与 Salt/Iterations 一样只能明文
	// 存放（解密方要靠它们派生密钥），同样不依赖保密 —— 篡改它们只会导致
	// 派生错误、认证失败。
	KDF        string `json:"kdf,omitempty"`
	Cipher     string `json:"cipher,omitempty"`
	Salt       string `json:"salt,omitempty"`
	Iterations int    `json:"iterations,omitempty"`
	// 明文模式下的三组数据；加密时它们在 Body 里，这里全空。
	Providers   []providerExport   `json:"providers,omitempty"`
	Credentials []credentialExport `json:"credentials,omitempty"`
	Models      []modelExport      `json:"models,omitempty"`
	// ModelGroups 是模型归组（v2）。组内成员通过 modelExport.group_name
	// 按显示名关联 —— 跨库靠名字对齐，与 provider 靠 slug 对齐同理。
	ModelGroups []modelGroupExport `json:"model_groups,omitempty"`
	// Networks 是出站网络清单（v3）。成员通过 providerExport.network_name
	// 按**网络名**关联：跨库 id 无意义（与 provider 靠 slug、模型组靠显示名
	// 同一理由），而名字本来就是管理员给它的。
	Networks []networkExport `json:"networks,omitempty"`
}

// networkExport 是一条出站网络（代理配置）。只带名字与地址。
type networkExport struct {
	Name     string `json:"name"`
	ProxyURL string `json:"proxy_url"`
}

// modelGroupExport 是一个模型归组。只带显示名：组的 id 是本库内部主键，
// 跨库无意义（同 provider 不带 id 的理由）。
type modelGroupExport struct {
	DisplayName string `json:"display_name"`
}

type providerExport struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	Endpoint   string `json:"endpoint"`
	Enabled    bool   `json:"enabled"`
	TimeoutMs  int    `json:"timeout_ms"`
	MaxRetries int    `json:"max_retries"`
	QuirksJSON string `json:"quirks_json,omitempty"`
	// 出站网络。v3 起按**名字**引用 networks 切片（跨库 id 无意义），
	// 空串 = 直连。字段名与之前不同是有意的：老版本网关读 v3 文件会拒
	// （版本号已升），而 v1/v2 文件缺该字段 → 空串 → 直连，向后兼容。
	NetworkName string `json:"network_name,omitempty"`
	// ProxyURL 是 v1/v2 的出站代理字段（每个上游各存一份 URL）。v3 起只在
	// **导入**侧读取：老文件里它非空时按地址建一条网络并引用，导出的新文件
	// 不再写这个字段（否则同一份代理会在文件里出现两次，且两者可能不一致）。
	//
	// 保留它不是为了兼容新写新读，而是 v1/v2 备份仍必须能导入 —— 那是自用
	// 部署最需要的向后兼容（见 exportFormatMinVersion）。
	ProxyURL string `json:"proxy_url,omitempty"`
}

type credentialExport struct {
	// ProviderSlug 而非 id：同上，跨库要靠 slug 关联。
	ProviderSlug string `json:"provider_slug"`
	Label        string `json:"label"`
	APIKey       string `json:"api_key"`
	// 指针而非 bool：导入侧要区分「文件显式写了 false」与「字段缺失」。
	// 缺失（旧版导出体 / 手写文件）按 true 处理 —— 与该字段加入前的导入
	// 行为一致；若用裸 bool，旧文件会把原本启用的凭据静默导成禁用。
	Enabled *bool `json:"enabled,omitempty"`
	Weight  int   `json:"weight"`
}

type modelExport struct {
	ProviderSlug     string `json:"provider_slug"`
	ModelID          string `json:"model_id"`
	DisplayName      string `json:"display_name,omitempty"`
	Enabled          bool   `json:"enabled"`
	ContextWindow    int    `json:"context_window,omitempty"`
	MaxOutputTokens  int    `json:"max_output_tokens,omitempty"`
	SupportsThinking *bool  `json:"supports_thinking,omitempty"`
	// EffortLevels 是逗号分隔的挡位原文（与库里那列同形），空串 = 未配置。
	EffortLevels  string  `json:"effort_levels,omitempty"`
	PriceInput    float64 `json:"price_input,omitempty"`
	PriceCacheHit float64 `json:"price_cache_hit,omitempty"`
	PriceOutput   float64 `json:"price_output,omitempty"`
	// GroupName 是该模型所属归组的显示名（v2）。空 = 未归组。
	// v1 文件缺这个字段 → 解码得空串 → 未归组，向后兼容。
	GroupName string `json:"group_name,omitempty"`
}

// 算法标识写进文件，是为了让「用错算法」变成一句明确的错误而不是
// 一次看不懂的认证失败。将来若换算法（如 argon2id），老文件仍能按它自己
// 声明的算法解开，而不会被新代码用错误的参数硬解。
const (
	exportKDFAlgo    = "pbkdf2-hmac-sha256"
	exportCipherAlgo = "aes-256-gcm"
	// 210000 次与 admin_auth.json 齐平（OWASP 对 PBKDF2-SHA256 的建议）。
	// 计算在服务端，几十毫秒而已 —— 与登录口令同一档是因为它们面临同一种
	// 离线爆破。
	exportKDFIterations = 210000
)

// sealExport 用口令加密整个导出体。
//
// 口令不足 8 位直接拒绝：PBKDF2 再慢也挡不住弱口令，而这里的数据值得那点门槛。
func sealExport(body configExport, passphrase string) (configExport, error) {
	if len([]rune(strings.TrimSpace(passphrase))) < 8 {
		return configExport{}, errors.New("加密口令至少 8 位")
	}
	// 加密的是**数据部分**，不含 Salt/Iterations 自身 ——
	// 那两个字段要在外层供解密方读取。
	inner := configExport{
		Version:     exportFormatVersion,
		ExportedAt:  body.ExportedAt,
		Providers:   body.Providers,
		Credentials: body.Credentials,
		Models:      body.Models,
		ModelGroups: body.ModelGroups,
		Networks:    body.Networks,
	}
	raw, err := json.Marshal(inner)
	if err != nil {
		return configExport{}, err
	}

	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return configExport{}, err
	}
	key, err := deriveExportKey(passphrase, salt, exportKDFIterations)
	if err != nil {
		return configExport{}, err
	}

	// nonce 前置，与 internal/crypto.Encrypt 一致
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return configExport{}, err
	}
	ct, err := gcmSeal(key, nonce, raw)
	if err != nil {
		return configExport{}, err
	}

	return configExport{
		Version:    exportFormatVersion,
		ExportedAt: body.ExportedAt,
		Encrypted:  true,
		Body:       base64.StdEncoding.EncodeToString(append(nonce, ct...)),
		KDF:        exportKDFAlgo,
		Cipher:     exportCipherAlgo,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Iterations: exportKDFIterations,
	}, nil
}

// openExport 解开导出体。口令错、文件被改、格式不认识，分别给出能让人
// 判断下一步的错误，而不是一律"failed to decrypt"。
func openExport(in configExport, passphrase string) (*configExport, error) {
	if !in.Encrypted {
		if err := checkExportVersion(in.Version); err != nil {
			return nil, err
		}
		return &in, nil
	}
	if strings.TrimSpace(passphrase) == "" {
		return nil, errors.New("该导出文件已加密，需要口令才能导入")
	}
	// 算法必须核对而不是默认套用。文件由别人生成，声明的算法可能是本版
	// 根本不支持的 —— 那时正确的反应是说"看不懂"，而不是拿错算法硬解一
	// 次再报"口令错误"，那会把人引向反复重试口令。
	if in.KDF != exportKDFAlgo {
		return nil, fmt.Errorf("该导出文件使用了不支持的密钥派生算法 %q（本版本只支持 %q）",
			in.KDF, exportKDFAlgo)
	}
	if in.Cipher != exportCipherAlgo {
		return nil, fmt.Errorf("该导出文件使用了不支持的加密算法 %q（本版本只支持 %q）",
			in.Cipher, exportCipherAlgo)
	}

	blob, err := base64.StdEncoding.DecodeString(in.Body)
	if err != nil {
		return nil, errors.New("导出文件已损坏：body 不是合法 base64")
	}
	if len(blob) < 12+16 {
		return nil, errors.New("导出文件已损坏：密文长度不足")
	}
	nonce, ct := blob[:12], blob[12:]

	salt, err := base64.StdEncoding.DecodeString(in.Salt)
	if err != nil || len(salt) == 0 {
		return nil, errors.New("导出文件已损坏：salt 字段不合法")
	}
	iter := in.Iterations
	if iter <= 0 {
		iter = exportKDFIterations
	}
	key, err := deriveExportKey(passphrase, salt, iter)
	if err != nil {
		return nil, errors.New("导出文件已损坏：无法派生密钥")
	}

	plain, err := gcmOpen(key, nonce, ct)
	if err != nil {
		// GCM 认证失败 = 口令错 或密文被改。这两者无法区分，也不该区分：
		// 告诉攻击者「口令对了只是密文坏了」没有意义。
		// 注意 salt/iterations 被篡改也落到这里 —— 派不出正确密钥就是
		// 认证失败，绕不过去。
		return nil, errors.New("口令不正确，或文件已被修改")
	}
	var out configExport
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, errors.New("解密成功但内容不是合法导出格式")
	}
	if err := checkExportVersion(out.Version); err != nil {
		return nil, err
	}
	return &out, nil
}

// checkExportVersion 判定一个版本号能否被本版本导入（明文与加密两条路径共用）。
//
// 语义是区间 [min, current]：v1 备份（NAS 自用部署的历史导出）必须能继续
// 导入 v2，而更新的版本本代码读不懂、必须拒 —— 理由见 exportFormatMinVersion。
func checkExportVersion(v int) error {
	if v < exportFormatMinVersion || v > exportFormatVersion {
		return fmt.Errorf("不支持的导出格式版本 %d（本版本接受 %d–%d）",
			v, exportFormatMinVersion, exportFormatVersion)
	}
	return nil
}

// deriveExportKey 从口令派生 32 字节密钥。
func deriveExportKey(passphrase string, salt []byte, iter int) ([]byte, error) {
	if iter < 1000 {
		// 太小的迭代数会让离线爆破变得廉价。低于这个值直接拒绝，
		// 而不是"尊重文件里的参数" —— 参数由文件携带，也就由攻击者控制。
		return nil, errors.New("迭代数过低")
	}
	return pbkdf2.Key(sha256.New, passphrase, salt, iter, 32)
}

// gcmSeal / gcmOpen 是 AES-256-GCM 的薄封装。
//
// 与 internal/crypto.Encrypt 的区别只有一处：这里不把 master.key 当密钥，
// 而用口令派生的 key。所以不能直接复用那个函数 —— 它的密钥来源固定。
func gcmSeal(key, nonce, plain []byte) ([]byte, error) {
	g, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return g.Seal(nil, nonce, plain, nil), nil
}

func gcmOpen(key, nonce, ct []byte) ([]byte, error) {
	g, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return g.Open(nil, nonce, ct, nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
