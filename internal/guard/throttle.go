package guard

import (
	"sync"
	"time"
)

// LoginFailLimit / LoginCooldown 是登录失败的限速参数，导出给
// admin 包的登录端点复用，保证「登录失败」与「管理鉴权失败」同一套阈值。
const (
	LoginFailLimit = loginFailLimit
	LoginCooldown  = loginCooldown
)

const (
	// loginFailLimit 是同一来源 IP 连续鉴权失败多少次后进入冷却。
	// 密码正确的用户一次成功即清零，正常使用不可能触及。
	loginFailLimit = 10
	// loginCooldown 是冷却时长。冷却期内连 KDF 都不做 ——
	// 顺带把爆破者本可以逼我们消耗的 CPU 也省掉。
	loginCooldown = time.Minute
)

// FailureThrottle 按来源 IP 记连续鉴权失败，给管理登录限速。
//
// 为什么需要它：管理密码下限只有 6 位，PBKDF2 21 万迭代把单次尝试压到几十毫秒
// （对交互无感），但并发下 6 位弱口令依然可爆破 —— 而且失败路径此前不留任何痕迹，
// 日志里看不出有人在猜密码。
type FailureThrottle struct {
	mu       sync.Mutex
	limit    int
	cooldown time.Duration
	fails    map[string]*failEntry
	// capConcurrent 为假时不施加并发上限，只统计失败次数。
	//
	// 并发上限的**唯一**目的（见 inflight 的注释）是防止同一 IP 用 N 个并发
	// 请求把 PBKDF2 的 CPU 消耗放大 N 倍 —— 只有真的要跑 KDF 的路径才需要它。
	// 会话鉴权路径（UserAuthMiddleware）一次 KDF 都不跑，却曾被连带套上这个
	// 上限，后果是普通页面加载直接失败（2026-10-10 修复的 bug）：
	// 总览页一进来就并发 5 个请求（统计/按天/按模型/按 key/按厂商），
	// 而上限是 4 —— 于是第 5 个必然 429，页面报「密码尝试过于频繁」，
	// 而这个用户密码完全正确、根本没在尝试登录。
	//
	// 失败计数（limit/cooldown）仍照常生效，所以「拿无效令牌撞门」的防护没变，
	// 去掉的只是那条与 KDF 无关、只会误伤正常页面的并发限制。
	capConcurrent bool
}

type failEntry struct {
	count int
	until time.Time
	// inflight 是当前正在跑 KDF 的并发请求数。Allow 必须**先占用再放行**：
	// 只读计数的话，同 IP 的 N 个并发请求会在第一次 Fail 落地前全部通过，
	// 既击穿限速，又把 PBKDF2 的 CPU 消耗放大 N 倍。
	inflight int
}

func NewFailureThrottle(limit int, cooldown time.Duration) *FailureThrottle {
	if limit <= 0 {
		limit = loginFailLimit
	}
	if cooldown <= 0 {
		cooldown = loginCooldown
	}
	return &FailureThrottle{
		limit: limit, cooldown: cooldown,
		fails: make(map[string]*failEntry),
		// 默认施加上限：登录 / 引导 / 改密三条路径都要跑 KDF。
		capConcurrent: true,
	}
}

// WithoutConcurrencyCap 造一个只统计失败次数、不限并发的限速器。
//
// 用于**不跑 KDF** 的路径（会话鉴权）。那里保留并发上限没有任何防护收益，
// 只会把正常页面的并发读请求判成限速（见 capConcurrent 的注释）。
func (t *FailureThrottle) WithoutConcurrencyCap() *FailureThrottle {
	t.capConcurrent = false
	return t
}

// ConcurrencyCapped 报告当前限速器是否启用了并发上限。
// 仅供测试断言构造约定；生产路径不需要读取它。
func (t *FailureThrottle) ConcurrencyCapped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.capConcurrent
}

// Allow 报告该 IP 当前是否允许尝试。返回 true 时同时**占用一个并发额度**，
// 调用方必须在验证结束后调用 Release 归还（Success/Fail 内部已各自归还，
// 只有中途 return 的分支需要手动调）。
func (t *FailureThrottle) Allow(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.fails[ip]
	if !ok {
		t.fails[ip] = &failEntry{inflight: 1}
		return true
	}
	if !e.until.IsZero() && time.Now().Before(e.until) {
		return false
	}
	// 并发上限：同一个 IP 最多同时有 maxConcurrent 个请求在跑 KDF。
	// 仅对 capConcurrent 的限速器生效（见该字段的注释）。
	if t.capConcurrent && e.inflight >= maxConcurrentAttempts {
		return false
	}
	e.inflight++
	return true
}

// maxConcurrentAttempts 是单个来源 IP 允许同时进行的鉴权尝试数。
// 超出的请求直接按限速处理（429），不给「并发抢先」留窗口。
const maxConcurrentAttempts = 4

// Release 归还 Allow 占用的并发额度。ip 没有条目时是 no-op
// （Success 删条目后迟到的 Release 不该凭空造出一个新条目）。
func (t *FailureThrottle) Release(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.fails[ip]; ok {
		if e.inflight > 0 {
			e.inflight--
		}
		if e.inflight == 0 && e.count == 0 && e.until.IsZero() {
			delete(t.fails, ip)
		}
	}
	if len(t.fails) >= maxEntries {
		t.sweepLocked()
	}
}

// RetryAfter 返回冷却剩余时长；未处于冷却期为 0。
func (t *FailureThrottle) RetryAfter(ip string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.fails[ip]
	if !ok {
		return 0
	}
	if d := time.Until(e.until); d > 0 {
		return d
	}
	return 0
}

// Fail 记一次失败；达到阈值即进入冷却并把计数清零。
//
// 「是否处于冷却期」必须用 `until` 是否为零值来判定，不能直接写
// `!time.Now().Before(e.until)` —— 未进过冷却的条目 `until` 是零值，
// 而任何时间点都「不在零值之前」，于是那个条件对新条目恒为真：
// 每失败一次就新建条目、count 被清回 1，计数**永远涨不到阈值**，
// 整个限速静默失效（2026-09-24 实测：连打 26 次错误密码全是 401，无一 429）。
func (t *FailureThrottle) Fail(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()

	e, ok := t.fails[ip]
	if !ok {
		e = &failEntry{}
		t.fails[ip] = e
	} else if !e.until.IsZero() && !time.Now().Before(e.until) {
		// 上一轮冷却已结束：从零重新计数。
		// until 必须清回零值，否则下一次 Fail 又会命中这个分支把 count 清零。
		e.count = 0
		e.until = time.Time{}
	}
	// 归还 Allow 占用的并发额度（Fail 是一次尝试的终点）。
	if e.inflight > 0 {
		e.inflight--
	}

	e.count++
	if e.count >= t.limit {
		e.count = 0
		e.until = time.Now().Add(t.cooldown)
	}
}

// Success 清空该 IP 的失败记录。
func (t *FailureThrottle) Success(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.fails[ip]
	if !ok {
		return
	}
	// 仍有并发尝试在跑时不能直接删条目 —— 那些请求结束时会调用 Fail，
	// 删掉条目会让它们凭空造出一个 count=0 的新条目，把失败计数清零。
	// 正确做法是清零失败状态、保留并发计数，等最后一个请求离开时再删。
	e.count = 0
	e.until = time.Time{}
	if e.inflight <= 0 {
		delete(t.fails, ip)
	}
}

// sweepLocked 防止 map 被海量一次性 IP 撑爆：条目数超阈值时清掉所有不在冷却期的记录。
// 代价是攻击者换个 IP 就能重置计数 —— 可接受，这个限速的目标是挡住单个来源的
// 暴力枚举，不是分布式防护。
func (t *FailureThrottle) sweepLocked() {
	if len(t.fails) < maxEntries {
		return
	}
	now := time.Now()
	// 访问预算：这一段跑在**全局互斥锁**里，而所有鉴权路径（Allow / Fail /
	// Success）都要抢同一把锁。扫描本身不能成为放大器 —— 否则攻击者只要
	// 慢速地把条目维持在阈值之上，每次失败尝试都要拖着全表扫一遍。
	// 4096 次 map 迭代在现代 CPU 上是几十微秒量级，取够本量级的预算。
	const visitBudget = 8192
	visited := 0
	for ip, e := range t.fails {
		visited++
		if visited > visitBudget {
			break
		}
		if !now.Before(e.until) {
			delete(t.fails, ip)
		}
	}
	// 冷却期内的条目扫不掉：攻击者把登录尝试摊到成百上千个来源 IP 上，
	// 就能让全部条目同时「在冷却中」，map 从此无界增长，而扫描代价
	// 落在每个合法用户的鉴权请求上。**必须硬封顶**。
	//
	// 无差别淘汰是安全的：条目 key 就是来源 IP，被淘汰的条目顶多让该 IP
	// 的失败计数从头开始 —— 而这正是本函数注释里早就写明并接受的行为
	//（「攻击者换个 IP 就能重置计数」）。真正被防护的场景（单个来源暴力
	// 枚举）只涉及一个 key，永远进不了淘汰路径。
	//
	// Go 的 map 迭代顺序是随机化的，所以遍历中淘汰的条目在 key 空间上
	// 均匀分布，不会变成「先挤掉哪一批」的可预测行为。
	evictTo := func(wantIdle bool) {
		for ip, e := range t.fails {
			if len(t.fails) < maxEntries/2 {
				return
			}
			if wantIdle && e.inflight > 0 {
				continue // 有在途请求的条目留到最后，见下方说明
			}
			delete(t.fails, ip)
		}
	}
	evictTo(true)
	// 仍超上限说明**所有**条目都有在途请求。此时才动它们：并发额度被
	// 淘汰后，迟到的 Fail 会凭空造出一个 count=1 的新条目 —— 该 IP 的
	// 失败计数从头开始，正是本函数早已接受的行为；但让它同时越过并发
	// 上限就是另一回事了，所以第二遍没有「只删空闲」的约束，且必然收敛
	// （map 非空就能删到一半）。
	evictTo(false)
}

// maxEntries bounds successful and failed authentication paths alike.
const maxEntries = 4096
