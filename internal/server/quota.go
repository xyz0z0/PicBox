package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// quotaEffectiveBytes 返回账号当前生效的配额（字节）。
//
// 语义与原版 lib/userQuota.ts 一致：
//   - User.quotaBytes 非 NULL 时优先使用该覆盖值；
//   - 否则继承全局 USER_QUOTA_BYTES（a.cfg.UserQuotaBytes）；
//   - 0 表示不限。
//
// 账号不存在时返回 errNoUser，由调用方决定状态码。
func (a *App) quotaEffectiveBytes(ctx context.Context, userID int64) (int64, error) {
	var override sql.NullInt64
	err := a.db.QueryRowContext(ctx, `SELECT quotaBytes FROM "User" WHERE id = ?`, userID).Scan(&override)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, errNoUser
	case err != nil:
		return 0, fmt.Errorf("读取账号配额失败: %w", err)
	}
	if override.Valid {
		return override.Int64, nil
	}
	return a.cfg.UserQuotaBytes, nil
}

// quotaUsedBytes 统计账号占用的原图字节数。
//
// 统计所有图片记录，包含回收站与管理员下架的图片：在超期清理或彻底删除真正移除
// 文件之前，它们仍然占用磁盘，不能因为 deletedAt 非空就释放配额。
func (a *App) quotaUsedBytes(ctx context.Context, userID int64) (int64, error) {
	var used sql.NullInt64
	if err := a.db.QueryRowContext(ctx,
		`SELECT SUM(size) FROM "Image" WHERE uploaderId = ?`, userID).Scan(&used); err != nil {
		return 0, fmt.Errorf("读取存储用量失败: %w", err)
	}
	if !used.Valid {
		return 0, nil
	}
	return used.Int64, nil
}
