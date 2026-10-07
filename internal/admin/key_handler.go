package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/crypto"
	"github.com/cn-maul/rosetta-gateway/internal/server"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type KeyHandler struct {
	store *store.Store
}

func NewKeyHandler(st *store.Store) *KeyHandler {
	return &KeyHandler{store: st}
}

// keyRequest 是访问密钥的创建 / PATCH 输入。
// PATCH 语义：字段为指针，nil = 未提供（保持原值），非 nil = 显式赋新值。
// quota_tokens 是终身 token 配额（input+output 累计）：0 = 不限，>0 时请求前预检，
// 达到即 429 拒绝（used_tokens 由 usage 触发器实时累加，见 DESIGN §11）。
type keyRequest struct {
	Name        *string `json:"name"`
	Enabled     *bool   `json:"enabled"`
	QuotaTokens *int64  `json:"quota_tokens"`
	// UserID 指定这把 key 归谁（仅管理员可用）。
	//
	// 留空时的归属规则见 Create：普通用户自动填自己，管理员留空则为无归属
	//（而无归属 key 会被迁移退役、鉴权拒绝—— 见 store.retireOrphanKeys）。
	UserID *string `json:"user_id"`
	// RPMLimit / TPMLimit：Key 维度每分钟限速（DESIGN §11.4），0 = 不限。
	RPMLimit *int `json:"rpm_limit"`
	TPMLimit *int `json:"tpm_limit"`
	// AllowedModels 是 key 级模型白名单（多用户改造 P1）。
	//
	// PATCH 语义与其它字段一致（nil = 保持原值），但这里多一层含义：
	//   - nil     → 保持原白名单；
	//   - []      → **清除**白名单（回到「不限制」）；
	//   - 非空    → 设为该白名单。
	//
	// 「传空数组 = 不限制」而不是「拒绝全部」：管理界面「取消所有勾选后保存」
	// 的自然读法就是「不限了」。要表达「一把模型都不给用」应该直接禁用这把 key，
	// 让语义落在 enabled 上而不是一个反直觉的空白名单。
	AllowedModels *[]string `json:"allowed_models"`
	// ExpiresAt 是有效期截止（毫秒时间戳，P2）。0 = 永不过期。
	// 用指针是为了区分「不传（保持原值）」与「传 0（改成永不过期）」。
	ExpiresAt *int64 `json:"expires_at"`
	// AllowedIPs 是来源 IP 白名单（逗号分隔的 CIDR 或单 IP，P2）。空串 = 不限制。
	// 与 ExpiresAt 同理用指针：「清空白名单」必须能被表达。
	AllowedIPs *string `json:"allowed_ips"`
	// GroupID 是 key 级分组覆盖（P2，仅管理员可用）。空串 = 沿用归属用户的分组。
	//
	// 为什么只给管理员：普通用户若能自选，就能把自己的 key 指向一个更宽松的组，
	// 绕过自己所属组的限制 —— 这是权限提升，不是配置。
	GroupID *string `json:"group_id"`
}

type keyResponse struct {
	ID        string `json:"id"`
	KeyPrefix string `json:"key_prefix"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	// UserID 是归属用户。空串 = **无归属**：那把 key 会被迁移退役、
	// 鉴权直接 401（见 store.retireOrphanKeys）。界面上必须显式标注
	// 「无归属（不可用）」，否则运维会以为它还能用。
	UserID      string `json:"user_id"`
	Username    string `json:"username,omitempty"`
	QuotaTokens int64  `json:"quota_tokens"`
	UsedTokens  int64  `json:"used_tokens"`
	RPMLimit    int    `json:"rpm_limit"`
	TPMLimit    int    `json:"tpm_limit"`
	// AllowedModels 为空数组 = 该 key 不限制模型（与「用户所属组」求交）。
	// 前端要能区分「空 = 不限」与「有值 = 白名单」，否则会把它渲染成
	// 「一个模型都不允许」。
	AllowedModels []string `json:"allowed_models"`
	// ExpiresAt / AllowedIPs / GroupID 是 P2 的三个字段。
	ExpiresAt  int64  `json:"expires_at"`
	AllowedIPs string `json:"allowed_ips"`
	GroupID    string `json:"group_id"`
	CreatedAt  int64  `json:"created_at"`
}

type keyCreateResponse struct {
	keyResponse
	PlaintextKey string `json:"plaintext_key"`
}

// List 返回访问密钥列表。
//
// # 作用域收窄（多用户改造 P0.5，最要命的一类漏洞）
//
// 改造前这里返回**全部** key。多用户后普通用户若看到全部，
// 就等于拿到了同事的 key_prefix、已用量、以及名字里可能带的项目代号。
//
// 所以：admin 看全部，普通用户只看自己的。
// 这个过滤**不靠调用方自觉** —— 它由中间件注入的 server.UserFromContext 决定，
// 漏写在这里等于数据泄露。
func (h *KeyHandler) List(w http.ResponseWriter, r *http.Request) {
	me := server.UserFromContext(r.Context())
	var keys []store.AccessKey
	var err error
	if me != nil && !me.IsAdmin() {
		// 普通用户：只看自己的。me.ID 为空是引导态合成用户，
		// 已被 requireAdmin 类的端点挡住，这里不重复判断。
		keys, err = h.store.ListAccessKeysByUser(r.Context(), me.ID)
	} else {
		keys, err = h.store.ListAccessKeys(r.Context())
	}
	if err != nil {
		writeServerError(w, "list keys", err)
		return
	}
	result := make([]keyResponse, 0, len(keys))
	for _, k := range keys {
		result = append(result, toKeyResponse(k))
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *KeyHandler) Create(w http.ResponseWriter, r *http.Request) {
	// 发 key 收敛为管理员专属。
	//
	// 自助发 key 曾对普通用户开放，并有「额度封顶」兜底（保留在下方作纵深
	// 防御）。但兜底挡不住绕过：管理员落在**具体某把 key** 上的强制策略 ——
	// 禁用、更紧的配额、RPM/TPM、有效期、IP 白名单 —— 用户随时重新建一把
	// （enabled=true、无期限、无 IP 限制）即可全部绕开；而 rpm/tpm 被强制
	// 清 0（不限速）加上「用户级额度未设即不限」，默认配置下普通用户可以
	// 自铸不限量、不限速的凭证，把上游的真实费用敞口直接打开。
	// 不可绕过的外层闸门（users.quota_tokens、用户禁用、组白名单）约束的是
	// 「这个人」，替代不了「管理员要约束某把具体 key」的语义。
	// 真正的自助发 key 需要用户/组级的强制 ceiling（schema + 热路径配合），
	// 是独立特性；在它存在之前，发 key 只能是管理动作。
	if !callerIsAdmin(r) {
		writeError(w, http.StatusForbidden, "只有管理员可以创建密钥")
		return
	}

	var req keyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	name := strings.TrimSpace(derefStr(req.Name))
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	plainKey := "sk-gw-" + crypto.GenerateKey()
	hash := sha256.Sum256([]byte(plainKey))
	keyHash := hex.EncodeToString(hash[:])
	keyPrefix := plainKey[:11] + "..."

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	var quota int64
	if req.QuotaTokens != nil {
		if *req.QuotaTokens < 0 {
			writeError(w, http.StatusBadRequest, "quota_tokens cannot be negative")
			return
		}
		quota = *req.QuotaTokens
	}

	rpm, tpm, bad, err := limitPair(req)
	if bad != "" {
		writeError(w, http.StatusBadRequest, bad)
		return
	}
	if err != nil {
		writeServerError(w, "parse limits", err)
		return
	}

	// 自助建 key 的额度必须**封顶**。
	//
	// 原实现让普通用户在创建请求里任意填 quota/rpm/tpm，而 0 = 不限 ——
	// 于是任何人建一把 key 就能得到「不限额、不限速」的凭证，key 级限速
	// 被完全架空。用户级配额（users.quota_tokens）是三级配额的**外层总闸**，
	// 但内层被自己拆掉之后，总闸形同虚设：只要额度够，总闸本就会拦住，
	// 于是真正起作用的是「无限的内层」，总闸反而成了摆设。
	//
	// 上限从哪来：用户级**只有** quota_tokens（users 表刻意没有 rpm/tpm 列，
	// 见 store.go 的注释 —— 那两个能力没有用户级执行点）。所以：
	//   - quota ≤ 用户级额度；用户级不限（0）时才允许 key 不限；
	//   - rpm/tpm **强制为 0**（不限速）。它们没有用户级上限可比，保留
	//     任意填的能力就是留一个不限速的口子，而 rpm/tpm 的**约束**已由
	//     用户级配额间接实现（总额封顶）。
	//
	// 管理员不受此限制：管理员本来就该能建任意额度的 key。
	if me := server.UserFromContext(r.Context()); me != nil && !me.IsAdmin() {
		var userQuota int64
		if u := snapshot.Get().UsersByID[me.ID]; u != nil {
			userQuota = u.QuotaTokens
		}
		if quota == 0 {
			if userQuota > 0 {
				quota = userQuota
			}
			// userQuota==0 时保持 0（不限）：用户自己没有总额上限，
			// 不该由 key 级凭空造一个。
		} else if userQuota > 0 && quota > userQuota {
			writeError(w, http.StatusForbidden,
				"quota_tokens 不能超过你的用户级额度")
			return
		}
		// rpm/tpm 一律清零：没有用户级上限可比，留任意填就是不限速的口子。
		rpm, tpm = 0, 0
	}

	// 归属：普通用户只能给自己建 key（忽略请求里的 user_id），
	// 管理员可显式指定。管理员留空则建出无归属 key —— 那把 key 会被
	// 下次重启的 retireOrphanKeys 退役掉，鉴权直接 401。
	// 这里**不**拒绝无归属：管理端「先建后认领」是合理流程。
	owner := ""
	me := server.UserFromContext(r.Context())
	if me != nil {
		if me.IsAdmin() {
			owner = strings.TrimSpace(derefStr(req.UserID))
		} else {
			owner = me.ID
		}
	}
	if owner != "" {
		if target, terr := h.store.GetUser(r.Context(), owner); terr != nil {
			writeServerError(w, "resolve key owner", terr)
			return
		} else if target == nil {
			// 归属必须指向真实存在的用户：写进去一个不存在的 id，
			// 鉴权时 UsersByID 查不到 → 401，key 变成「谁都用不了」的死物。
			writeError(w, http.StatusBadRequest, "user_id 指向的用户不存在")
			return
		}
	}

	k := &store.AccessKey{
		ID:          generateID(),
		KeyHash:     keyHash,
		KeyPrefix:   keyPrefix,
		Name:        name,
		Enabled:     enabled,
		QuotaTokens: quota,
		UserID:      owner,
		RPMLimit:    rpm,
		TPMLimit:    tpm,
	}
	// 模型白名单（P1）：与组白名单求交，且 key 级只能更紧。
	// 校验名字真实存在 —— 拼错的后果是「这个模型谁都调不了」而界面无异常。
	if req.AllowedModels != nil {
		if !validateModelAllowlist(w, *req.AllowedModels) {
			return
		}
		k.AllowedModels = *req.AllowedModels
	}

	// P2：有效期 / 来源 IP / 分组覆盖。
	if !h.applyP2(w, r, req, k) {
		return
	}

	if err := h.store.CreateAccessKey(r.Context(), k); err != nil {
		writeServerError(w, "create key", err)
		return
	}

	writeJSON(w, http.StatusCreated, keyCreateResponse{
		keyResponse:  toKeyResponse(*k),
		PlaintextKey: plainKey,
	})
}

// applyP2 写入 P2 的三个字段（有效期 / 来源 IP 白名单 / 分组覆盖）。
// 返回 false 表示已写出错误响应，调用方应立即 return。
//
// 抽出来是因为 Create 与 Update 的校验必须**完全一致**：
// 「Create 校验了 CIDR、Update 忘了校验」这种漂移，会让同一个非法值
// 从另一个入口悄悄写进库 —— 而读路径只能把它折叠成「全拒」，
// 症状是那把 key 从所有地址都连不上，错误信息还指向 IP 而非配置。
func (h *KeyHandler) applyP2(w http.ResponseWriter, r *http.Request, req keyRequest, k *store.AccessKey) bool {
	if req.ExpiresAt != nil {
		if *req.ExpiresAt < 0 {
			writeError(w, http.StatusBadRequest, "expires_at 不能为负（0 = 永不过期）")
			return false
		}
		k.ExpiresAt = *req.ExpiresAt
	}
	if req.AllowedIPs != nil {
		// 写库前解析一遍：坏值一旦落库，读路径会折叠成「全部拒绝」。
		// 解析结果**回写**而不是保留原文：ParseAllowedNets 会逐段 trim、
		// 丢掉空段（"10.0.0.0/8," 与 "10.0.0.0/8" 语义完全相同），
		// 存原文等于让回显与实际生效的规则长得不一样。
		nets, err := store.ParseAllowedNets(*req.AllowedIPs)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return false
		}
		k.AllowedIPs = store.FormatAllowedNets(nets)
	}
	if req.GroupID != nil {
		// 分组覆盖是**管理员的强制手段**，设置与清除都只该由管理员操作。
		// 原实现只拦「设置」：清除分支（id == ""）排在管理员判定之前，
		// 管理员把某把 key 压到更严的组之后，用户一条 {"group_id":""}
		// 就能退回归属用户自带的（往往更宽松的）分组 —— 收紧被静默撤销，
		// 且不走 guardNoLoosening（清空对它来说是「无此字段」）。
		if !callerIsAdmin(r) {
			writeError(w, http.StatusForbidden, "只有管理员可以设置分组覆盖")
			return false
		}
		id := strings.TrimSpace(*req.GroupID)
		if id == "" {
			k.GroupID = "" // 显式清空 = 回到「沿用归属用户的分组」
		} else {
			g, err := h.store.GetGroup(r.Context(), id)
			if err != nil {
				writeServerError(w, "resolve group", err)
				return false
			}
			if g == nil {
				writeError(w, http.StatusBadRequest, "group_id 指向的分组不存在")
				return false
			}
			k.GroupID = id
		}
	}
	return true
}

// callerIsAdmin 报告当前调用者是否是管理员。
//
// 建 key 并指定归属（user_id）是管理动作，普通用户建 key 一律归属自己 ——
// 见 Create 里对 user_id 的强制覆盖。
func callerIsAdmin(r *http.Request) bool {
	me := server.UserFromContext(r.Context())
	return me != nil && me.IsAdmin()
}

// guardNoLoosening 对普通用户执行「只能收紧」规则：管理员通过 Create/Update
// 施加的强制措施（禁用、配额、限速、有效期、IP 白名单）不能被归属者用一条
// PATCH 撤销 —— 否则「因泄露禁用某把 key」这个安全动作对懂 API 的用户形同虚设
// （审计 actor 恒为 "admin"，事后连是谁翻转的都查不到）。
//
// 各字段规则（管理员不受限，直接放行）：
//   - enabled：被禁用的 key 不能自行重新启用（自行禁用仍允许）；
//   - 配额/限速（0 = 不限）：现值非 0 时新值必须是相等或更紧的正数，
//     现值为 0（还没设过）时允许任意设置 —— 那是给自己加限制；
//   - 有效期：现值非 0 时只能缩短或保持，不能清零或顺延（已过期 key 的
//     「复活」路径就在这里堵死）；现值为 0（永不过期）时可设期限；
//   - IP 白名单：现值为空（不限）时可任意收紧；现值非空时新集合必须 ⊆ 现集合
//     （每个新网段都落在某个现有网段内），且不能清空。
//
// allowed_models 刻意不在此列：它有组白名单兜底（管理员控制的有效上限），
// 且自助建 key 本来就能建出一把无白名单的 key —— 只拦这里的「清空」拦不住
// 任何真实威胁，却会破坏「取消所有勾选后保存」的既有界面流程。
//
// 返回 false 表示已写出 403，调用方应立即 return。必须在任何字段写入前调用。
func (h *KeyHandler) guardNoLoosening(w http.ResponseWriter, r *http.Request, req keyRequest, existing *store.AccessKey) bool {
	if callerIsAdmin(r) {
		return true
	}
	if req.Enabled != nil && *req.Enabled && !existing.Enabled {
		writeError(w, http.StatusForbidden, "密钥已被管理员禁用，不能自行重新启用")
		return false
	}
	if req.QuotaTokens != nil && !canTightenQuota(existing.QuotaTokens, *req.QuotaTokens) {
		writeError(w, http.StatusForbidden, "配额只能收紧不能放宽或清零（0 = 不限，需管理员修改）")
		return false
	}
	if req.RPMLimit != nil && !canTightenLimit(int64(existing.RPMLimit), int64(*req.RPMLimit)) {
		writeError(w, http.StatusForbidden, "RPM 限速只能调紧不能放宽或清零（0 = 不限，需管理员修改）")
		return false
	}
	if req.TPMLimit != nil && !canTightenLimit(int64(existing.TPMLimit), int64(*req.TPMLimit)) {
		writeError(w, http.StatusForbidden, "TPM 限速只能调紧不能放宽或清零（0 = 不限，需管理员修改）")
		return false
	}
	if req.ExpiresAt != nil && !canTightenExpiry(existing.ExpiresAt, *req.ExpiresAt) {
		writeError(w, http.StatusForbidden, "有效期只能缩短不能顺延或清除（0 = 永不过期，需管理员修改）")
		return false
	}
	if req.AllowedIPs != nil {
		if msg := ipAllowlistWidened(existing.AllowedIPs, *req.AllowedIPs); msg != "" {
			writeError(w, http.StatusForbidden, msg)
			return false
		}
	}
	return true
}

// canTightenQuota / canTightenLimit：0 = 不限。现值非 0 时新值必须是相等或更紧的正数；
// 现值为 0 时任何非负新值都是收紧或保持。负值返回 true —— 那是非法值不是放宽，
// 交给调用方的 400 校验去拒绝。
func canTightenQuota(existing, new int64) bool {
	if new < 0 {
		return true
	}
	if existing <= 0 {
		return true
	}
	return new > 0 && new <= existing
}

func canTightenLimit(existing, new int64) bool {
	return canTightenQuota(existing, new)
}

// canTightenExpiry：0 = 永不过期。现值非 0（设过期限）时新值必须是相等或更早的
// 正数时间戳；现值为 0 时可设任何期限（含 0 = 保持不过期）。负值同上交给 400。
func canTightenExpiry(existing, new int64) bool {
	if new < 0 {
		return true
	}
	if existing <= 0 {
		return true
	}
	return new > 0 && new <= existing
}

// ipAllowlistWidened 报告把 allowed_ips 从 old 改成 new 是否构成放宽。
// 返回空串 = 允许；非空 = 403 文案。
//
// 两个方向都要拦：清空（= 全部来源放行）与「换个更大的网段」。包含判定用
// 「新集合的每个网段都落在旧集合的某个网段内」—— 保守方向：个别合法的
// 拆分组合会被拒（要求管理员改），漏放比错拒严重得多。
//
// 新值解析失败时返回空串交给 applyP2 去报 400 —— 非法值本来就该 400 而非 403。
// 旧值解析失败（损坏数据）则一律拒绝：在不知道旧集合是什么的情况下，
// 任何改写都可能是在放宽。
func ipAllowlistWidened(old, new string) string {
	oldNets, err := store.ParseAllowedNets(old)
	if err != nil {
		return "现有 IP 白名单无法解析，请联系管理员修正后再改"
	}
	newNets, err := store.ParseAllowedNets(new)
	if err != nil {
		return ""
	}
	if len(oldNets) == 0 {
		// 现在是不限：设任何集合都是收紧（含清空 = 维持不限）。
		return ""
	}
	if len(newNets) == 0 {
		return "IP 白名单只能收紧不能清空（清空 = 不限制来源，需管理员修改）"
	}
	for _, p := range newNets {
		covered := false
		for _, q := range oldNets {
			if q.Bits() <= p.Bits() && q.Contains(p.Addr()) {
				covered = true
				break
			}
		}
		if !covered {
			return "IP 白名单只能收紧不能放宽（新网段必须落在现有网段内）"
		}
	}
	return ""
}

// ownedByCaller 校验「当前调用者有权操作这把 key」。
//
// 改造前所有 /admin/api/keys/{id} 端点都不校验归属，多用户后
// 普通用户改别人的 key 就是数据越权。这里是唯一的判定点：
// admin 放行一切；普通用户必须满足 k.UserID == 自己。
//
// 注意**无归属 key（UserID 为空）对普通用户是不可见的**：
// 放行等于让任何登录用户接管一把不属于任何人的 key。
func (h *KeyHandler) ownedByCaller(w http.ResponseWriter, r *http.Request, k *store.AccessKey) bool {
	me := server.UserFromContext(r.Context())
	if me == nil {
		writeError(w, http.StatusUnauthorized, "需要登录")
		return false
	}
	// 只有真admin 才放行。**空 ID 一律拒绝**（fail-closed）：
	// 统一认证后代码里已无任何构造 ID=="" 用户的路径（会话都对应 users 行），
	// 所以这条分支形同虚设；但它一旦可达就是**静默提权** —— 空 ID 等于
	// admin，方向全错。宁可让某个真 admin 被误判成普通用户（可见、可修），
	// 也不要让「拿不到身份」等价于「拿到最高权限」。
	if me.ID == "" {
		writeError(w, http.StatusUnauthorized, "需要登录")
		return false
	}
	if me.IsAdmin() {
		return true
	}
	if k.UserID != me.ID {
		// 刻意回 404 而不是 403：不告诉调用者「这把 key 存在，只是
		// 不是你的」，那本身也是信息（可枚举他人 key 的 id）。
		writeError(w, http.StatusNotFound, "密钥不存在")
		return false
	}
	return true
}

func (h *KeyHandler) Update(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetAccessKey(r.Context(), id)
	if err != nil {
		writeServerError(w, "get key", err)
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	if !h.ownedByCaller(w, r, existing) {
		return
	}

	var req keyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		if errors.Is(err, errUnsupportedMediaType) {
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// 「只能收紧」守卫必须在任何字段写入**之前**：它比较的是请求想写的新值
	// 与库里现值，先改后判就没了比较基准。
	if !h.guardNoLoosening(w, r, req, existing) {
		return
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
		existing.Name = name
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.QuotaTokens != nil {
		if *req.QuotaTokens < 0 {
			writeError(w, http.StatusBadRequest, "quota_tokens cannot be negative")
			return
		}
		existing.QuotaTokens = *req.QuotaTokens
	}
	// 限速字段沿用 PATCH 语义：nil = 保持原值，显式数字（含 0 = 取消限制）才覆盖。
	if req.RPMLimit != nil {
		if *req.RPMLimit < 0 {
			writeError(w, http.StatusBadRequest, "rpm_limit cannot be negative")
			return
		}
		existing.RPMLimit = *req.RPMLimit
	}
	if req.TPMLimit != nil {
		if *req.TPMLimit < 0 {
			writeError(w, http.StatusBadRequest, "tpm_limit cannot be negative")
			return
		}
		existing.TPMLimit = *req.TPMLimit
	}
	// 模型白名单：nil = 保持原值，[] = 清除（回到不限制），非空 = 覆盖。
	// 空数组归一成 nil 再落库，使「清除」与「从没配过」在数据层是同一状态
	// —— 两个语义本来就该是同一个：都不限制。
	if req.AllowedModels != nil {
		if !validateModelAllowlist(w, *req.AllowedModels) {
			return
		}
		if len(*req.AllowedModels) == 0 {
			existing.AllowedModels = nil
		} else {
			existing.AllowedModels = *req.AllowedModels
		}
	}

	// 归属（认领）：nil = 保持原值，非空 = 改判给该用户。
	//
	// 修复前 Update **从不读** req.UserID（keyRequest 里声明了它），
	// 于是「先建后认领」这个 Create 注释里明确承诺的流程不存在：
	// 实测 PATCH {"user_id":"<真实用户>"} -> 200，响应里 user_id 仍是 ""，
	// 那把 key 之后永远 401（auth.go 的 ErrKeyUnowned）。
	//
	// 只能管理员改归属 —— 与 Create 同一口径；普通用户即使传了也忽略。
	// 落库走独立的 ReassignAccessKey：UpdateAccessKey 刻意不写 user_id
	// （归属不该由一个 PATCH 随手改写）。
	claimTo := ""
	claim := false
	if req.UserID != nil {
		if me := server.UserFromContext(r.Context()); me != nil && me.IsAdmin() {
			claimTo = strings.TrimSpace(*req.UserID)
			claim = true
			if claimTo != "" {
				target, terr := h.store.GetUser(r.Context(), claimTo)
				if terr != nil {
					writeServerError(w, "resolve key owner", terr)
					return
				}
				if target == nil {
					// 与 Create 同理：指向不存在的用户 = 一把永远用不了的死物。
					writeError(w, http.StatusBadRequest, "user_id 指向的用户不存在")
					return
				}
			}
		}
	}

	// P2：有效期 / 来源 IP / 分组覆盖（与 Create 共用同一套校验）。
	if !h.applyP2(w, r, req, existing) {
		return
	}

	if err := h.store.UpdateAccessKey(r.Context(), id, existing); err != nil {
		writeNotFoundOrError(w, "update key", "密钥不存在（可能已被并发删除）", err)
		return
	}
	// 归属单独一步：UpdateAccessKey 刻意不写 user_id（见其注释）。
	if claim {
		if err := h.store.ReassignAccessKey(r.Context(), id, claimTo); err != nil {
			writeNotFoundOrError(w, "reassign key owner", "密钥不存在（可能已被并发删除）", err)
			return
		}
		existing.UserID = claimTo
	}

	writeJSON(w, http.StatusOK, toKeyResponse(*existing))
}

func (h *KeyHandler) Delete(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetAccessKey(r.Context(), id)
	if err != nil {
		writeServerError(w, "get key", err)
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	if !h.ownedByCaller(w, r, existing) {
		return
	}
	if err := h.store.DeleteAccessKey(r.Context(), id); err != nil {
		writeDeleteError(w, "delete key", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// RecomputeUsage 从 usage_records 重算该密钥的已用量并覆盖 used_tokens。
//
// 存在的理由：used_tokens 由数据库触发器单调累加，没有任何回退路径。
// 一旦因误写或 bug 偏高，配额预检会从此恒返回 429，而 Update 刻意不写
// used_tokens（防止请求体随意改配额计数）—— 于是这把 key 在管理界面上
// 变成「怎么改配置都救不回来」的死 key，只能直接改库。这个端点把
// 恢复能力还给运维。
func (h *KeyHandler) RecomputeUsage(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.GetAccessKey(r.Context(), id)
	if err != nil {
		writeServerError(w, "get key", err)
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "key not found")
		return
	}
	if !h.ownedByCaller(w, r, existing) {
		return
	}
	used, err := h.store.RecomputeUsedTokens(r.Context(), id)
	if err != nil {
		// key 不存在时 DAO 返回 ErrNotFound → 404，而不是把「重算了一把
		// 不存在的 key」报成 200 + used_tokens=0（前端会以为重算已完成）。
		writeNotFoundOrError(w, "recompute key usage", "密钥不存在", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"used_tokens": used,
	})
}

// toKeyResponse 把内部结构转成响应。username 从**快照**里的
// snapshot.Get().UsersByID 取，不额外查库。
//
// 此前 username 字段声明了却**永远为空**（死契约）：管理员在密钥列表里
// 只能看到裸 user_id，要确认「这把 key 属于谁」只能去用户页逐个比对 id。
// 用快照零成本 —— UsersByID 本来就随每次重建全量更新（数据面鉴权要读它）。
func toKeyResponse(k store.AccessKey) keyResponse {
	username := ""
	if k.UserID != "" {
		if u := snapshot.Get().UsersByID[k.UserID]; u != nil {
			username = u.Name
		}
	}
	return keyResponse{
		ID:            k.ID,
		KeyPrefix:     k.KeyPrefix,
		Name:          k.Name,
		Enabled:       k.Enabled,
		UserID:        k.UserID,
		Username:      username,
		QuotaTokens:   k.QuotaTokens,
		UsedTokens:    k.UsedTokens,
		RPMLimit:      k.RPMLimit,
		TPMLimit:      k.TPMLimit,
		AllowedModels: allowedModelsForResponse(k.AllowedModels),
		ExpiresAt:     k.ExpiresAt,
		AllowedIPs:    k.AllowedIPs,
		GroupID:       k.GroupID,
		CreatedAt:     k.CreatedAt,
	}
}

// allowedModelsForResponse 把内部的 nil（= 不限制）转成空数组，并按
// **落库时的同一套规则**去重、排序、裁掉空白。
//
// 修复前它直接返回原值，于是创建响应回显**请求原文**
// （["  "," pub-chat ","pub-chat"]），而 600ms 后 GET 读回来是
// ["pub-chat"] —— 同一把 key 前后两个答案。界面「限 N 个模型」徽标
// （web/src/views/Keys.vue:270）读的就是这个回显，管理员会看到「限 3 个」
// 而实际限 1 个。归一后两个答案必然一致。
//
// 「空 = 拒绝全部」这个内部约定**不会**从 API 暴露出去
// （见 store.AccessKey.AllowedModels 的注释：非 nil 空切片只出现在
// 库里 JSON 损坏的兜底路径上，属于故障态）。
func allowedModelsForResponse(models []string) []string {
	if models == nil {
		return []string{}
	}
	return store.NormalizeModelNames(models)
}

// limitPair 解析 rpm_limit / tpm_limit（PATCH 语义 + 非负校验）。
// 返回值约定：bad 非空 = 校验失败（直接 400）；否则 err 上抛为 500。
func limitPair(req keyRequest) (rpm, tpm int, bad string, err error) {
	if req.RPMLimit != nil {
		if *req.RPMLimit < 0 {
			return 0, 0, "rpm_limit cannot be negative", nil
		}
		rpm = *req.RPMLimit
	}
	if req.TPMLimit != nil {
		if *req.TPMLimit < 0 {
			return 0, 0, "tpm_limit cannot be negative", nil
		}
		tpm = *req.TPMLimit
	}
	return rpm, tpm, "", nil
}
