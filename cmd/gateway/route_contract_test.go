package main

// 生产路由契约导出。internal/server 的外部测试包无法 import package main，
// 所以由本测试进程执行真实 buildApp，再把路由 pattern 以一行 JSON 交给它。
// 这不读源码、不手抄清单：端点漏注册会让导出的集合直接少一条。

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/ratelimit"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

func TestExportRouteContract(t *testing.T) {
	if testing.Short() {
		t.Skip("route contract export is run by internal/server authz tests")
	}
	db, err := store.Open(t.TempDir()+"/route-contract.db", discardLogger())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	app := buildApp(routeDeps{
		db: db, cfg: config.Default(), logger: discardLogger(),
		pool: upstream.NewPool(discardLogger()), rateLimiter: ratelimit.New(),
		usage: newUsageRecorder(db, discardLogger()),
	})
	b, err := json.Marshal(app.Routes)
	if err != nil {
		t.Fatalf("marshal route contract: %v", err)
	}
	fmt.Printf("ROUTE_CONTRACT:%s\n", b)
}
