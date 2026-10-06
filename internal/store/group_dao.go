package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

// Group 是模型可见性的分组（多用户改造 P1）。
//
// 语义要点（写死，不可配置）：
//
//   - 组本身**不**携带额度与限速 —— 那些能力现在没有执行点，
//     加列只会让 schema 说谎（见 store.go 的 dropDeadColumns 教训）。
//   - `groups` 表**为空** = 分组功能未启用 = 全部 key 可见全部模型，
//     行为与改造前完全一致。这是 P1 的向后兼容保证。
//   - 某组**白名单为空** = 该组不限制（与上一条同源：空即不限制）。
//     配置界面上必须把这两种「空」的差别讲清楚，否则会出现
//     「建了组、忘了配模型、以为收紧了权限，实际放得更开」。
type Group struct {
	ID          string
	Name        string
	Description string
	CreatedAt   int64
	UpdatedAt   int64
}

// ErrGroupNotEmpty 表示组里还有成员（用户或访问密钥），拒绝删除。
//
// 为什么拒绝而不是靠外键的 ON DELETE SET NULL 放行：
// 组的白名单非空时，成员被 SET NULL 后会**从「受限」变成「不受限」**——
// 删一个组等于悄悄给一组人扩权。这类「静默放宽」正是多用户改造要消除的东西，
// 所以宁可让删除失败，要求管理员先把成员迁走。
// （foreign key 上的 SET NULL 仍保留，作为直接改库时的安全网。）
var ErrGroupNotEmpty = errors.New("group still has members or keys")

// groupColumns 是 groups 表的读列清单。抽成常量避免多处 SELECT 漏改
// （同 user_dao.userColumns 的理由）。
const groupColumns = `id, name, COALESCE(description,''), created_at, updated_at`

// ListGroups 返回全部组，按名称排序。
//
// 名称排序而不是创建时间：管理界面按名字找组，按创建时间排会让
// 「刚建的组排最后」随着组数增长越来越难找。
func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT `+groupColumns+` FROM groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Group, 0, 4)
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetGroup 按 id 取组；不存在时返回 (nil, nil)（与 GetUser 同约定）。
func (s *Store) GetGroup(ctx context.Context, id string) (*Group, error) {
	var g Group
	err := s.read.QueryRowContext(ctx, `SELECT `+groupColumns+` FROM groups WHERE id = ?`, id).
		Scan(&g.ID, &g.Name, &g.Description, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// CreateGroup 插入一个组。名称冲突由唯一约束报出，
// 调用方用 IsUniqueViolation(err) 区分「重名」与「其他故障」。
func (s *Store) CreateGroup(ctx context.Context, g *Group) error {
	now := time.Now().UnixMilli()
	if g.CreatedAt == 0 {
		g.CreatedAt = now
	}
	g.UpdatedAt = now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO groups (id, name, description, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		g.ID, g.Name, g.Description, g.CreatedAt, g.UpdatedAt)
	return err
}

// UpdateGroup 更新名称与描述。
func (s *Store) UpdateGroup(ctx context.Context, g *Group) error {
	g.UpdatedAt = time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`UPDATE groups SET name = ?, description = ?, updated_at = ? WHERE id = ?`,
		g.Name, g.Description, g.UpdatedAt, g.ID)
	return checkAffected(res, err)
}

// DeleteGroup 删除组及其模型白名单。组内仍有**用户或 key** 时返回
// ErrGroupNotEmpty —— 两类成员都会因 SET NULL 而丢失组约束。
//
// 成员检查与删除在同一个事务里做：分两步会出现「检查通过 → 别人刚好把用户
// 加进来 → 删除成功」的竞态，而那次竞态的后果正是把那个用户的权限放大。
func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 同时挡users 与 **access_keys**。
	//
	// access_keys.group_id 是 ON DELETE SET NULL（见 DDL），删组时 key 的
	// 组归属被清空 → 该key 从「受组模型白名单约束」变成「不受约束」。
	// 这是**静默放宽**：用户的模型权限凭空变大，且界面上看不出任何变化
	// （key 还在，只是 group_id 变空）。只挡 users 时，一组key 就能拆掉
	// 管理员设的模型白名单。
	var members int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE group_id = ?`, id).Scan(&members); err != nil {
		return err
	}
	if members > 0 {
		return ErrGroupNotEmpty
	}
	var keys int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM access_keys WHERE group_id = ?`, id).Scan(&keys); err != nil {
		return err
	}
	if keys > 0 {
		return ErrGroupNotEmpty
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM groups WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, aerr := res.RowsAffected()
	if aerr != nil {
		s.logger.Debug("delete group: RowsAffected unavailable", "error", aerr)
	} else if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// CountGroups 返回组总数。供启动日志与快照判定「分组功能是否启用」。
func (s *Store) CountGroups(ctx context.Context) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM groups`).Scan(&n)
	return n, err
}

// CountGroupMembers 返回组内账号数。
//
// 存在的理由：删除被拒时要把「还有几个人」告诉管理员 —— 只说「删不掉」
// 会让人去查库，而真正该做的是把人迁走。单独一个 COUNT 而不是复用
// ListUsers：后者会把全部账号（含密码哈希）读进内存，为一个数字不值得。
func (s *Store) CountGroupMembers(ctx context.Context, groupID string) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE group_id = ?`, groupID).Scan(&n)
	return n, err
}

// ListGroupModels 返回全部组的模型白名单：group_id → 公开模型名（已排序）。
//
// 一次全量读而不是按组读：快照重建时需要的就是全量，
// 按组读会变成 N+1 次查询。
func (s *Store) ListGroupModels(ctx context.Context) (map[string][]string, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT group_id, public_model FROM user_group_models ORDER BY group_id, public_model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string][]string, 4)
	for rows.Next() {
		var gid, model string
		if err := rows.Scan(&gid, &model); err != nil {
			return nil, err
		}
		out[gid] = append(out[gid], model)
	}
	return out, rows.Err()
}

// ListGroupModelsByGroup 返回单个组的白名单，供管理界面回显。
func (s *Store) ListGroupModelsByGroup(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT public_model FROM user_group_models WHERE group_id = ? ORDER BY public_model`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]string, 0, 8)
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ReplaceGroupModels 整体替换某组的模型白名单（先删后插，同一事务）。
//
// 为什么是「整体替换」而不是增删单条：管理界面的交互就是「勾选一组模型后保存」，
// 逐个 diff 需要前端算差集，且并发下两次 diff 会互相覆盖。整体替换语义清晰、
// 幂等，且天然把「取消勾选」表达为不在新集合里。
//
// 传空切片 = 清空白名单 = 该组不限制模型（不是「什么都看不到」，
// 语义见 Group 的注释）。
func (s *Store) ReplaceGroupModels(ctx context.Context, groupID string, models []string) error {
	normalized := NormalizeModelNames(models)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 显式校验组存在：models 为空时下面一句 INSERT 都不执行，
	// 光靠外键兜不住「给一个不存在的组设空白名单」—— 那会静默成功。
	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM groups WHERE id = ?`, groupID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM user_group_models WHERE group_id = ?`, groupID); err != nil {
		return err
	}
	for _, m := range normalized {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_group_models (group_id, public_model) VALUES (?, ?)`,
			groupID, m); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NormalizeModelNames 去空白、丢空串、去重、排序。
//
// 排序让落库顺序确定（便于比对与测试）；去重让前端「同一个模型勾了两次」
// 不会撞主键报 500。
//
// 导出是因为管理面**回显**也必须走同一条规则：创建/更新响应若回显
// 请求原文，就会与随后 GET 读回来的归一值不一致（实测
// ["  "," pub-chat ","pub-chat"] vs ["pub-chat"]），界面上的
// 「限 N 个模型」徽标因此显示错数字。
func NormalizeModelNames(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
