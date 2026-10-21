package routing

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestRouteIndex_ConcurrentReadWriteDoesNotCrash 坐实 *X15 的修法。
//
// # 这个测试在修之前会怎样
//
// 修之前 Add* 无锁，而它们是**导出**方法 —— snapshot 的「已发布快照不可变」
// 只是注释约定，没有任何机制阻止某个包在 Swap 之后继续往里写。一旦发生，
// 与热路径的 Resolve 并发时就是**并发 map 读写**，而 Go 运行时对此的处理是
// 直接 fatal error（不是 panic，recover 拦不住）—— 整个进程当场消失，
// 所有在途请求一起断。
//
// 现在 Add* 与所有读路径共用一把 RWMutex，并发读写被串行化。
//
// 注意：`go test -race` 是这里的关键判据。没有锁时它会报
// "concurrent map writes" / "concurrent map read and map write"。
func TestRouteIndex_ConcurrentReadWriteDoesNotCrash(t *testing.T) {
	ri := NewRouteIndex()

	// 先放一条可解析的路由，读 goroutine 才有东西可读。
	ri.AddProvider(&ProviderRef{ID: "p0", Slug: "alpha", Enabled: true})
	ri.AddUpstreamModel(&UpstreamModel{ID: "m0", ProviderID: "p0", ModelID: "chat", Enabled: true})
	ri.AddRoute(&Route{ID: "r0", PublicName: "flash", ProviderID: "p0", UpstreamModelID: "m0", Enabled: true})

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 读路径：覆盖全部公开读方法 —— ListRoutes / ListPublicNames /
	// ListUpstreamModels / Resolve / FindUpstreamModelByModelID。
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if res, err := ri.Resolve("flash"); err == nil && len(res.Candidates) == 0 {
					t.Error("Resolve 返回了空候选链")
					return
				}
				_ = ri.ListRoutes()
				_ = ri.ListPublicNames()
				_ = ri.ListUpstreamModels()
				_ = ri.FindUpstreamModelByModelID("p0", "chat")
			}
		}()
	}

	// 写路径：与读路径并发地往索引里灌新路由。
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				id := "p" + strconv.Itoa(base) + "-" + strconv.Itoa(i)
				ri.AddProvider(&ProviderRef{ID: id, Slug: "slug-" + id, Enabled: true})
				ri.AddUpstreamModel(&UpstreamModel{
					ID: "m" + id, ProviderID: id, ModelID: "model-" + id, Enabled: true,
				})
				ri.AddRoute(&Route{
					ID: "r" + id, PublicName: "pub-" + id,
					ProviderID: id, UpstreamModelID: "m" + id, Enabled: true,
				})
				ri.AddRouteTarget(&Target{
					ID: "t" + id, RouteID: "r" + id,
					ProviderID: id, UpstreamModelID: "m" + id, Position: 0, Enabled: true,
				})
			}
		}(w)
	}

	// 跑一小会儿即够：并发 map 读写在有锁的情况下一次都不会发生，
	// 而无锁时几十次迭代内几乎必然触发。
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestRouteIndex_ResolveNeverReturnsHalfBuiltChain 钉住 *X15 真正会
// 出事的那条路径。
//
// # 为什么盯 Resolve 而不是盯「两次枚举条数相等」
//
// 「ListRoutes 与 ListPublicNames 条数相等」看着像个好不变量，其实不是：
// 那是**两次独立调用**，跨越两个锁区间，中间插入一条路由是正常行为。
// 真正的不变量在**单次 Resolve 内部**：它拿到的 Candidate 必须同时有
// Provider 与 UpstreamModel，两者都不为 nil。
//
// 这个不变量在无锁时会以两种方式破：
//  1. 直接并发 map 读写 → Go 运行时 fatal error，进程当场消失；
//  2. （退一步，即使没触发 fatal）buildCandidates 的过滤循环里
//     findProviderByID / findUpstreamModel 各读一张 map，夹在 Add* 中间
//     会得到「target 在、provider 已写、model 还没写」的组合 —— 该成员
//     被过滤掉还算运气好，更糟的是**主目标合成兜底**那条分支读到半成品
//     后返回一个 Provider 或 UpstreamModel 为 nil 的 Candidate，
//     调用方 cand.Provider.Slug 当场 nil deref panic。
//
// 现在整条 Resolve（含内部连锁查找）在同一把读锁内完成，上述两种都不可达。
func TestRouteIndex_ResolveNeverReturnsHalfBuiltChain(t *testing.T) {
	ri := NewRouteIndex()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			id := "x" + strconv.Itoa(i)
			ri.AddProvider(&ProviderRef{ID: id, Slug: id, Enabled: true})
			ri.AddUpstreamModel(&UpstreamModel{ID: id, ProviderID: id, ModelID: id, Enabled: true})
			ri.AddRoute(&Route{ID: id, PublicName: id, ProviderID: id, UpstreamModelID: id, Enabled: true})
			ri.AddRouteTarget(&Target{
				ID: id, RouteID: id, ProviderID: id, UpstreamModelID: id, Position: 0, Enabled: true,
			})
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// 单次 Resolve 内部自洽：每个候选都必须成对带 Provider 与
			// UpstreamModel。缺一个就是「半成品链」，调用方解引用即 panic。
			if res, err := ri.Resolve("x0"); err == nil {
				if res.Route == nil {
					t.Error("Resolve 返回了 nil Route")
					return
				}
				for i, c := range res.Candidates {
					if c.Provider == nil || c.UpstreamModel == nil {
						t.Errorf("候选 %d 是半成品：Provider=%v UpstreamModel=%v（调用方解引用即 panic）",
							i, c.Provider, c.UpstreamModel)
						return
					}
				}
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}
