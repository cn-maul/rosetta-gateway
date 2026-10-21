package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// TestKeyHandler_PatchFieldAndClaimAreAtomic 坐实 A7-P2-08 的修法。
//
// # 修之前会怎样
//
// PATCH 是**两次自动提交**：先 UpdateAccessKey（名称/配额/有效期/…），
// 再 ReassignAccessKey（归属）。第二步失败时第一步已经落库，而响应是
// 404/500 —— 调用方看到失败、库里却是新配置。后果：
//   - 客户端重试 → 以为回到原点，实际带着上一次的半截修改继续跑；
//   - 运维手工核对 → 「明明保存失败了，怎么限制真的变了」；
//   - 归属那一半更糟：重命名成功、改归属失败，界面按旧响应显示新主人，
//     下一次调用就用错了身份计费。
//
// # 怎么在测试里复现「第二步失败」
//
// handler 自己会先校验目标用户存在，所以走 HTTP 层造不出这个失败。这里
// 直接打 DAO 的合并入口，传一个**外键不存在的 user_id** ——
// access_keys.user_id 上有 REFERENCES users(id)，第二次 UPDATE 必然失败。
// 这正是生产里第二步会失败的那一类原因（并发删除用户、库瞬时故障）。
func TestKeyHandler_PatchFieldAndClaimAreAtomic(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()

	if err := st.CreateUser(ctx, &store.User{
		ID: "u1", Username: "alice", PasswordHash: "x",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	seed := &store.AccessKey{
		ID: "k-atomic", KeyHash: "hash-atomic", KeyPrefix: "sk-", Name: "原名",
		Enabled: true, UserID: "u1", QuotaTokens: 1000,
	}
	if err := st.CreateAccessKey(ctx, seed); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	// 字段改成新值，同时把归属改到一个不存在的用户 → 第二步必然失败。
	before, err := st.GetAccessKey(ctx, "k-atomic")
	if err != nil || before == nil {
		t.Fatalf("read before: %+v %v", before, err)
	}
	before.Name = "改成的新名字"
	before.QuotaTokens = 9999

	err = st.UpdateAccessKeyWithOwner(ctx, "k-atomic", before, "ghost-user", true)
	if err == nil {
		t.Fatal("第二步（改归属）应当失败：ghost-user 不是合法外键")
	}

	// 关键断言：第一步的字段修改也必须**不存在**。
	after, err := st.GetAccessKey(ctx, "k-atomic")
	if err != nil || after == nil {
		t.Fatalf("read after: %+v %v", after, err)
	}
	if after.Name != "原名" {
		t.Errorf("字段修改未被回滚：name=%q，两步写的部分提交仍然存在", after.Name)
	}
	if after.QuotaTokens != 1000 {
		t.Errorf("配额修改未被回滚：quota_tokens=%d", after.QuotaTokens)
	}
	if after.UserID != "u1" {
		t.Errorf("归属被改坏了：user_id=%q", after.UserID)
	}
}

// TestKeyHandler_PatchFieldsOnlyKeepsOwner 钉住「只改字段时归属绝不被碰」。
//
// UpdateAccessKeyWithOwner 的 changeOwner=false 分支必须与原来的
// UpdateAccessKey 逐字等价：一条 UPDATE、只写那些可管理字段、不碰
// user_id。少了这条，一次纯改名就会把 key 变成无归属 —— 而 auth 对
// 无归属 key 一律 401（ErrKeyUnowned），症状是「改个名字 key 就坏了」。
func TestKeyHandler_PatchFieldsOnlyKeepsOwner(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	if err := st.CreateUser(ctx, &store.User{
		ID: "u1", Username: "alice", PasswordHash: "x",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := st.CreateAccessKey(ctx, &store.AccessKey{
		ID: "k-owner", KeyHash: "hash-owner", KeyPrefix: "sk-", Name: "原名",
		Enabled: true, UserID: "u1",
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	got, err := st.GetAccessKey(ctx, "k-owner")
	if err != nil || got == nil {
		t.Fatalf("read: %+v %v", got, err)
	}
	got.Name = "只改名字"
	if err := st.UpdateAccessKeyWithOwner(ctx, "k-owner", got, "", false); err != nil {
		t.Fatalf("UpdateAccessKeyWithOwner(changeOwner=false): %v", err)
	}

	after, err := st.GetAccessKey(ctx, "k-owner")
	if err != nil || after == nil {
		t.Fatalf("read after: %+v %v", after, err)
	}
	if after.Name != "只改名字" {
		t.Errorf("名字没改进去：%q", after.Name)
	}
	if after.UserID != "u1" {
		t.Errorf("changeOwner=false 却动了归属：user_id=%q", after.UserID)
	}
}

// TestKeyHandler_PatchClaimViaHTTPStillWorks 确认合并事务没有把既有的
// 「管理员 PATCH 认领」这条路走坏 —— 上面两个测试直接打 DAO，这条走
// 真实 HTTP handler，两头都要绿。
func TestKeyHandler_PatchClaimViaHTTPStillWorks(t *testing.T) {
	st := newTestStore(t)
	h := NewKeyHandler(st)
	ctx := t.Context()
	if err := st.CreateUser(ctx, &store.User{
		ID: "u-claim", Username: "claimable", PasswordHash: "x",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := st.CreateUser(ctx, &store.User{
		ID: "u1", Username: "alice", PasswordHash: "x",
		Role: store.RoleUser, Status: store.UserStatusActive, AuthVersion: 1,
	}); err != nil {
		t.Fatalf("seed owner: %v", err)
	}

	rec := httptest.NewRecorder()
	h.Create(rec, asUser(jsonRequest(http.MethodPost, "/admin/api/keys",
		bytes.NewBufferString(`{"name":"k"}`)), "u1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body.String())
	}
	var created keyCreateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// 同时改名字和认领 —— 这正是原来会部分提交的组合。
	rec2 := httptest.NewRecorder()
	h.Update(rec2, asAdmin(jsonRequest(http.MethodPatch, "/admin/api/keys/"+created.ID,
		bytes.NewBufferString(`{"name":"改完的名字","user_id":"u-claim"}`))), created.ID)
	if rec2.Code != http.StatusOK {
		t.Fatalf("patch code=%d body=%s", rec2.Code, rec2.Body.String())
	}

	dbKey, _ := st.GetAccessKey(ctx, created.ID)
	if dbKey == nil {
		t.Fatal("key 不见了")
	}
	if dbKey.Name != "改完的名字" {
		t.Errorf("名字没改进去：%q", dbKey.Name)
	}
	if dbKey.UserID != "u-claim" {
		t.Errorf("认领没生效：user_id=%q", dbKey.UserID)
	}
}
