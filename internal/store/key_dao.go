package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

type AccessKey struct {
	ID        string
	KeyHash   string
	KeyPrefix string
	// KeyEnc 是明文访问密钥的可选加密副本，仅用于「再次复制」。
	//
	// 鉴权始终只查 KeyHash；KeyEnc 不参与数据面热路径。旧库补列后历史行为
	// NULL，这些密钥保持不可回读，不会为了兼容而保存或返回明文。
	KeyEnc []byte
	// HasSecret 表示该行是否有可回读的加密密钥。列表响应据此决定是否
	// 显示复制按钮，不把 BLOB 本身带进每次列表查询。
	HasSecret   bool
	Name        string
	Enabled     bool
	QuotaTokens int64
	UsedTokens  int64
	// UserID 是归属用户（多用户改造 P0）。
	//
	// 空串 = **无归属**。无归属 key 一律失效：鉴权查不到归属用户时
	// 直接返回 ErrKeyUnowned（401）。所以这个字段在热路径上**必然非空**，
	// 「跳过用户级配额」的旧兼容分支已不存在（MULTIUSER.md §5.1 的修订）。
	UserID string
	// AllowedModels 是 key 级模型白名单（多用户改造 P1）。
	//
	// 三种取值的区分（这是个真实的约定，不是笔误）：
	//
	//	nil          → 不限制。对应列 NULL，或合法但为空的数组 '[]'。
	//	非 nil 非空   → 白名单生效，只有列出的公开模型名可见。
	//	非 nil 且空   → **拒绝全部模型**。只出现在「库里的 JSON 解析失败」
	//	               这一条兜底路径上（见 decodeAllowedModels）。
	//
	// 与组白名单求交，且 key 级只能更紧。
	AllowedModels []string
	// ExpiresAt 是有效期截止（毫秒时间戳，P2）。0 = 永不过期。
	//
	// 热路径在鉴权时比较（见 internal/auth），所以过期的 key 立刻 403，
	// 不依赖任何后台任务去扫库改状态。
	ExpiresAt int64
	// AllowedIPs 是来源 IP 白名单的**原文**（逗号分隔的 CIDR 或单 IP，P2）。
	// 空 = 不限制。
	AllowedIPs string
	// AllowedNets 是 AllowedIPs 解析后的结果，供热路径直接比对。
	// 由 parseAllowedNets 在读取时填充，json:"-"（对外不暴露解析产物）。
	//
	// 解析失败时这里是**空切片而非 nil**：见 parseAllowedNets 的注释，
	// 坏值必须让这把 key 从所有地址都不可用，而不是从所有地址都可用。
	AllowedNets []netip.Prefix `json:"-"`
	// GroupID 是 key 级分组覆盖（P2）。空串 = 沿用归属用户的分组。
	GroupID string
	// RPMLimit / TPMLimit 是 Key 维度的每分钟限速（DESIGN §11.4），0 = 不限。
	RPMLimit  int
	TPMLimit  int
	CreatedAt int64
}

// keyColumns 是 access_keys 的统一投影。
//
// 抽成常量是因为本文件里 List / Get / GetByHash / ListByUser 四处查询
// 都要用同一份列清单 —— 手工维护四份必然漂移，而漂移的典型后果是
// Scan 目标错位（把 user_id 扫进 rpm_limit），表现为字段被莫名赋值，
// 且编译与测试全绿。
//
// user_id 必须用 COALESCE 兜成空串：它是外键列，无归属 key 在 SQLite 里
// 是真 NULL，而把 NULL 扫进 string 会报
// "converting NULL to string is unsupported"（见 model_dao 的同款注释）。
const keyColumns = `id, key_hash, key_prefix, (key_enc IS NOT NULL) AS has_secret, name, enabled, quota_tokens, used_tokens, COALESCE(user_id, '') AS user_id, rpm_limit, tpm_limit, allowed_models_json, expires_at, COALESCE(allowed_ips, '') AS allowed_ips, COALESCE(group_id, '') AS group_id, created_at`

func scanAccessKey(sc scanner) (*AccessKey, error) {
	var k AccessKey
	var enabled int
	var hasSecret int
	// allowed_models_json 可空，先用 NullString 承接再解析 —— 直接扫进 string
	// 会在 NULL 上报 "converting NULL to string is unsupported"。
	var allowedJSON sql.NullString
	if err := sc.Scan(&k.ID, &k.KeyHash, &k.KeyPrefix, &hasSecret, &k.Name, &enabled,
		&k.QuotaTokens, &k.UsedTokens, &k.UserID, &k.RPMLimit, &k.TPMLimit,
		&allowedJSON, &k.ExpiresAt, &k.AllowedIPs, &k.GroupID, &k.CreatedAt); err != nil {
		return nil, err
	}
	k.Enabled = enabled == 1
	k.HasSecret = hasSecret == 1
	k.AllowedModels = decodeAllowedModels(allowedJSON)
	k.AllowedNets = parseAllowedNets(k.AllowedIPs)
	return &k, nil
}

// parseAllowedNets 是读路径的包装：把解析错误折叠成「非 nil 的空切片」。
//
// 为什么不返回 error：读路径（ListAccessKeys 等）无法把错误带给调用方 —���
// 而这里的选择只有两个，「不限制」或「全拒」。必须是后者，理由见
// ParseAllowedNets 的说明。管理写入口用的是 ParseAllowedNets 本身，
// 那条路径会把错误原样返回给管理员。
func parseAllowedNets(raw string) []netip.Prefix {
	nets, err := ParseAllowedNets(raw)
	if err != nil {
		return []netip.Prefix{}
	}
	return nets
}

// ParseAllowedNets 解析逗号分隔的 CIDR / 单 IP（P2）。
//
// # 返回值的两种空态含义不同
//
//   - (nil, nil)：**未配置限制** → 任何来源地址都允许。
//   - (nil, err)：配置了但有条目解析不出来。
//
// 为什么坏值必须报错而不是「跳过那条继续」：跳过会得到**部分生效**的白名单 ——
// 列表里第一条能用、第三条不能用，且取决于书写顺序。那是最难排查的形态，
// 管理员会先怀疑防火墙再怀疑网关。所以要么整份通过，要么整份作废。
//
// 单 IP 按 /32（IPv4）或 /128（IPv6）处理，让比对逻辑只有一条路径。
func ParseAllowedNets(raw string) ([]netip.Prefix, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := make([]netip.Prefix, 0, 4)
	var bad []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(part); err == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		bad = append(bad, part)
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("无法解析的 IP/CIDR：%s", strings.Join(bad, ", "))
	}
	return out, nil
}

// FormatAllowedNets 把解析结果写回成规范原文（逗号分隔、逐段去空白、
// 空结果 = 空串 = 不限制）。
//
// 与 ParseAllowedNets 互为逆运算：写库时用它，管理面回显因此与实际生效的
// 规则一致 —— 否则 "10.0.0.0/8," 这类带尾随逗号的输入会被原样存下，
// 看起来像和 "10.0.0.0/8" 是两套规则，其实一模一样。
func FormatAllowedNets(nets []netip.Prefix) string {
	if len(nets) == 0 {
		return ""
	}
	parts := make([]string, 0, len(nets))
	for _, p := range nets {
		parts = append(parts, p.String())
	}
	return strings.Join(parts, ",")
}

// decodeAllowedModels 解析 key 级模型白名单列。
//
// # 解析失败为什么是「拒绝全部」而不是「不限制」
//
// 这是本函数唯一需要小心的判断。该列由我们的 API 用 json.Marshal 写入，
// 正常路径下不会坏；坏值的唯一来源是手工改库或未来的写入 bug。此时：
//
//   - 当成「不限制」= 一次数据故障变成**静默放权**，把本该受限的 key
//     放开给全部模型，且没有任何人会发现。
//   - 当成「拒绝全部」= 那把 key 用不了，立刻有人来问，日志里有 WARN
//     指到具体 key id。损失面是「一把 key 暂时不可用」，且管理员在
//     管理 API 上重新保存一次白名单即可修好。
//
// 也刻意**不**返回 error：读行列失败会让 RebuildFromDB 整体失败，
// 结果是「网关启动不了」（首建快照就在启动路径上）—— 为一个字段的
// 格式问题挡住整个服务，代价完全不成比例。
func decodeAllowedModels(v sql.NullString) []string {
	if !v.Valid || v.String == "" {
		return nil // 未配置限制
	}
	var models []string
	if err := json.Unmarshal([]byte(v.String), &models); err != nil {
		return []string{} // 非 nil 空切片 = 拒绝全部，见 AccessKey.AllowedModels
	}
	if len(models) == 0 {
		return nil // 合法空数组 = 未配置限制（与 NULL 同义）
	}
	out := make([]string, 0, len(models))
	for _, m := range models {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// encodeAllowedModels 把白名单序列化进列。nil → NULL（不限），
// 其余 → JSON 数组。已去重排序，写入顺序确定。
func encodeAllowedModels(models []string) any {
	normalized := NormalizeModelNames(models)
	if len(normalized) == 0 {
		return nil
	}
	b, err := json.Marshal(normalized)
	if err != nil {
		// []string 的 Marshal 不会有错；真的发生了宁可存 NULL（不限制）
		// 也比写入一个半截字符串好 —— 后者会被下一次读取判为损坏。
		return nil
	}
	return string(b)
}

func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func (s *Store) ListAccessKeys(ctx context.Context) ([]AccessKey, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+keyColumns+` FROM access_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]AccessKey, 0)
	for rows.Next() {
		k, err := scanAccessKey(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *k)
	}
	return result, rows.Err()
}

func (s *Store) GetAccessKey(ctx context.Context, id string) (*AccessKey, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+keyColumns+` FROM access_keys WHERE id = ?`, id)
	k, err := scanAccessKey(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return k, err
}

// GetAccessKeyByHash 按 SHA-256 哈希取 key。数据面热路径走快照不查库，
// 这条给需要单条查询的管理面路径用（如归属一致性核对）。
func (s *Store) GetAccessKeyByHash(ctx context.Context, keyHash string) (*AccessKey, error) {
	row := s.read.QueryRowContext(ctx,
		`SELECT `+keyColumns+` FROM access_keys WHERE key_hash = ?`, keyHash)
	k, err := scanAccessKey(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return k, err
}

// ListAccessKeysByUser 返回某用户名下的全部 key（按创建时间）。
//
// userID 为空时返回**空切片**而非全部 ——「无归属」不该被当成「查所有」，
// 否则一个漏传 userID 的调用会把无归属 key 连同其用量一并暴露出去。
func (s *Store) ListAccessKeysByUser(ctx context.Context, userID string) ([]AccessKey, error) {
	if userID == "" {
		return []AccessKey{}, nil
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+keyColumns+` FROM access_keys WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]AccessKey, 0, 8)
	for rows.Next() {
		k, err := scanAccessKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

func (s *Store) CreateAccessKey(ctx context.Context, k *AccessKey) error {
	now := time.Now().UnixMilli()
	enabled := 0
	if k.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO access_keys (id, key_hash, key_prefix, key_enc, name, enabled, quota_tokens, user_id, rpm_limit, tpm_limit, allowed_models_json, expires_at, allowed_ips, group_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.KeyHash, k.KeyPrefix, nullableBytes(k.KeyEnc), k.Name, enabled, k.QuotaTokens, nullIfEmpty(k.UserID),
		k.RPMLimit, k.TPMLimit, encodeAllowedModels(k.AllowedModels), k.ExpiresAt,
		nullIfEmpty(k.AllowedIPs), nullIfEmpty(k.GroupID), now)
	// 回写时间戳：落库用的是局部变量 now，结构体仍是零值 → 创建响应里的
	// created_at 会是 0，与随后 GET 到的同一条记录不一致（前端会显示「建于 1970」）。
	if err == nil {
		k.CreatedAt = now
		k.HasSecret = len(k.KeyEnc) > 0
	}
	return err
}

// AccessKeySecret 返回访问密钥的加密副本。
//
// found=false 表示该 key 不存在；found=true 且 enc 为空表示这把 key
// 创建于「可回读」上线之前，只有哈希没有密文。
func (s *Store) AccessKeySecret(ctx context.Context, id string) (blob []byte, found bool, err error) {
	err = s.read.QueryRowContext(ctx,
		`SELECT key_enc FROM access_keys WHERE id = ?`, id).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return blob, true, nil
}

// UpdateAccessKey 更新 key 的可管理字段。
//
// 刻意**不写 user_id**：归属是「key 属于谁」的事实，不该由一个 PATCH
// 请求随手改写 —— 那等于允许把别人的 key 划到自己名下。
// 转归属需要专门的接口并校验权限。
//
// allowed_models_json 可写：它是「限制」不是「归属」，而且收紧限制本身
// 不构成越权（用户把自己的 key 收窄是合理操作）。
func (s *Store) UpdateAccessKey(ctx context.Context, id string, k *AccessKey) error {
	enabled := 0
	if k.Enabled {
		enabled = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE access_keys SET name = ?, enabled = ?, quota_tokens = ?, rpm_limit = ?, tpm_limit = ?,
		        allowed_models_json = ?, expires_at = ?, allowed_ips = ?, group_id = ?
		  WHERE id = ?`,
		k.Name, enabled, k.QuotaTokens, k.RPMLimit, k.TPMLimit,
		encodeAllowedModels(k.AllowedModels), k.ExpiresAt,
		nullIfEmpty(k.AllowedIPs), nullIfEmpty(k.GroupID), id)
	return checkAffected(res, err)
}

// ReassignAccessKey 单独改写一把 key 的归属。
//
// 单独一个方法而不是给 UpdateAccessKey 加参数：归属是「key 属于谁」的
// 事实，不该由一个 PATCH 请求随手改写 —— 那等于允许把别人的 key 划到自己
// 名下（见 UpdateAccessKey 的注释与 user_dao_test 的断言）。
//
// 认领（无归属 → 某用户）正是 Create 里承诺的「先建后认领」流程所缺的
// 那一环：修复前 keyRequest 声明了 UserID，Update 却不读它，DAO 也不写
// user_id，于是 PATCH {"user_id":"…"} 返回 200 而 user_id 恒为空，
// 那把 key 之后永远 401（auth.go 的 ErrKeyUnowned）。
//
// 权限校验（只有管理员可改归属、目标用户必须存在）在 admin 层做。
//
// # 现状（2026-10-21 修 A7-P2-08 之后）
//
// 生产已无调用点：admin 的 Key PATCH 改走 UpdateAccessKeyWithOwner
// （字段与归属同一个事务，避免两步写的部分提交窗口）。保留本方法是因为
// 它是「只改归属、绝不碰别的字段」的单一职责入口 —— 与 UpdateAccessKey
// 刻意不写 user_id 是同一条口径的两半。将来若有「纯转归属」的入口
// （例如管理界面上单独的「转移」按钮），用它而不是现场拼 SQL。
func (s *Store) ReassignAccessKey(ctx context.Context, id, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE access_keys SET user_id = ? WHERE id = ?`, nullIfEmpty(userID), id)
	return checkAffected(res, err)
}

// UpdateAccessKeyWithOwner 在**一个事务**里更新 key 的字段并改写归属
// （2026-10-21 修 A7-P2-08）。
//
// # 为什么需要它
//
// 原先 PATCH 是两次自动提交：先 UpdateAccessKey（名称/配额/有效期/…），
// 再 ReassignAccessKey（归属）。第二步失败时第一步**已经落库**，而响应是
// 404/500 —— 调用方看到失败，库里却已经是新配置。此时：
//   - 客户端重试 → 以为回到原点，实际带着上一次的半截修改继续跑；
//   - 运维手工核对 → 「明明保存失败了，怎么限制真的变了」；
//   - 更坏的是**归属**：重命名成功、改归属失败，这把 key 仍属于原用户，
//     而界面按 200 的旧响应显示新主人 —— 下一次调用就用错了身份计费。
//
// 分两步的真实理由（「归属不该由一个 PATCH 随手改写」）在权限层已经
// 由 handler 保证：只有管理员能改归属，且目标用户必须存在。DAO 层没有
// 必要为此再付一次「非原子」的代价。
//
// # 为什么不是「把 user_id 加进 UpdateAccessKey」
//
// UpdateAccessKey 被 20+ 处测试与内部路径直接调用，它们的语义都是
// 「绝不碰归属」（改配置不该顺带换主人）。给它加参数会让每一个现有调用点
// 都要想一遍「我这次会不会顺手改了归属」。单独一个显式方法，名字就把
// 「这会动归属」写在脸上。
func (s *Store) UpdateAccessKeyWithOwner(ctx context.Context, id string, k *AccessKey, newUserID string, changeOwner bool) error {
	return s.RunTx(ctx, func(ctx context.Context, ex execer) error {
		enabled := 0
		if k.Enabled {
			enabled = 1
		}
		res, err := ex.ExecContext(ctx,
			`UPDATE access_keys SET name = ?, enabled = ?, quota_tokens = ?, rpm_limit = ?, tpm_limit = ?,
			        allowed_models_json = ?, expires_at = ?, allowed_ips = ?, group_id = ?
			  WHERE id = ?`,
			k.Name, enabled, k.QuotaTokens, k.RPMLimit, k.TPMLimit,
			encodeAllowedModels(k.AllowedModels), k.ExpiresAt,
			nullIfEmpty(k.AllowedIPs), nullIfEmpty(k.GroupID), id)
		if err != nil {
			return err
		}
		if err := checkAffected(res, nil); err != nil {
			return err
		}
		if !changeOwner {
			return nil
		}
		res2, err := ex.ExecContext(ctx,
			`UPDATE access_keys SET user_id = ? WHERE id = ?`, nullIfEmpty(newUserID), id)
		if err != nil {
			return err
		}
		return checkAffected(res2, nil)
	})
}

// GetKeyQuota 读单个密钥的配额与已用量，供热路径预检。
// ok=false 表示密钥不存在（已删除）。used_tokens 由 usage_records 触发器实时累加，
// 故这里是权威值；配额预检直接查库而非读快照——快照只在管理写操作后重建，会严重滞后。
func (s *Store) GetKeyQuota(ctx context.Context, id string) (quota, used int64, ok bool, err error) {
	err = s.read.QueryRowContext(ctx,
		`SELECT quota_tokens, used_tokens FROM access_keys WHERE id = ?`, id).
		Scan(&quota, &used)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return quota, used, true, nil
}

// DeleteAccessKey 删除一把访问密钥；id 不存在时返回 ErrNotFound。
func (s *Store) DeleteAccessKey(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "access_keys", id)
}

// RecomputeUsedTokens 从 usage_records 重新计算某把密钥的已用量并写回
// access_keys.used_tokens，返回修正后的值。
//
// 为什么需要它：used_tokens 由 INSERT 触发器单调累加（store.go 的
// trg_update_used_tokens），**没有任何回退或重算路径**。一次误写、一次手工
// 插记录、或一次 bug，都会让它永久偏高 —— 而配额预检（GetKeyQuota 的
// used >= quota）会从此对这把 key 恒返回 429，且管理 API 里没有修正入口，
// 唯一的办法是直接改库。这是「触发器维护的派生值」的固有代价，
// 补一个显式重算把恢复能力还给运维。
func (s *Store) RecomputeUsedTokens(ctx context.Context, keyID string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var used int64
	// 只有计入配额的记录才应计入 used_tokens。当前 total_tokens 即计费口径，
	// 故直接求和；status 不筛 —— canceled 的请求同样消耗了上游配额。
	//
	// 数据源必须是 UsageSource（明细 ∪ 日归档），与热路径的 GetKeyQuota
	// 读取的 used_tokens 同口径：少算归档就等于给了一个「把已用量改小」的
	// 入口，一次误用就永久放宽这把 key 的配额。
	//
	// 用写池的 tx 而不是 s.read：读池在 WAL 下可能读到本事务之前的状态，
	// 而重算写回的是这个值，跨事务读会让「重算」在并发下不可复现。
	src, args := UsageSource(UsageFilter{KeyID: keyID})
	if err := tx.QueryRowContext(ctx,
		`SELECT `+UsageSumExpr("total_tokens", "u")+` FROM `+src, args...).Scan(&used); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE access_keys SET used_tokens = ? WHERE id = ?`, used, keyID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		// 老版本 SQLite 驱动可能不支持；不因此把成功变成失败。
		s.logger.Debug("recompute used_tokens: RowsAffected unavailable", "error", err)
	} else if n == 0 {
		return 0, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return used, nil
}

// ReserveQuota 为一次请求**原子地**预占 est 个 token 的配额，返回是否放行。
//
// # 为什么需要预占（而不是「查了再放」）
//
// GetKeyQuota 是纯查询，两个并发请求会同时读到同一个 used，于是**都通过**，
// 各自再发一次完整生成 —— 并发突发可超发数十倍于剩余额度。
// 原注释说「并发下容忍至多一个在途超发」，那只在**串行**时成立。
//
// 更要命的是「数十倍」而不是「多一个」：剩余额度 1000 token 时，
// 50 个并发请求每个预估 200 token，check-then-act 会让它们全部通过。
//
// # 为什么预占写独立的 reserved_tokens 列，而不是 used_tokens
//
// used_tokens 由 usage_records 的 AFTER INSERT 触发器实时累加（store.go 的
// trg_update_used_tokens），真实用量落库时它**自己会涨**。旧实现把预占也加进
// used_tokens、收尾 ReleaseQuota 按 actual-est 补差，一次请求的净记账是
// est + actual + (actual - est) = 2×actual —— 每个请求双倍扣费
// （2026-10-07 修复的 P0-2）。现在预占单独落在 reserved_tokens：
// 判定用 used+reserved+est 三者之和，释放原额退 reserved_tokens，
// used_tokens 全程只归触发器管，两个口径不再纠缠。
//
// # 为什么不能用 ratelimit 的窗口限速器
//
// TPM 限的是**速率**（每分钟窗口，窗口一翻自然释放）；配额限的是**终身累计**，
// 没有「窗口结束」这回事。所以预占必须落在库里、随真实用量校正：
// ReserveQuota 加，ReleaseQuota 减，两者都在**同一条写事务**里，
// 与检查合并成一句 UPDATE —— 这样「检查」与「占用」之间不存在窗口。
//
// 落库而不是放内存：网关是单进程，写池单连接，内存预占在崩溃/重启后
// 全部消失（额度被白白释放）；更重要的是它无法与 used_tokens 保持一致。
func (s *Store) ReserveQuota(ctx context.Context, id string, est int64) (reserved int64, ok bool, err error) {
	if est <= 0 {
		// 预估为 0（未知长度）时不能直接放行 —— 那样等于没有预检；
		// 也不能因为「要花 0」就拒绝。折中：按 1 个 token 预占，
		// 至少能把并发请求数本身变成约束。
		est = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // 已提交时是 no-op

	// 审计 M1：这条 UPDATE 必须是事务的**第一条语句**。
	//
	// 旧实现先 SELECT quota/used/reserved 再 UPDATE。SQLite 的 DEFERRED 事务
	// 在「先读后写」时，如果读快照与第一次写之间有其它连接提交，升级写事务
	// 会返回 SQLITE_BUSY_SNAPSHOT；_busy_timeout 对这种「快照已失效」不生效。
	// 调用方（main.go 配额预检）是 fail-open 的，于是跨连接竞争下会静默
	// 放行超额请求。把「检查 + 占用」压进一条条件 UPDATE，事务一开始就拿写锁。
	//
	// quota_tokens <= 0 表示不限额（GetKeyQuota 同口径）：此时不预占。
	// 条件里同时判断 used + reserved + est <= quota，命中 0 行后再区分
	// 「key 不存在」「不限额」「额度不够」三种语义。
	res, err := tx.ExecContext(ctx,
		`UPDATE access_keys
		    SET reserved_tokens = reserved_tokens + ?
		  WHERE id = ? AND quota_tokens > 0
		    AND used_tokens + reserved_tokens + ? <= quota_tokens`,
		est, id, est)
	if err != nil {
		return 0, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if n == 0 {
		// 未命中的三种原因必须区分：不存在/不限额是 no-op 或放行，
		// 只有「额度不足」才是拒绝。
		var quota int64
		qerr := tx.QueryRowContext(ctx,
			`SELECT quota_tokens FROM access_keys WHERE id = ?`, id).Scan(&quota)
		if errors.Is(qerr, sql.ErrNoRows) {
			return 0, false, nil
		}
		if qerr != nil {
			return 0, false, qerr
		}
		if quota <= 0 {
			return 0, true, tx.Commit()
		}
		return 0, false, nil // 剩余额度不够本次预估
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return est, true, nil
}

// ReleaseQuota 原额退掉一次请求的配额预占（按 reserved 全额退，不补差）。
//
// 真实用量**不在这里记账**：usage_records 落库时触发器已把 total_tokens 累进
// used_tokens，这里再按 actual-est 补差就是双倍扣费（2026-10-07 修复的 P0-2，
// 「差值补齐/退回」的旧语义随之作废）。actual 参数因此不再需要。
//
// 上游没报 usage 时调用方**也必须**调用本方法：预占按 max_tokens 全额计，
// 不退就是每次 missing 请求永久吃掉一笔额度（修复前整条收尾被跳过的正是
// 这种情况）。漏账本身已由 usage_state="missing" 在报表里可见
// （见 DESIGN §17 R9）—— 报表可见与额度可回收是两回事，后者不能省。
//
// 用 MAX(0, …) 夹住下界：重复释放或与其它路径的释放竞争时可能退过头，
// reserved_tokens 变负等于凭空多出额度，后续 used+reserved+est 判定会被
// 静默放宽。
func (s *Store) ReleaseQuota(ctx context.Context, id string, reserved int64) error {
	if reserved <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE access_keys SET reserved_tokens = MAX(0, reserved_tokens - ?) WHERE id = ?`, reserved, id)
	return err
}
