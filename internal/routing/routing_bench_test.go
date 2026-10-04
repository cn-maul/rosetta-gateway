package routing

import (
	"fmt"
	"testing"
)

// buildIndexWithModels 造一个挂了 n 个上游模型的索引。
func buildIndexWithModels(n int) *RouteIndex {
	ri := NewRouteIndex()
	ri.AddProvider(&ProviderRef{ID: "p1", Slug: "myslug", Enabled: true})
	for i := 0; i < n; i++ {
		ri.AddUpstreamModel(&UpstreamModel{
			ID:         fmt.Sprintf("m%04d", i),
			ProviderID: "p1",
			ModelID:    fmt.Sprintf("model-name-%04d", i),
			Enabled:    true,
		})
	}
	return ri
}

// BenchmarkResolveProviderModelDirect 量的是 provider/model 直连形式的解析 ——
// 每个请求都会走 FindUpstreamModelByModelID。
//
// 旧实现是 `for _, m := range ri.providerModels[providerID]` 逐个字符串比较，
// 复杂度 O(该 provider 的模型数)。这是纯 CPU 的字符串比较，在高 QPS 下
// 直接吃掉一个核心。改成 modelsByModelID 反查表后是两次 O(1) 查找。
//
// 现实规模参考：单 provider 挂 50~200 个上游模型很常见（OpenAI 官方就有
// 上百个 model id）。
func BenchmarkResolveProviderModelDirect(b *testing.B) {
	for _, n := range []int{10, 50, 200, 1000} {
		b.Run(fmt.Sprintf("models=%d", n), func(b *testing.B) {
			ri := buildIndexWithModels(n)
			// 取最后一个 —— 线性扫描的最坏情况，必须扫完全部。
			worst := fmt.Sprintf("myslug/model-name-%04d", n-1)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := ri.Resolve(worst); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFindUpstreamModelByModelID 单独量反查本身，把路由其余部分排除掉。
func BenchmarkFindUpstreamModelByModelID(b *testing.B) {
	for _, n := range []int{10, 50, 200, 1000} {
		b.Run(fmt.Sprintf("models=%d", n), func(b *testing.B) {
			ri := buildIndexWithModels(n)
			worst := fmt.Sprintf("model-name-%04d", n-1)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if ri.FindUpstreamModelByModelID("p1", worst) == nil {
					b.Fatal("lookup failed")
				}
			}
		})
	}
}

// BenchmarkResolveNamedRoute 是对照组：具名路由走 byPublicName 的一次 O(1)
// 查找，模型数增加对它**不应有任何影响**。若这张表随模型数上涨，
// 说明有人把反查塞进了主路径。
func BenchmarkResolveNamedRoute(b *testing.B) {
	for _, n := range []int{10, 200, 1000} {
		b.Run(fmt.Sprintf("models=%d", n), func(b *testing.B) {
			ri := buildIndexWithModels(n)
			ri.AddRoute(&Route{ID: "r1", PublicName: "flash", ProviderID: "p1",
				UpstreamModelID: "m0000", Enabled: true})
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := ri.Resolve("flash"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
