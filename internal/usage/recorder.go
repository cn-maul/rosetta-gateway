package usage

// Package usage owns asynchronous usage persistence and balance charging.
// It deliberately knows nothing about HTTP, routing, or the runtime snapshot:
// callers inject the balance-exemption policy through Config.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// BatchInsertFunc is the batch persistence hook. Production uses the store's
// implementation; tests can inject transient failures.
type BatchInsertFunc func(context.Context, []*store.UsageRecord) ([]float64, error)

// Config contains the recorder's explicit dependencies.
type Config struct {
	DB              *store.Store
	Logger          *slog.Logger
	BatchInsert     BatchInsertFunc
	IsBalanceExempt func(userID string) bool
}

// New starts the usage recorder workers.
func New(cfg Config) *Recorder {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	isExempt := cfg.IsBalanceExempt
	if isExempt == nil {
		isExempt = func(string) bool { return false }
	}
	u := &Recorder{
		db:              cfg.DB,
		logger:          logger,
		queue:           make(chan *store.UsageRecord, usageQueueSize),
		stop:            make(chan struct{}),
		batchInsert:     cfg.BatchInsert,
		isBalanceExempt: isExempt,
		done:            make(chan struct{}),
	}
	for i := 0; i < usageWorkers; i++ {
		u.wg.Add(1)
		go u.worker()
	}
	go func() {
		u.wg.Wait()
		close(u.done)
	}()
	return u
}

// usage_records.status / usage_state 的取值。
//
// 这两个字符串是 usage 记录的**判读字段**（报表、成功率、以及余额扣费的
// 判据都在读它们），散成裸字面量时改一处忘一处的后果很隐蔽：扣费判据一旦
// 与落库侧写的不一致，用户就会「明明成功却被免单」或「明明失败却被扣钱」，
// 而这两种都**没有任何报错**。故在此固定成常量 —— 只给**新增**的判读方
// （Recorder.charge）用，既有落库处的裸字面量保持原样，避免本次改动
// 波及热路径上每一条 usage 记录。
const (
	// usageStatusOK 是「这次调用真的成功」。与 dataplane.Outcome.Success 的
	// 判据同源：流式只有 status=="ok" 才算成功，断流/溢出/中断都不是。
	usageStatusOK = "ok"
)

// Recorder 负责把用量记录异步落库，并在关停时等它们写完。
//
// 异步是必须的 —— 不能让客户端为了等一次 SQLite 写多耗一个来回；但纯粹的
// fire-and-forget 有真实代价：srv.Shutdown 只等 handler 返回、不等这些 goroutine，
// 进程退出时尾部若干条 usage 会凭空消失（表现是配额与统计对不上账）。
//
// 用固定数量的 worker + 有界队列取代「每条记录一个 goroutine」：
//   - 高并发下不再无限堆积 goroutine（配合 SetMaxOpenConns(1)，无界 goroutine
//     只会全部阻塞在 DB 锁上排队，白白吃内存）；
//   - 队列满时 record 退化为同步写，提供背压而不是静默丢记录。
type Recorder struct {
	db     *store.Store
	logger *slog.Logger
	queue  chan *store.UsageRecord
	wg     sync.WaitGroup

	// stop 让「关停超时后仍在重试」的 worker 立刻退出（2026-10-21 修 *X0）。
	// 原来 worker 的重试退避是裸 time.Sleep，wait 超时返回后它仍会在后台
	// 无限重试；main 返回时 db.Close() 与它并发，日志被 "database is closed"
	// 刷屏，而且进程随时可能退出、手上那批记录既没落库也没留下可对账的痕迹。
	// stop 只打断**退避等待**，不打断正在进行的 flush —— 那一次写可能本来能成功。
	stop     chan struct{}
	stopOnce sync.Once

	// mu 保护 closed，且**把「发送」纳入临界区**（2026-10-10 修复的 P1）。
	//
	// 为什么需要它：`record` 里的 `select { case ch <- rec: default: }`
	// 对**已关闭**的 channel 并不安全 —— 向关闭的 channel 发送是运行时
	// panic，而不是「default 分支」那种「暂时不可写」。只写 `default` 挡不住。
	//
	// 触发顺序：Shutdown 的 10s 预算到期后它只是**返回错误**，并不会杀掉
	// 仍在跑的 handler（http.Server.Shutdown 到点即返回）；紧接着 wait 关闭
	// 队列；此时那条还活着的流收尾调用 record → send on closed channel →
	// panic。流请求的存活时间本来就可能超过 10s（WriteTimeout 为 0，
	// 生命周期只看 TTFT/空闲看门狗），所以这是正常时序，不是边角。
	//
	// 用锁把「判 closed + 发送」变成原子操作：要么在关闭前成功入队，
	// 要么看见 closed 后走同步写，**永远不会 panic**。
	mu     sync.Mutex
	closed bool

	// flushFailed 记录「上一次整批落库失败」，供 worker 决定要不要回灌
	//（2026-10-12 修 P1-1）。
	//
	// 为什么需要这个标志：flush 失败后要把整批保留下来重试，而不能
	// 非阻塞地插回有界队列（usageQueueSize=1024）—— 队列满时插回会静默
	// 丢弃，那正是本次要修的缺陷本身。所以这批留在 worker 手里，
	// flushFailed 就是「手上还攥着一批待重试」的标记，让关停流程能识别出来。
	flushFailed bool

	// batchInsert 是批量落库的注入点（2026-10-12 修 P1-1 的配套）。
	//
	// 为什么需要：flush 的核心分支是「失败 → 重试 → 降级逐条 → 回灌」，
	// 而失败的唯一来源是 DB。真实 SQLite 无法在测试里稳定制造 SQLITE_BUSY
	// （它依赖真实的锁竞争时序），于是这条分支在没有注入点时**永远测不到** ——
	// 而它恰恰是整个修复的核心。
	//
	// nil = 用默认实现（db.CreateUsageRecordsBatched）。仅测试设置。
	// 单条路径不走这里：降级逐条调用 db.CreateUsageRecordWithCost，
	// 测试要构造「批量失败但单条成功」的场景正是靠这个不对称。
	batchInsert func(ctx context.Context, recs []*store.UsageRecord) ([]float64, error)

	// isBalanceExempt 由调用方注入，避免 usage 包反向依赖运行时快照。
	isBalanceExempt func(userID string) bool

	// done 在全部 worker 退出后关闭，仅供生命周期测试观察。
	done chan struct{}
}

const (
	// usageQueueSize 是有界队列容量。超过该值即触发同步写背压。
	usageQueueSize = 1024
	// usageWorkers 是并发落库的 worker 数。SQLite 单写锁下并发写没有收益，
	// 1 个 worker 足够，队列本身负责吸收突发。
	usageWorkers = 1

	// usageBatchSize 是一批最多攒多少条 usage 记录（2026-11 P1 性能修复）。
	//
	// 取 50 是**实测**出来的（batch 探针，
	// 同机同方法）：每行耗时随批大小的曲线是
	//   bs=1 → 305µs   bs=10 → 77µs   bs=50 → 39µs   bs=200 → 45µs
	// 收益在 50 附近饱和，再往上反而略升（大 VALUES 文本变大，
	// 解析与 B-tree 插入都更贵）。
	//
	// 上限的意义不只是吞吐，还有**延迟上界**：一批攒到 50 才发，
	// 最坏情况下每条记录要等 49 个同伴都到齐才落库。高负载下 50 条
	// 形成只需要几十微秒（实测 39µs/行 × 50），可以忽略；
	// 而**低负载下队列根本攒不满**，靠 `default` 分支立刻发出，
	// 所以延迟不受批大小影响 —— 这是选「非阻塞攒批」而非「定时批量」的原因。
	usageBatchSize = 50

	// usageFlushMaxAttempts 是一批落库的**整批重试**次数（2026-10-12 修 P1-1）。
	//
	// 重试针对的是**瞬时**故障：SQLITE_BUSY（写池被剪枝/管理写短暂占住）、
	// 磁盘瞬时抖动。这类错误重试一两次就能过。持久性故障（磁盘满、库损坏）
	// 重试多少次都没用 —— 所以次数刻意给得很小，真正兜底的是整批失败后
	// 的**降级逐条**（见 flush）：把爆炸半径从「整批 50 条」缩到「单条」。
	//
	// 退避用固定 20ms，不搞指数：SQLite 的 busy_timeout 已经是 5s，
	// 走到返回错误说明那次等待已经用尽，再多等意义不大，快点降级更重要。
	usageFlushMaxAttempts = 3
	// usageFlushRetryDelay 是整批重试之间的间隔。
	usageFlushRetryDelay = 20 * time.Millisecond
)

// Exported lifecycle/behavior constants for callers that need to describe or
// test the recorder without reaching into package internals.
const (
	StatusOK  = usageStatusOK
	QueueSize = usageQueueSize
)

// ctxBackground 是 context.Background 的短别名。
//
// 落库是异步的、与请求无关，用 Background 而不是请求的 ctx：
// 客户端断开**不该**取消用量落库（那次调用真的发生了、真的要记账）。
// 抽成常量只是为了不重复 import context 后到处敲长名字。
func ctxBackground() context.Context { return context.Background() }

func newRecorder(db *store.Store, logger *slog.Logger) *Recorder {
	u := &Recorder{
		db:     db,
		logger: logger,
		queue:  make(chan *store.UsageRecord, usageQueueSize),
		stop:   make(chan struct{}),
	}
	for i := 0; i < usageWorkers; i++ {
		u.wg.Add(1)
		go u.worker()
	}
	return u
}

// worker 消费队列并落库，直到队列关闭。
//
// 落库**之后**按「请求是否成功」决定要不要扣余额（Lead 与用户确认的口径）。
// 为什么扣费挂在落库之后、而不是请求结束时按内存里的估算扣：金额必须等于
// 那条 usage 落库时固化的 cost_total。两处各算一份必然漂移，而漂移的后果是
// 「报表显示花了 X、余额少了 Y」且无人能发现。所以这里用
// CreateUsageRecordWithCost —— 它返回的**就是写进 cost_total 列的那个变量**
// （不是重算一次），换句话说「扣的 = 报表的」在结构上就不可能漂移。
//
// # 批量落库（2026-10-11，P1 性能修复）
//
// 原来是 `for rec := range u.queue` 一次一条，每条一个**独立事务**。
// 实测单条落库 **296µs/行**；把 N 行并进一个事务后，批大小 50 时只需
// **39µs/行（7.6×）**。差的是每次独立事务的固定开销（commit）。
//
// 写池是单连接（SetMaxOpenConns(1)），于是每个成功请求都要付一次那个
// 固定开销 —— 这就是「吞吐被落库钉死」那个 P1 的直接成因：
// 吞吐被钉在落库速率上（实测仅 INSERT 时 3693 req/s）。
//
// 现在改成：**攒够一批（或队列已排空）就一个事务写入**。
// 计价仍**逐条**由 store 的 freezeUsageCost 算（批量只省提交开销，
// **不改计价语义**），所以「扣的 = 报表的」这条不变量逐字不变。
//
// # 改后的事务边界
//
// 一批 N 条 = **一个事务**（一条 N 行多值 INSERT）。
// 失败时**整批回滚**，store 返回**全 0 费用**，调用方据此**整批不扣费**。
// 这与单条路径的「落库失败 → 不扣费」口径完全一致：账记不下来时不收钱。
//
// 扣费**不跟着批量**，仍是逐条一个事务 —— 理由：
// 幂等键 (request_id, user_id) 是逐条的，聚合扣费会让「哪几笔算已扣过」
// 变成需要额外状态才能回答的问题，错一次就是漏收或重复收费。
func (u *Recorder) worker() {
	defer u.wg.Done()

	// batch 是复用缓冲区：高负载下每秒数千批，每批都 make 一次会直接
	// 变成 GC 压力（而 GC 压力正是我们想减掉的东西）。
	batch := make([]*store.UsageRecord, 0, usageBatchSize)

	// stillPending 是**上一轮落库失败、留待重试**的记录（2026-10-12 修 P1-1）。
	//
	// 为什么留在 worker 手里而不是插回队列：队列是有界的（usageQueueSize=1024）
	// 且此刻可能已满，非阻塞插回会在 `default` 分支静默丢弃 —— 那正是本次
	// 要修的缺陷本身。留在手上则保证「不丢」，代价是这批重试期间不再取新记录，
	// 让入队端自然背压（record 里的 select 会走 default 同步写）。
	//
	// 顺序：上一批重试成功后才继续取新记录，保证「先失败的先落库」。
	var stillPending []*store.UsageRecord

	for {
		// 上一轮失败的记录优先重试（仍在 stillPending 里）。
		if len(stillPending) > 0 {
			batch = append(batch[:0], stillPending...)
			stillPending = stillPending[:0]
			if u.flush(ctxBackground(), batch) {
				// 全部落库成功，继续取新记录。
			} else {
				// 又失败：留到下一轮，且**不再从队列取新记录**，
				// 免得失败批越攒越多、最终占满整个队列。
				//
				// *X0 配套修复：这一分支原来只 markFlushFailed 就 continue，
				// 而上面已经把 stillPending 清空、记录只在 batch 里；下一轮
				// 开头用空的 stillPending 覆盖 batch，整批就静默丢了。
				// 队列关闭后 worker 还会直接走「无待处理记录」的退出路径。
				// 必须在 continue 前把整批放回 stillPending。
				stillPending = append(stillPending[:0], batch...)
				u.markFlushFailed()
				if !u.retryPause(usageFlushRetryDelay) {
					u.logPendingAbandoned(stillPending)
					return
				}
				continue
			}
		}

		// 取第一条：**阻塞**读。关闭时 channel 被 close，这里返回 !ok，
		// 冲掉手上这批再退出（关停 drain 的全部意义就是别弄丢已发生的调用）。
		rec, ok := <-u.queue
		if !ok {
			// 队列已关。仍要冲掉手上的 stillPending —— 那批已经出队过，
			// 留在这里就真的丢了（关停 drain 的意义正在于此）。
			if len(stillPending) > 0 {
				batch = append(batch[:0], stillPending...)
				stillPending = stillPending[:0]
				u.flush(ctxBackground(), batch)
			}
			return
		}
		batch = append(batch, rec)

		// 尽力多攒：把**当前已就绪**的记录挪进这批。
		//
		// 用 `default` 而不是阻塞读：阻塞会把「攒批」变成「每个请求等一批」，
		// 尾延迟会变成 N×批间隔，低负载时尤其难看。这里要的是
		// 「能攒就攒，攒不到就立刻发」—— 低负载时批自然很小（退化成单条），
		// 而低负载本来就不需要吞吐优化，**低延迟更重要**。
	drain:
		for len(batch) < usageBatchSize {
			select {
			case r2, ok2 := <-u.queue:
				if !ok2 {
					break drain
				}
				batch = append(batch, r2)
			default:
				break drain
			}
		}

		if u.flush(ctxBackground(), batch) {
			u.clearFlushFailed()
		} else {
			// 整批（含降级逐条）仍有失败 → 把整批留作下一轮重试。
			// 已成功的部分下次会被幂等键挡掉，不会重复计费。
			stillPending = append(stillPending[:0], batch...)
			u.markFlushFailed()
			if !u.retryPause(usageFlushRetryDelay) {
				u.logPendingAbandoned(stillPending)
				return
			}
		}
		batch = batch[:0]
	}
}

// retryPause 等待一段退避时间；stop 关闭时立刻返回 false。
// 返回 false 表示关停已经放弃 drain，调用方应记录未落库记录后退出。
func (u *Recorder) retryPause(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-u.stop:
		return false
	}
}

// abort 让仍在重试退避中的 worker 退出。仅由关停流程在 drain 超时后调用。
// 幂等：多次调用只关一次 channel。
func (u *Recorder) Abort() {
	u.stopOnce.Do(func() { close(u.stop) })
}

// Done 在全部 worker 退出后关闭。
func (u *Recorder) Done() <-chan struct{} { return u.done }

// Store returns the store used by the recorder.
func (u *Recorder) Store() *store.Store { return u.db }

// Logger returns the recorder's structured logger.
func (u *Recorder) Logger() *slog.Logger { return u.logger }

// logPendingAbandoned 把放弃 drain 时仍留在 worker 手里的记录打成一条
// 可对账的 ERROR。逐条 "usage record lost" 只在单条写失败时出现；
// 关停超时可能发生在整批重试阶段，那些记录还没有逐条尝试过，若不在这里
// 汇总，日志里就只剩下一句「records were lost」，无法知道丢了哪几次调用。
func (u *Recorder) logPendingAbandoned(recs []*store.UsageRecord) {
	if len(recs) == 0 {
		return
	}
	const maxSample = 10
	sample := make([]string, 0, maxSample)
	for _, rec := range recs {
		if len(sample) == maxSample {
			break
		}
		sample = append(sample, rec.RequestID)
	}
	u.logger.Error("usage recorder aborted with unpersisted records",
		"count", len(recs), "request_id_sample", sample,
		"hint", "persistent write failure or shutdown deadline; reconcile these request_ids manually")
}

// flush 把一批记录落库，然后**逐条**扣费。
//
// 落库是批量的（省提交开销），扣费保持逐条（幂等键逐条不同，聚合风险高
// —— 见 worker 的注释）。
//
// # 失败语义（2026-10-12 修 P1-1）
//
// 原实现遇错只打一条 ERROR 就 `return` —— 记录**已从队列出队**，不重试、
// 不落补偿队列，整批最多 50 条用量与扣费静默丢失。后果：这些请求既不入报表、
// 不计配额、也不扣余额，**用户实际消耗了上游 token 却完全免单**，
// 且配额被永久放宽。触发只需一次写池故障（SQLITE_BUSY / 磁盘抖动），
// **不需要攻击者**。
//
// 现在分两级处置：
//
//  1. **整批重试**（usageFlushMaxAttempts 次，固定退避）——针对瞬时故障；
//  2. 仍失败则**降级逐条**，把爆炸半径从「整批 50 条」缩到「单条」。
//
// 逐条仍失败的那条才真正丢弃，且此时打的是带 request_id 的 ERROR，
// 至少能在日志里追查到具体是哪次调用漏了账（原来只有一条不含 ID 的批级日志，
// 事后根本无从对账）。
//
// 为什么不干脆永久回灌重试：持久性故障（磁盘满、库损坏）下无限重试会让
// 队列只进不出、最终把 1024 的容量占满，反而堵死后续所有记录的入队 ——
// 那是把「丢一批」换成「全丢」。降级逐条 + 单条计数告警是更稳的取舍。
//
// 返回值报告「是否全部落库成功」，供 worker 决定要不要把这批插回队头。
func (u *Recorder) flush(ctx context.Context, batch []*store.UsageRecord) bool {
	if len(batch) == 0 {
		return true
	}

	// 批量写入走注入点（测试可替换；生产为 nil 时用默认实现）。
	insert := u.batchInsert
	if insert == nil {
		insert = u.db.CreateUsageRecordsBatched
	}

	costs, err := insert(ctx, batch)
	for attempt := 1; err != nil && attempt < usageFlushMaxAttempts; attempt++ {
		u.logger.Warn("usage batch flush failed, retrying",
			"error", err, "attempt", attempt, "max", usageFlushMaxAttempts,
			"count", len(batch), "key_id", batch[0].AccessKeyID)
		select {
		case <-ctx.Done():
			// 关停路径上不该再等退避：直接走降级，把能落的落掉。
			attempt = usageFlushMaxAttempts
		case <-time.After(usageFlushRetryDelay):
		}
		costs, err = insert(ctx, batch)
	}

	if err != nil {
		// 整批仍失败 → 降级逐条。写池是单连接，逐条本就是批量之前的老路径，
		// 正确性早已被验证；这里只是把它当作**降级手段**而非常规路径。
		u.logger.Error("usage batch flush failed after retries, degrading to per-record",
			"error", err, "count", len(batch))
		return u.flushOneByOne(ctx, batch)
	}

	for i, rec := range batch {
		var cost float64
		if i < len(costs) {
			cost = costs[i]
		}
		u.charge(rec, cost)
	}
	return true
}

// flushOneByOne 逐条落库，作为整批失败后的降级路径（2026-10-12 修 P1-1）。
//
// 逐条各自独立事务，所以「一条失败」只影响那一条，不再连坐整批 ——
// 这正是降级要买的保险。
//
// 返回值报告**是否全部成功**：有任何一条失败就返回 false，worker 据此
// 把整批插回队头重试（宁可多试一遍，也不要让失败的那条无声消失）。
//
// **重试必须能认出「这条已经写过了」**。usage_records 的幂等键是记录主键
// id（同一个 *UsageRecord 指针重试时 id 不变），而 request_id 上并没有唯一
// 约束 —— 旧注释写反了。认不出主键冲突时，一次「批量失败 → 逐条降级 →
// 部分成功」的路径会变成永久活锁：批量重试因已存在的行整体回滚，
// 逐条重试又把这些行继续判为失败，那批记录再也冲不出去，
// worker 停在 stillPending 分支不再消费队列，之后每条用量都退化到
// 请求路径上的同步写并大概率丢失（配额不涨、余额不扣）。
func (u *Recorder) flushOneByOne(ctx context.Context, batch []*store.UsageRecord) bool {
	allOK := true
	for _, rec := range batch {
		cost, err := u.db.CreateUsageRecordWithCost(ctx, rec)
		if err != nil {
			if !store.IsUniqueViolation(err) {
				allOK = false
				// 带 request_id —— 原来的批级日志不含它，事后无从对账。
				u.logger.Error("usage record lost (per-record write also failed)",
					"error", err, "request_id", rec.RequestID,
					"key_id", rec.AccessKeyID, "user_id", rec.UserID)
				continue
			}
			// 主键冲突：这条**可能**已经落库了。
			//
			// 旧实现在这里无条件 continue（跳过 charge），理由是
			// 「已落库 ⇒ 已扣费」。这个推理**不成立**（2026-10-21 修 A4-P1-5）：
			//   - 批路径在 tx.Commit() 返回 I/O 错误时可能**已经提交**了；
			//     flush 判失败 → 降级逐条 → 每条都撞主键冲突 → 全部跳过
			//     → 这一批全部落库了（报表有费用）而余额一分没扣；
			//   - 更简单的可达路径：批路径因 rec.ID 重复而失败时，
			//     库里根本没这批记录，逐条写时每条都冲突 → 全部跳过
			//     → 全部不扣费。
			//
			// 幂等责任本来就该由 balance_charges 的 (request_id, user_id)
			// 承担 —— ChargeBalance 自己是幂等的（占位行命中即返回，
			// 不碰余额）。所以这里**继续走 charge**：重复调用是空操作，
			// 而漏调则是永久漏收。
			//
			// 费用从库里读回：这条已经落库，cost_total 是**已固化**的值，
			// 不重算（重算会按当前价，与「固化于落库时」的口径冲突）。
			// 读不到就按 0 处理 —— 那是「取不到」而不是「算错了」。
			cost = u.recordedCost(ctx, rec)
			u.logger.Warn("usage record already existed; reconciling charge",
				"request_id", rec.RequestID, "key_id", rec.AccessKeyID,
				"cost_yuan", cost)
		}
		u.charge(rec, cost)
	}
	return allOK
}

// recordedCost 读回一条**已落库**用量的固化费用（元）。
//
// 只在「主键冲突 = 这条可能已经落库」时用：此时 CreateUsageRecordWithCost
// 返回的 cost 是零值（写入失败了），而我们要扣的钱是**库里那个已固化的值**。
//
// 读不到（记录其实没落库、已被剪枝、或读池抖动）时返回 0：
// 0 元在扣费侧是完全 no-op（见 ChargeBalance 的注释），而「取不到」与
// 「算出 0」在账上的后果相同 —— 都不扣钱。相比之下猜一个数去扣，
// 那是「按编出来的数收用户钱」。
func (u *Recorder) recordedCost(ctx context.Context, rec *store.UsageRecord) float64 {
	if rec == nil || rec.ID == "" {
		return 0
	}
	var cost float64
	err := u.db.DB().QueryRowContext(ctx,
		`SELECT COALESCE(cost_total, 0) FROM usage_records WHERE id = ?`, rec.ID).Scan(&cost)
	if err != nil {
		u.logger.Error("cannot read back cost_total for an already-recorded usage row",
			"error", err, "record_id", rec.ID, "request_id", rec.RequestID)
		return 0
	}
	return cost
}

// charge 对一条**已落库**的用量记录扣余额。cost 是本次落库时固化的费用（元），
// 由 CreateUsageRecordWithCost 给出 —— 它与 cost_total 列是同一个值。
//
// # 只对成功的请求扣费（Lead 与用户确认的第四条口径）
//
// 判据是 usage_records.status == "ok"。它是「这一次调用真的成功」的既有
// 事实来源，attemptStream/attemptNonStream 落库时就定好了，且**与熔断计数的
// 判据是同一个**（见 dataplane.Outcome.Success 的注释：Committed 只说明字节已
// 写出，断流/溢出也是 committed 却是失败）。用同一个判据，余额与健康度不会
// 各说各话。
//
// 明确**不扣**的形态（它们都不该收用户钱）：上游错误与不可转移错误、
// 流式截断/溢出/中断、客户端主动断开（canceled）、以及根本没碰上游就被拒的
// 请求（限流、配额、余额预检自身、model_not_found）。后者压根不会走到这里
// —— 没有 usage 记录可扣。
//
// 与 cost_total 的差别要记牢：cost_total **无论成败都记**（那是网关的上游
// 成本），扣费**只看成功**（那是用户的应收）。同一次调用两个数相同、触发条件
// 不同。见 billing.go 的口径说明。
//
// # 与 dataplane.RateCommit 收尾的关系（两者互不干扰）
//
// RateCommit 管的是**预占**（TPM 窗口 + 终身 token 配额），它必须在请求
// 结束那一刻收尾（流式是边写边收）。扣费管的是**实收**，它挂在 usage 落库
// 之后、异步发生。两者没有共享状态：预占退回的是"还没花的额度"，扣费收的是
// "已经花的钱"，方向相反不会互相抵消。
//
// usage missing（上游没报 token 数）时 cost_total 记 0 → 扣费也是 0
// （store.ChargeBalance 对 0 元 no-op）。这与该路径"只退配额预占、保留 TPM
// 预占"的既有决策不冲突：后者是终身累计不退就永久泄漏，前者随窗口翻转自愈，
// 而我们收 0 元本来就是正确结果（不知道实际花了多少，就不该收钱）。
func (u *Recorder) charge(rec *store.UsageRecord, costYuan float64) {
	// 管理员短路（纵深防御：数据面上不可达，见 billing.go 的 balanceExempt）。
	//
	// 2026-10 控制面/数据面分离后，管理员在 auth.Authenticate 就被拒
	// （ErrAdminCannotCallModel），压根不会有管理员的 usage 落到这里。保留
	// 这道短路是纵深防御：余额是「钱」，万一某条路径绕过 auth 送来一条管理员
	// 的 usage 记录，这里立刻 no-op，不去碰他的余额。判据不依赖任何 DB 状态、
	// 只读快照，代价为零。
	if u.isBalanceExempt(rec.UserID) {
		return
	}
	// 非成功不扣：见本函数「只对成功的请求扣费」。
	if rec.Status != usageStatusOK {
		return
	}
	// 空 request_id → 没有幂等键，扣费**不做**（宁可漏扣不可重复扣）。
	//
	// 生产里 request_id 由 server.Middleware 每个请求生成，必非空；为空
	// 意味着有代码路径绕过了中间件，那属于异常而非常态。这里记 ERROR
	// 而不是静默跳过 —— 漏扣是一次没人发现的钱（对账才看得出），而
	// 无幂等的扣费会在重复收尾时**真的扣第二次**，用户立刻就会投诉。
	// 两害相权取轻，但绝不无声无息。
	if rec.RequestID == "" {
		u.logger.Error("usage record has empty request_id, skipping charge (no idempotency key)",
			"user_id", rec.UserID, "access_key_id", rec.AccessKeyID,
			"public_model", rec.PublicModel, "cost_cents", store.YuanToCents(costYuan))
		return
	}
	// 元 → 分走 store 的 YuanToCents（唯一实现）：预检侧估的与这里扣的
	// 必须用同一个换算，否则「预检以为够、实际差一分钱」会成为常态。
	//
	// 传给 ChargeBalance 的是**元**而不是分（2026-10-10）：单价可能远低于
	// 一分（实测 price_input=3.0 元/百万时，一次 8000 token 的调用只有
	// 0.004 元），按分取整就是 0，而 0 分在扣费侧是 no-op —— 一次都扣不到钱。
	// 余数的累计与折算都由 store 内部完成，这里只负责把「这一次实际花了
	// 多少元」这个已经固化在 cost_total 里的数字送过去。
	err := u.db.ChargeBalance(context.Background(), rec.UserID, costYuan, rec.RequestID)
	switch {
	case err == nil:
		return
	case errors.Is(err, store.ErrInsufficientBalance):
		// 预检在前，正常路径走不到这里。走到 = 并发透支或预检漏了。
		// 此时上游已被调用、费用已固化进 cost_total，钱收不回，
		// 「报欠费」比「悄悄放过」诚实。响应早已写出，改不了，只能记 ERROR。
		//
		// 欠费**不重试**：它不是瞬时故障，重试一万次也不会成功。
		// ChargeBalance 已经把金额记进 balance_pending 待结算账本
		// （见该函数的欠费分支），用户充值后会被结转 —— 这条路径是通的。
		u.logger.Error("charge failed: insufficient balance (precheck should have prevented this)",
			"user_id", rec.UserID, "request_id", rec.RequestID,
			"access_key_id", rec.AccessKeyID, "public_model", rec.PublicModel)
		return
	default:
		// 瞬时故障（DB 抖动、磁盘忙、连接被掐）—— **必须重试**
		// （2026-10-21 修 A4-P1-6）。
		//
		// 旧实现在这里只记一条 ERROR 就结束：一次 DB 抖动 = 钱款**永久漏收**，
		// 而记录看起来完全正常（usage_state='reported'、cost_total 非 0），
		// 事后对账都无法从数据本身发现 —— 因为「该扣多少」与「扣了没有」
		// 分处两处，而后者没有落盘的痕迹。
		//
		// 重试为什么是安全的：ChargeBalance 自己幂等（balance_charges 的
		// (request_id, user_id) 占位行），重复调用不会重复扣。
		// 所以重试的代价只是几次 IO，收益是「漏收」变成「延迟几百毫秒收到」。
		u.retryCharge(rec, costYuan, err)
	}
}

// retryCharge 对一次失败的扣费做有限次重试。
//
// # 为什么不无限重试
//
// 扣费发生在 usage 落库之后的**异步**路径上，而这条路径不能拖住 worker：
// worker 卡住 = 整条用量队列停摆 = 之后的每一次调用都不记账。
// 所以重试必须有界，且总耗时必须短（几百毫秒量级）。
//
// # 为什么重试几乎必然能成功
//
// ChargeBalance 幂等，所以重试是安全的；而它失败的原因绝大多数是
// 瞬时的（写锁竞争、磁盘忙、连接被掐）。有界重试足以覆盖这类。
//
// # 真的重试完还是失败怎么办
//
// 记 ERROR，并且把 request_id 记全 —— 这是「这笔钱没收回来」的唯一线索。
// 运维据此对账。**不能**静默：静默的漏收是本次修复要消灭的东西。
func (u *Recorder) retryCharge(rec *store.UsageRecord, costYuan float64, cause error) {
	const attempts = 3
	for i := 1; i <= attempts; i++ {
		// 退避：第一次立刻重试（抖动常常是瞬时的），之后各等一小段。
		// 用 time.Sleep 而不是 time.AfterFunc：这是 worker 路径，
		// 阻塞几十毫秒是可接受的（批落库本身就要几毫秒）。
		if i > 1 {
			time.Sleep(time.Duration(i-1) * 20 * time.Millisecond)
		}
		err := u.db.ChargeBalance(context.Background(), rec.UserID, costYuan, rec.RequestID)
		if err == nil {
			if i > 1 {
				u.logger.Warn("balance charge succeeded after retry",
					"attempts", i, "user_id", rec.UserID, "request_id", rec.RequestID)
			}
			return
		}
		if errors.Is(err, store.ErrInsufficientBalance) {
			// 欠费不是瞬时故障，重试无意义。已经在上面记过 ERROR 了。
			return
		}
		cause = err
	}
	u.logger.Error("failed to charge balance after retries; this amount was NOT collected",
		"error", cause, "attempts", attempts,
		"user_id", rec.UserID, "request_id", rec.RequestID,
		"access_key_id", rec.AccessKeyID, "public_model", rec.PublicModel,
		"cost_cents", store.YuanToCents(costYuan),
		"reconcile_hint", "该笔应收未实收；对账时按 request_id 核对 balance_charges 是否缺行")
}

// record 排队一条异步落库，立即返回。
//
// 队列满时退化为同步写：宁可让当前请求多等一次 SQLite 写，也不丢用量记录。
// 同步写失败同样记日志，与异步路径口径一致。
//
// 扣费在两条路径上**都必须发生**，所以同步回退里那行 CreateUsageRecord 后面
// 紧跟着 u.charge(rec)，与 worker 里完全一致。漏掉的后果是「队列一满，
// 从此之后所有请求都不扣费」—— 一个只在高压时才出现、且不会有任何报错的
// 漏收，正是 CreateUsageRecord 注释里说的「把'别忘了算'变成可以各自忘记的
// 地方」。这里刻意不抽公共函数：两条路径的收尾已经足够短，抽出去反而让
// 「它们是同一件事」这件事看不出来。
func (u *Recorder) Record(rec *store.UsageRecord) {
	// 「判关闭 + 入队」必须在同一把锁里完成，否则会与 wait 的 close 竞争，
	// 产生 send on closed channel（见 mu 的注释）。锁外的同步写只碰 DB。
	u.mu.Lock()
	if !u.closed {
		select {
		case u.queue <- rec:
			u.mu.Unlock()
			return
		default:
			// 队列已满 —— 落到下面同步写（背压）。
		}
	}
	u.mu.Unlock()

	// 到这里有两种可能：队列满（背压），或队列已关闭（关停收尾）。
	// 后者仍走同步写而不是丢弃：这条记录对应的是一次**已经发生的调用**，
	// 而关停 drain 的全部意义就是别把它弄丢。
	cost, err := u.db.CreateUsageRecordWithCost(context.Background(), rec)
	if err != nil {
		u.logger.Error("failed to record usage (sync fallback)", "error", err, "key_id", rec.AccessKeyID)
		return
	}
	u.charge(rec, cost)
}

// markFlushFailed / clearFlushFailed / HasPendingUsage 记录「是否还攥着
// 一批没落库成功的记录」（2026-10-12 修 P1-1）。
//
// 用途有两个：
//  1. worker 的重试循环据此决定要不要继续 sleep 重试；
//  2. wait 的调用方（关停流程）据此判断「drain 是否真的干净了」——
//     原来 flush 失败就静默 return，wait 永远以为排空了，
//     于是日志上一片祥和、实际丢了最多 50 条计费记录。
func (u *Recorder) markFlushFailed() {
	u.mu.Lock()
	u.flushFailed = true
	u.mu.Unlock()
}

func (u *Recorder) clearFlushFailed() {
	u.mu.Lock()
	u.flushFailed = false
	u.mu.Unlock()
}

// HasPendingUsage 报告是否还有未落库成功的记录。仅供关停流程与测试。
func (u *Recorder) HasPendingUsage() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.flushFailed
}

// wait 关闭队列、等 worker 把在途写入全部完成，或 ctx 到期（到期即放弃，
// 不让一个卡住的 SQLite 写把进程关停拖成无限期）。
//
// closed 标志与 close **同处一把锁**：这是 record 判据与 close 动作的配对点。
// 两者分开就又出现「record 已判完未关闭、close 恰好插进来」的窗口。
//
// 返回值报告「是否真的排空」：false 表示仍有记录未落库（flush 反复失败）。
// 调用方（main 的关停流程）应当据此打一条 ERROR —— 那是**真丢了计费记录**，
// 不能像原来那样无声无息。
func (u *Recorder) Wait(ctx context.Context) bool {
	u.mu.Lock()
	u.closed = true
	close(u.queue)
	u.mu.Unlock()

	done := make(chan struct{})
	go func() {
		u.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// 超时放弃：可能 worker 还在重试循环里。此时 flushFailed 仍为 true，
		// 返回 false 让调用方知道「这批没落干净」。
	}
	return !u.HasPendingUsage()
}
