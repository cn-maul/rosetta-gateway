package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// execer 是 *sql.DB 与 *sql.Tx 的公共子集。
//
// 写操作统一写成「接受 execer 的内部函数 + 一个用 s.db 的公开方法」，
// 这样同一份 SQL 既能在自动提交下跑，也能被包进事务，不会出现两套写法漂移。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ErrNotFound 表示按主键操作的目标行不存在。
//
// 旧的 DAO 是裸 ExecContext 且不看 RowsAffected：删一个不存在的 ID 也返回 nil，
// 接口于是回 200 {"status":"deleted"} —— 前端把「删成功了并不存在的行」当成功，
// 用户以为清掉了。Delete 的语义应该是「确实删掉了一行」。
var ErrNotFound = errors.New("record not found")

// deleteByID 按主键删除，影响 0 行时返回 ErrNotFound。
// table 只接受本包内的字面量（routes / providers / upstream_models / access_keys）。
func deleteByID(ctx context.Context, ex execer, table, id string) error {
	res, err := ex.ExecContext(ctx, `DELETE FROM `+table+` WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// checkAffected 承接 `UPDATE ... WHERE id = ?` 的结果：影响 0 行（主键已不存在）
// 返回 ErrNotFound，而不是静默 nil。
//
// 各 Update handler 都先 Get 再 Update，TOCTOU 窗口极小，但并非为零：Get 与 Update
// 之间那行被删（并发删除 / 另一实例）时，旧实现 UPDATE 命中 0 行仍返回 nil，接口回
// 200「更新成功」，用户以为写进去了，实际目标早已消失。SQLite 对匹配 WHERE 的行一律计入
// changes（即便新旧值相同），所以「存在但值没变」不会被误判为 0 行。
func checkAffected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// IsUniqueViolation 报告错误是否为唯一约束冲突（如 routes.public_name 重名）。
//
// modernc 的驱动会把 constraint failed 报成 *sqlite.Error，但没有导出可供
// errors.As 的具名 sentinel；匹配错误文本是这里稳定且够用的判据（文本由 SQLite
// 自身产出，不随驱动版本变）。
func IsUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// IsForeignKeyViolation 报告错误是否为外键约束失败。
//
// 删除仍被 route / 上游模型引用的 provider 会撞 RESTRICT 外键。这不是「服务器故障」，
// 而是「还有依赖没清干净」—— 该回 409 + 一句人能看懂的话，而不是 500 + 裸 SQL 错误。
func IsForeignKeyViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// ValidateRouteTargetRef 校验 (providerID, modelID) 是一个可用的路由目标：
// provider 存在、上游模型存在，且该模型确属该 provider。
//
// routes 与 route_targets 两张表都有这两列，但外键只做**单列**存在性检查，
// 不拦跨 provider 的错配。而运行时 buildCandidates 是按 (provider, model) 成对
// 查表的 —— 查不到就得到空候选，于是产出最难排查的那种形态：
// 接口回 201、界面显示保存成功、/v1 上这个模型名恒 404。
//
// 目标链接口（PUT /routes/{id}/targets）从一开始就在做这个校验，routes 的
// Create/Update 却只检查了非空串 —— 同一件事两条写入路径宽严不一，所以把校验
// 提到这里共用。
func (s *Store) ValidateRouteTargetRef(ctx context.Context, providerID, modelID string) error {
	if providerID == "" || modelID == "" {
		return errors.New("provider_id 与 upstream_model_id 均为必填")
	}
	p, err := s.GetProvider(ctx, providerID)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("provider %q 不存在", providerID)
	}
	m, err := s.GetUpstreamModel(ctx, modelID)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("上游模型 %q 不存在", modelID)
	}
	if m.ProviderID != providerID {
		return fmt.Errorf("上游模型 %q 不属于 provider %q", modelID, providerID)
	}
	return nil
}

// CreateProviderWithCredential 在一个事务里创建上游与它的首条凭据。
//
// provider 与凭据是「添加一个上游」这一个动作的两半。分两步写、第二步失败，
// 就留下一个没有任何凭据的 provider：界面报 500、用户以为没建成功，
// 而它已经躺在列表里了 —— 而且永远不可用（GetAnyClient 找不到凭据）。
// c 为 nil 时只建 provider（表单没填 api_key 的情形）。
func (s *Store) CreateProviderWithCredential(ctx context.Context, p *Provider, c *Credential) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := createProvider(ctx, tx, p); err != nil {
		return err
	}
	if c != nil {
		if err := createCredential(ctx, tx, c); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CreateRouteWithHeadTarget 在一个事务里创建 route 并种下它的链首目标。
//
// 分两步写会留下「route 已建、链没种上」的半成品：接口回 500，但那条 route
// 已经在库里了；运维重试又撞 public_name 的 UNIQUE 约束，得到一个和上次
// 完全不同的错误。要么全成，要么全不成。
func (s *Store) CreateRouteWithHeadTarget(ctx context.Context, r *Route) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := createRoute(ctx, tx, r); err != nil {
		return err
	}
	if err := createRouteTarget(ctx, tx, &RouteTarget{
		RouteID:         r.ID,
		ProviderID:      r.ProviderID,
		UpstreamModelID: r.UpstreamModelID,
		Position:        0,
		Enabled:         true,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateRouteWithHeadTarget 在一个事务里更新 route 并把链首对齐到新的主目标。
//
// 不这么做的话，SyncHeadTarget 失败即留下「routes 主目标列 = 新值、链首 = 旧值」
// 的永久分叉：响应回显新值、界面显示成功，实际流量仍打旧目标。运行时以链为准，
// 这种分叉没有任何自愈路径。
func (s *Store) UpdateRouteWithHeadTarget(ctx context.Context, id string, r *Route) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := updateRoute(ctx, tx, id, r); err != nil {
		return err
	}
	if err := syncHeadTarget(ctx, tx, id, r.ProviderID, r.UpstreamModelID); err != nil {
		return err
	}
	return tx.Commit()
}
