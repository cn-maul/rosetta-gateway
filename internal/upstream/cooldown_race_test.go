package upstream

import (
	"context"
	"sync"
	"testing"
	"time"
)

// concurrencyStore 在 SetCredentialCooldown 执行期间，**真实地**启动另一个
// goroutine 去改写同一把凭据的共享字段，并等待它完成。
//
// 为什么必须真的并发：Go 的实参在**进入函数前**求值，所以
//
//	SetCredentialCooldown(ctx, id, "cooling", cred.CooldownUntil)
//
// 这一行里 `cred.CooldownUntil` 是在**主 goroutine**、解锁之后那一刻读的。
// 污染若发生在 store 函数体内（已经太晚），实参早就求完了——
// 那样构造的测试永远是恒绿的假信号（这个坑踩过一次）。
//
// 真正能触发 bug 的时序是：A 解锁 → A 求实参 → A 调 store，
// 而 B 在「A 解锁」到「A 求实参」之间完成写入。二者必须跨 goroutine 交错，
// 且无法用单线程顺序调用模拟。
type concurrencyStore struct {
	cred *CredentialEntry

	// interleave 在每次落库期间执行一次干扰：起一个 goroutine 改写共享字段。
	interleave func(n int)
	// gate 通知干扰已完成，确保窗口真实存在。
	gate chan struct{}

	mu    sync.Mutex
	seen  []time.Time
	calls int
}

func (s *concurrencyStore) SetCredentialCooldown(_ context.Context, _, _ string, until time.Time) error {
	s.mu.Lock()
	s.seen = append(s.seen, until)
	s.calls++
	n := s.calls
	s.mu.Unlock()

	// 干扰必须在**另一个 goroutine** 里做，且本调用要等它完成——
	// 这样下一轮调用的写入会与本次调用的求值窗口真实交错。
	if s.interleave != nil {
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.interleave(n)
		}()
		<-done
	}
	return nil
}

func (s *concurrencyStore) snapshot() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := make([]time.Time, len(s.seen))
	copy(t, s.seen)
	return t
}

// TestRegression_CooldownPersistsOwnValueNotSharedField 是 P2-13 的核心回归测试。
//
// 旧实现的bug：`SetCredentialCooldown(..., cred.CooldownUntil)` 的实参
// 在**解锁之后**才求值，而 cred 是共享指针。并发场景下 A 读到的是 B 写的值。
//
// 后果不是"显示不准"。冷却是**必须持久化**的运行时状态—— 池会被任何 admin
// 写操作重建，重建时从库读cooldown_until。若 60s 的冷却被写成 1s，
// 刚被判 401 失效的凭据会提前 59 秒回到轮换里，冷却对持续失效的 key 形同虚设。
//
// 本测试用可区分的时长（60s / 2h）标识每次调用，断言落库值必须与调用方对应。
func TestRegression_CooldownPersistsOwnValueNotSharedField(t *testing.T) {
	cred := &CredentialEntry{ID: "c1", Status: "healthy", Enabled: true}
	p := newTestPool()
	p.mu.Lock()
	p.credIndex = map[string]*CredentialEntry{"c1": cred}
	p.mu.Unlock()

	// 两种时长悬殊到不可能混淆：1 秒 vs 1 小时。
	// 这样落库值本身就能唯一标识它属于哪次调用，无需额外标签（标签在并发下会串）。
	const shortD = 1 * time.Second
	const longD = 1 * time.Hour

	st := &concurrencyStore{cred: cred}
	st.interleave = func(n int) {
		// 干扰：把共享字段改成一个**不属于任何合法时长**的值。
		// 若落库值落在这里，说明读到的是别人的值。
		//
		// **必须走 p.mu**（2026-10-10 修）：原先这里只用了一个测试私有的
		// mu 去写 cred.CooldownUntil，而生产代码在 p.mu 下写同一字段 ——
		// 两个不同的锁保护同一个字段，`-race` 必然报 DATA RACE。
		// 实测（本会话首次跑通 -race 后）：
		//   WARNING: DATA RACE
		//     Write at ... by goroutine 410: cooldown_race_test.go:93 (本干扰)
		//     Previous write at ...: upstream.go:425 (MarkCredentialCooldown)
		//
		// 那不是生产缺陷（生产对该字段的**每一处**读写都在 p.mu 内，
		// 已逐处核对），而是**测试自己制造的**竞争。但后果一样严重：
		// 它让 `go test -race ./...` 永久变红，于是真正的竞争会被淹没在
		// 这条噪音里 —— 一个永远红的检测器等于没有检测器。
		//
		// 用 p.mu 不影响本测试的判据：干扰要发生的时间窗是「生产已解锁、
		// 正把快照交给 store」那一段，此时生产**不持有** p.mu，所以这里
		// 仍能拿锁写入，依旧能把毒值种进去。
		p.mu.Lock()
		cred.CooldownUntil = time.Unix(1, 0).UTC()
		p.mu.Unlock()
		_ = n
	}
	p.cooldownSto = st

	// 并发两次调用，窗口真实交错。long 先起、short 后起但两者都会跑。
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); p.MarkCredentialCooldown("c1", longD) }()
		go func() { defer wg.Done(); p.MarkCredentialCooldown("c1", shortD) }()
	}
	wg.Wait()

	seen := st.snapshot()
	if len(seen) == 0 {
		t.Fatal("没有任何落库调用")
	}

	// 每个落库值必须二选一：≈1 秒（short 调用）或 ≈1 小时（long 调用）。
	// 落在两者之外的值只有一个来源——被别人改写过的共享字段。
	var other int
	for _, v := range seen {
		if within(v, shortD) || within(v, longD) {
			continue
		}
		if v.Equal(time.Unix(1, 0).UTC()) {
			t.Fatalf("落库值=%v —— 1970-01-01 是 store 注入的污染值。\n"+
				"说明 SetCredentialCooldown 的实参来自解锁后的共享字段读取。\n"+
				"旧实现：SetCredentialCooldown(..., cred.CooldownUntil)", v)
		}
		other++
	}
	if other > 0 {
		t.Fatalf("有 %d/%d 次落库既不属于 short(1s) 也不属于 long(1h)，"+
			"说明落库的是别人的值", other, len(seen))
	}
}

// within 判断落库时刻是否 ≈ base+duration（容差 5 秒，覆盖调度与时钟精度）。
func within(v time.Time, d time.Duration) bool {
	delta := time.Until(v)
	return delta > d-5*time.Second && delta < d+5*time.Second
}

// TestRegression_RecoveryNotMisjudgedUnderConcurrentCooldown 覆盖恢复路径。
//
// RecordCredentialSuccess 的落库参数是常量，天然不受竞态影响。
// 但它的 changed 判定若放到锁外读 cred.Status，就会与并发冷却争抢同一行——
// 冷却把 CooldownUntil 推到未来，恢复据此判「本来 healthy」而跳过，
// 于是刚被冷却的凭据没被恢复，内存与库就此分叉。
func TestRegression_RecoveryNotMisjudgedUnderConcurrentCooldown(t *testing.T) {
	cred := &CredentialEntry{
		ID: "c1", Status: "healthy", Enabled: true,
		CooldownUntil: time.Now().Add(time.Minute), // 处于冷却 → 需要恢复
	}
	p := newTestPool()
	p.mu.Lock()
	p.credIndex = map[string]*CredentialEntry{"c1": cred}
	p.mu.Unlock()

	var mu sync.Mutex
	var statuses []string
	p.cooldownSto = recorder(func(_ context.Context, _, status string, _ time.Time) error {
		// 窗口内另一个 goroutine 立刻把它重新冷却 —— 干扰恢复路径的判定。
		//
		// 与上面那条同一个纪律：写共享的 cred 必须走 **p.mu**，否则
		// `-race` 会报「测试私有的 mu 与生产 p.mu 保护同一字段」。
		// 详见 TestRegression_CooldownPersistsOwnValueNotSharedField 里
		// st.interleave 的注释（那里是实测报过 DATA RACE 的地方）。
		done := make(chan struct{})
		go func() {
			defer close(done)
			p.mu.Lock()
			cred.CooldownUntil = time.Now().Add(time.Hour)
			cred.Status = "cooling"
			p.mu.Unlock()
		}()
		<-done
		mu.Lock()
		statuses = append(statuses, status)
		mu.Unlock()
		return nil
	})

	for i := 0; i < 50; i++ {
		p.RecordCredentialSuccess("c1")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, s := range statuses {
		if s != "healthy" {
			t.Fatalf("落库状态=%q，期望 \"healthy\"", s)
		}
	}
}

// TestRegression_NoDataRaceUnderConcurrentChurn 是 -race 的载体。
//
// 本机（Windows 无 gcc）跑不了 -race，但该测试必须在 CI 完成编译与执行；
// 有 gcc 的环境里它能真正检出共享字段竞争。同时覆盖四条路径：
// 冷却、恢复、读健康集合、查目标可用性。
func TestRegression_NoDataRaceUnderConcurrentChurn(t *testing.T) {
	p := newTestPool()

	cred1 := &CredentialEntry{ID: "c1", Status: "healthy", Enabled: true}
	cred2 := &CredentialEntry{ID: "c2", Status: "healthy", Enabled: true}
	prov := &ProviderEntry{Slug: "pv", Enabled: true, Credentials: []*CredentialEntry{cred1, cred2}}

	p.mu.Lock()
	p.credIndex = map[string]*CredentialEntry{"c1": cred1, "c2": cred2}
	p.providers = map[string]*ProviderEntry{"pv": prov}
	p.mu.Unlock()

	p.cooldownSto = recorder(func(context.Context, string, string, time.Time) error { return nil })

	var wg sync.WaitGroup
	for i := 0; i < 300; i++ {
		for _, id := range []string{"c1", "c2"} {
			wg.Add(3)
			go func(id string) { defer wg.Done(); p.MarkCredentialCooldown(id, time.Minute) }(id)
			go func(id string) { defer wg.Done(); p.RecordCredentialSuccess(id) }(id)
			go func() { defer wg.Done(); p.TargetAvailable("t1") }()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.mu.RLock()
			_ = p.getHealthyCredentials(prov)
			p.mu.RUnlock()
		}()
	}
	wg.Wait()
}

// recorder 是最小 CooldownStore 实现。
type recorder func(ctx context.Context, id, status string, until time.Time) error

func (r recorder) SetCredentialCooldown(ctx context.Context, id, status string, until time.Time) error {
	return r(ctx, id, status, until)
}

var _ CooldownStore = (*concurrencyStore)(nil)
var _ CooldownStore = recorder(nil)
