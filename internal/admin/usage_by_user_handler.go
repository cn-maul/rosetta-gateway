package admin

import (
	"net/http"
	"time"
)

// usageUserEntry 是「按用户汇总消费」表格的一行（GET /admin/api/usage/by-user）。
//
// 与 usageGroupEntry 分开而不是复用它：那一行的 name 是「密钥名」，这一行的
// 展示名是「用户名」，且这一行必须多带 cost 与 role。硬塞进 usageGroupEntry
// 会让 by-key 端点下发一个恒为 0 的 cost（前端于是不得不猜「0 是还没算完还是
// 真的没花钱」）—— 一个字段在某个端点上恒为 0，就是契约在说谎。
type usageUserEntry struct {
	// Key 是 usage_records.user_id（冗余固化的归属，见 store.UsageRecord.UserID）。
	// 空串 = 无归属（迁移前的无主 key 产生的历史用量），前端会显示成「无归属」。
	Key string `json:"key"`
	// UserName 是 users.username；**JOIN 不到时为空**，表示该用户已被删除。
	// 前端必须回退显示 Key，留白会被误读成「无归属」（两者是不同的事）。
	UserName string `json:"user_name"`
	// Role 是 users.role（admin / user），用户已删除时为空。
	//
	// 这一列存在的唯一理由是**标注管理员的历史用量**：管理员现在被
	// ErrAdminCannotCallModel 拦在模型调用之外，理论上不该出现在这张表里；
	// 但升级前留下的用量是真实发生过的消费，静默过滤会让这张表的合计小于
	// 总览的全局合计（对不上账）。所以照实返回，由前端加标记。
	Role   string `json:"role"`
	Count  int64  `json:"count"`
	Tokens int64  `json:"tokens"`
	// Cost 是该用户固化的消费合计（元）。这是这张表的主角 ——
	// 用户要的就是「每人花了多少钱」。
	Cost float64 `json:"cost"`
}

// usageByUserResponse 是 by-user 的响应：当页行 + **有消费的用户总数**。
//
// # 为什么这一个 by-* 端点返回对象，而它的三个兄弟返回裸数组
//
// by-key / by-model / by-provider 由总览页调用，固定 limit=10 且只用来画
// 「Top 10」条形图 —— 截断是它们的**既定语义**（标题就写着 Top 10），
// 所以不需要总数。
//
// 而这张表是**按金额读账**的：管理员会把它当成「所有人各花了多少」。
// 一旦用户数超过 limit，一张被截断的表与一张完整的表在界面上完全同形 ——
// 把「前 100 名」读成「全部用户」会直接得出错误的结论（「合计怎么对不上
// 上面那张汇总卡」）。所以必须把 total 下发，让前端能说出「前 N / 共 M」。
// 与 ExportCSV 写 X-Export-Truncated 同一个原则：**截断必须可见**。
type usageByUserResponse struct {
	Records []usageUserEntry `json:"records"`
	Total   int64            `json:"total"`
}

// GroupByUser 按用户汇总消费（GET /admin/api/usage/by-user）。
//
// # 条数上限
//
// limit 走与其它 by-* 同一个 clampLimit（默认 100、上限 1000）。不另立一套：
// 这张表的每一行都对应一个真实用户，真正的保护在前端（钱包页默认只要 100 行），
// 这里的上限只用于防止有人手工构造一个巨大的 limit 把整表物化进内存 ——
// 而 clampLimit 已经把 `limit=-1`（SQLite 语义是「不限制」）这条路堵死了。
//
// # 必须 admin-only，且不能只靠路径白名单（与 Prune 同一个坑）
//
// 本路径落在 **/admin/api/usage** 前缀下，而那个前缀在
// server.userAccessiblePrefixes 白名单里（普通用户要能读自己的用量）。
// 白名单只保证「能进这个端点」，不保证「有权限」。这是一个**全站**口径的
// 端点：它按用户分组、返回每个用户的消费金额与用户名 —— 一个普通用户拿到
// 它就是看到了同事的账单（谁在用什么模型、花了公司多少钱）。
// 所以这里必须显式 requireAdmin，见 Prune 的同款说明。
//
// 判定在 handler 内做而不是靠 AdminGateGuard：那一层是**前缀**匹配，
// 只要前缀在名单里就整体放行，管不到名单内部的单个端点。
//
// # 默认窗口与三个兄弟端点一致（近 7 天）
//
// 刻意不把默认拉宽成「全部」：by-* 这一族对 from 的语义是统一的一套，
// 单独给一个端点换默认值，会让「同一个 from 参数在四个端点上含义不同」——
// 那正是上一轮刚修掉的 queryRange/queryRangeExplicit 分叉。
// 钱包页要「全部历史」时显式发 from=0（explicit 语义，不设下界），
// 这与 stats 端点的「全部」档是同一个口径。
func (h *UsageHandler) GroupByUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	q := r.URL.Query()
	from, to, explicit, err := queryRangeExplicit(q, 7*24*time.Hour)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := clampLimit(q.Get("limit"), defaultUsageLimit, maxUsageLimit)

	// 复用 groupFilter 而不是自己拼 from/to：它是 by-* 这一族对
	// 「explicit && from==0 → 全部历史」的唯一解释点。自己再写一遍，
	// 就等于又造出第二个会漂移的口径。
	//
	// callerScope 在这里恒为 ""（上面刚 requireAdmin 过），所以 scope 收窄
	// 无关紧要 —— 但**仍然走它**：将来若有人放开权限，这个端点的收窄不会
	// 因为「忘了传 scope」而静默失效。
	_, _, filter := groupFilter(from, to, explicit, callerScope(r))

	rows, total, err := h.store.UsageByUser(r.Context(), filter.From, filter.To, int(limit))
	if err != nil {
		writeServerError(w, "usage by user", err)
		return
	}

	entries := make([]usageUserEntry, 0, len(rows))
	for _, s := range rows {
		entries = append(entries, usageUserEntry{
			Key:      s.UserID,
			UserName: s.Username,
			Role:     s.Role,
			Count:    s.Requests,
			Tokens:   s.Tokens,
			Cost:     s.Cost,
		})
	}
	writeJSON(w, http.StatusOK, usageByUserResponse{Records: entries, Total: total})
}
