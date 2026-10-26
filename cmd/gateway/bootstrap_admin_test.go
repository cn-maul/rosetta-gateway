package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "gw.db"), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// 全新部署（users 表为空）→ 必须建出一个**待初始化**的 admin 账号。
//
// 这是统一认证后「第一个管理员怎么来」的唯一答案。密码哈希必须为空：
// 空串是一个可判定状态，前端据此显示「首次设置密码」表单；
// 留一个假哈希会让登录走到「认证失败」，用户只会反复重试。
func TestEnsureBootstrapAdmin_CreatesUninitializedAdmin(t *testing.T) {
	db := newTestStore(t)

	ensureBootstrapAdmin(context.Background(), db, slog.Default())

	users, err := db.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("want exactly 1 bootstrapped user, got %d", len(users))
	}
	if users[0].Role != store.RoleAdmin {
		t.Errorf("role = %q, want %q", users[0].Role, store.RoleAdmin)
	}
	if users[0].Status != store.UserStatusActive {
		t.Errorf("status = %q, want active", users[0].Status)
	}
	if users[0].PasswordHash != "" {
		t.Errorf("password_hash = %q, want empty so the UI can offer password setup", users[0].PasswordHash)
	}
	if users[0].AuthVersion != 1 {
		t.Errorf("auth_version = %d, want 1", users[0].AuthVersion)
	}
}

// 引导出的账号必须能被「首次设置密码」端点认出来。
//
// 这两段必须成对：ensureBootstrapAdmin 建出空哈希账号，
// FindUninitializedAdmin 认出它。任一侧改了而另一侧没改，
// 症状是「新部署打开登录页看到一个永远登不进去的输入框」。
func TestEnsureBootstrapAdmin_CreatesAccountRecognizedByBootstrapLookup(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	ensureBootstrapAdmin(ctx, db, slog.Default())

	pending, err := db.FindUninitializedAdmin(ctx)
	if err != nil {
		t.Fatalf("find uninitialized admin: %v", err)
	}
	if pending == nil {
		t.Fatal("bootstrapped admin not recognized by FindUninitializedAdmin; " +
			"the login page would show a password box that can never succeed")
	}
	if pending.Username != bootstrapAdminUsername {
		t.Errorf("username = %q, want %q", pending.Username, bootstrapAdminUsername)
	}
}

// 设过密码之后引导状态必须立刻消失 —— 否则这个免鉴权窗口永不关闭。
func TestBootstrapLookup_DisappearsAfterPasswordSet(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	ensureBootstrapAdmin(ctx, db, slog.Default())
	pending, err := db.FindUninitializedAdmin(ctx)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if pending == nil {
		t.Fatal("expected a pending admin before password is set")
	}

	// 用一个格式合法的假哈希即可 —— 这里只关心「哈希非空」这件事。
	if err := db.SetUserPassword(ctx, pending.ID, "pbkdf2-sha256$210000$c2FsdA==$aGFzaA=="); err != nil {
		t.Fatalf("set password: %v", err)
	}

	after, err := db.FindUninitializedAdmin(ctx)
	if err != nil {
		t.Fatalf("find after set: %v", err)
	}
	if after != nil {
		t.Fatal("admin still reported as uninitialized after password was set; " +
			"the免鉴权 bootstrap endpoint would stay open forever")
	}
}

// 幂等：重复调用不得再建第二个账号。
func TestEnsureBootstrapAdmin_Idempotent(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	ensureBootstrapAdmin(ctx, db, slog.Default())
	ensureBootstrapAdmin(ctx, db, slog.Default())

	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("idempotency broken: want 1 user, got %d", len(users))
	}
}

// 已有用户时不引导 —— 绝不能覆盖别人自建的账号。
func TestEnsureBootstrapAdmin_SkipsWhenUsersExist(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	if err := db.CreateUser(ctx, &store.User{
		ID: "u1", Username: "alice", PasswordHash: "x",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	ensureBootstrapAdmin(ctx, db, slog.Default())

	users, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("bootstrap overwrote existing users: want 1, got %d", len(users))
	}
	if users[0].Username != "alice" {
		t.Errorf("existing user replaced by bootstrap: got %q", users[0].Username)
	}
}

type bootstrapStoreStub struct {
	*store.Store
	countUsersErr  error
	findPendingErr error
	createUserErr  error
	onCreateUser   func(context.Context, *store.User) error
}

func (s *bootstrapStoreStub) CountUsers(ctx context.Context) (int, error) {
	if s.countUsersErr != nil {
		return 0, s.countUsersErr
	}
	return s.Store.CountUsers(ctx)
}

func (s *bootstrapStoreStub) CreateUser(ctx context.Context, u *store.User) error {
	if s.onCreateUser != nil {
		return s.onCreateUser(ctx, u)
	}
	if s.createUserErr != nil {
		return s.createUserErr
	}
	return s.Store.CreateUser(ctx, u)
}

func (s *bootstrapStoreStub) FindUninitializedAdmin(ctx context.Context) (*store.User, error) {
	if s.findPendingErr != nil {
		return nil, s.findPendingErr
	}
	return s.Store.FindUninitializedAdmin(ctx)
}

// R0-AC-07：bootstrap 状态不可读取时必须返回 failed，而不是假装成功。
func TestR0AC07_BootstrapReadFailureIsFatal(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	readErr := errors.New("database unavailable")

	t.Run("count users", func(t *testing.T) {
		db := &bootstrapStoreStub{
			Store:         newTestStore(t),
			countUsersErr: readErr,
		}
		outcome, err := ensureBootstrapAdmin(ctx, db, logger)
		if !errors.Is(err, readErr) {
			t.Fatalf("outcome = %q, err = %v, want failed with %v",
				outcome, err, readErr)
		}
		if outcome != adminBootstrapFailed {
			t.Fatalf("outcome = %q, want %q", outcome, adminBootstrapFailed)
		}
	})

	t.Run("uninitialized admin lookup", func(t *testing.T) {
		db := &bootstrapStoreStub{
			Store:          newTestStore(t),
			findPendingErr: readErr,
		}
		outcome, err := ensureBootstrapAdmin(ctx, db, logger)
		if !errors.Is(err, readErr) {
			t.Fatalf("outcome = %q, err = %v, want failed with %v",
				outcome, err, readErr)
		}
		if outcome != adminBootstrapFailed {
			t.Fatalf("outcome = %q, want %q", outcome, adminBootstrapFailed)
		}
	})
}

// R0-AC-08：空库上的非唯一写入失败必须阻断 bootstrap。
func TestR0AC08_NonUniqueCreateFailureIsFatal(t *testing.T) {
	ctx := context.Background()
	createErr := errors.New("disk I/O error")
	db := &bootstrapStoreStub{
		Store:         newTestStore(t),
		createUserErr: createErr,
	}
	outcome, err := ensureBootstrapAdmin(ctx, db,
		slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if !errors.Is(err, createErr) {
		t.Fatalf("outcome = %q, err = %v, want failed with %v",
			outcome, err, createErr)
	}
	if outcome != adminBootstrapFailed {
		t.Fatalf("outcome = %q, want %q", outcome, adminBootstrapFailed)
	}
}

// R0-AC-09：唯一冲突只有在重读证明并发实例确实建出管理员后才可接受。
func TestR0AC09_ConcurrentCreateIsVerifiedByReRead(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)
	conflict := errors.New("UNIQUE constraint failed: users.username")
	db := &bootstrapStoreStub{
		Store: base,
		onCreateUser: func(ctx context.Context, u *store.User) error {
			// 模拟另一个实例在本实例写入前已成功创建了带密码的管理员。
			winner := *u
			winner.ID = "concurrent-admin"
			winner.PasswordHash = "pbkdf2-sha256$210000$c2FsdA==$aGFzaA=="
			if err := base.CreateUser(ctx, &winner); err != nil {
				t.Fatalf("seed concurrent winner: %v", err)
			}
			return conflict
		},
	}

	outcome, err := ensureBootstrapAdmin(ctx, db,
		slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("verified concurrent create failed bootstrap: %v", err)
	}
	if outcome != adminBootstrapReady {
		t.Fatalf("outcome = %q, want %q", outcome, adminBootstrapReady)
	}
}

// 唯一冲突后重读仍为空，说明错误码本身不能证明 bootstrap 成功。
func TestR0AC09_UnverifiedUniqueConflictIsFatal(t *testing.T) {
	ctx := context.Background()
	conflict := errors.New("UNIQUE constraint failed: users.username")
	db := &bootstrapStoreStub{
		Store:         newTestStore(t),
		createUserErr: conflict,
	}
	outcome, err := ensureBootstrapAdmin(ctx, db,
		slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err == nil {
		t.Fatal("unverified unique conflict was accepted as success")
	}
	if outcome != adminBootstrapFailed {
		t.Fatalf("outcome = %q, want %q", outcome, adminBootstrapFailed)
	}
}

func TestInitializeAdminBootstrap_PropagatesFailure(t *testing.T) {
	db := newTestStore(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	_, err := initializeAdminBootstrap(context.Background(), db, slog.Default())
	if err == nil {
		t.Fatal("initializeAdminBootstrap returned nil after a read failure")
	}
}
