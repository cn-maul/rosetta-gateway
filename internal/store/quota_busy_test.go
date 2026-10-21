package store

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
)

// 审计 M1：两个 Store 打开同一个 SQLite 文件并发预占时，失败只能是
// 「额度不足」（ok=false, err=nil），不能出现 SQLITE_BUSY/BUSY_SNAPSHOT
// 错误。调用方对错误是 fail-open 的，一旦这里返回 busy，多实例部署下终身
// 配额会静默失效。
//
// 旧实现先 SELECT 再 UPDATE：读快照与升级写之间若有其它连接提交，SQLite
// 返回 BUSY_SNAPSHOT(517)，而 _busy_timeout 对该错误无效。改成写先行的
// 条件 UPDATE 后，事务一开始就拿写锁。
func TestReserveQuota_TwoStoresNeverBusy(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gw.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	first, err := Open(dbPath, logger)
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	t.Cleanup(func() { first.Close() })
	second, err := Open(dbPath, logger)
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	t.Cleanup(func() { second.Close() })

	ctx := context.Background()
	if err := first.CreateUser(ctx, &User{
		ID: "u1", Username: "alice", Status: UserStatusActive,
		QuotaTokens: 10_000, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := first.CreateAccessKey(ctx, &AccessKey{
		ID: "k1", Name: "k1", KeyHash: "hash", UserID: "u1", QuotaTokens: 10_000,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	stores := []*Store{first, second}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := stores[i%2]
			if _, _, err := st.ReserveQuota(ctx, "k1", 100); err != nil {
				errs <- err
			}
			if _, _, err := st.ReserveUserQuota(ctx, "u1", 100); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("reserve returned an error under two-store contention: %v", err)
	}
}
