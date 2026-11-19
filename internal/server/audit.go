package server

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// 管理端审计动作全集。必须与原版 lib/audit.ts 的 14 个值逐一对应：
// 管理端 UI 的筛选下拉、审计写入与日志查询都依赖这份清单。
const (
	auditActionAdminLogin                   = "ADMIN_LOGIN"
	auditActionAdminUserCreate              = "ADMIN_USER_CREATE"
	auditActionAdminUserDisable             = "ADMIN_USER_DISABLE"
	auditActionAdminUserEnable              = "ADMIN_USER_ENABLE"
	auditActionAdminUserPasswordReset       = "ADMIN_USER_PASSWORD_RESET"
	auditActionAdminUserExportCooldownReset = "ADMIN_USER_EXPORT_COOLDOWN_RESET"
	auditActionAdminUserQuotaUpdate         = "ADMIN_USER_QUOTA_UPDATE"
	auditActionAdminUserEmailUpdate         = "ADMIN_USER_EMAIL_UPDATE"
	auditActionAdminActivationCodeCreate    = "ADMIN_ACTIVATION_CODE_CREATE"
	auditActionAdminActivationCodeRevoke    = "ADMIN_ACTIVATION_CODE_REVOKE"
	auditActionActivationCodeUsed           = "ACTIVATION_CODE_USED"
	auditActionAdminImageHide               = "ADMIN_IMAGE_HIDE"
	auditActionAdminImageShow               = "ADMIN_IMAGE_SHOW"
	auditActionAdminImageDelete             = "ADMIN_IMAGE_DELETE"
)

// auditActions 是允许写入 AuditLog 的动作集合。数据库 schema 里同样有 CHECK 约束，
// 这里在 Go 侧提前拒绝未知值，避免把拼写错误当成有效审计行写进库。
var auditActions = map[string]struct{}{
	auditActionAdminLogin:                   {},
	auditActionAdminUserCreate:              {},
	auditActionAdminUserDisable:             {},
	auditActionAdminUserEnable:              {},
	auditActionAdminUserPasswordReset:       {},
	auditActionAdminUserExportCooldownReset: {},
	auditActionAdminUserQuotaUpdate:         {},
	auditActionAdminUserEmailUpdate:         {},
	auditActionAdminActivationCodeCreate:    {},
	auditActionAdminActivationCodeRevoke:    {},
	auditActionActivationCodeUsed:           {},
	auditActionAdminImageHide:               {},
	auditActionAdminImageShow:               {},
	auditActionAdminImageDelete:             {},
}

// auditActionValues 按固定顺序返回全部允许动作（供管理端筛选校验使用）。
func auditActionValues() []string {
	return []string{
		auditActionAdminLogin,
		auditActionAdminUserCreate,
		auditActionAdminUserDisable,
		auditActionAdminUserEnable,
		auditActionAdminUserPasswordReset,
		auditActionAdminUserExportCooldownReset,
		auditActionAdminUserQuotaUpdate,
		auditActionAdminUserEmailUpdate,
		auditActionAdminActivationCodeCreate,
		auditActionAdminActivationCodeRevoke,
		auditActionActivationCodeUsed,
		auditActionAdminImageHide,
		auditActionAdminImageShow,
		auditActionAdminImageDelete,
	}
}

// auditActionAllowed 判断动作是否属于允许集合。
func auditActionAllowed(action string) bool {
	_, ok := auditActions[action]
	return ok
}

// coreTimeLayout 与本服务图片/导出使用的时间格式完全一致（毫秒 + 字面 Z，
// 见 images.go 的 imagesTimeLayout）。新表（AuditLog/AccountToken/ActivationCode）
// 与 User.lastExportAt/exportReservationAt 可能由不同 worker 写入，只有格式完全相同时
// SQL 里的 TEXT 大小比较才等价于时间先后；混用秒级 RFC3339 会让同一秒内的比较失真。
const coreTimeLayout = "2006-01-02T15:04:05.000Z"

// coreTimestamp 返回本服务统一的 UTC 毫秒时间戳字符串。
func coreTimestamp(t time.Time) string {
	return t.UTC().Truncate(time.Millisecond).Format(coreTimeLayout)
}

// coreNow 返回当前 UTC 时间戳，格式见 coreTimestamp。
func coreNow() string {
	return coreTimestamp(time.Now())
}

// auditWrite 在调用方事务内写入一行审计日志。
//
// 必须在业务事务的同一个 *sql.Tx 上执行：审计与业务变更要么一起提交、要么一起回滚，
// 不能用 a.db 在事务内部再取连接（本服务数据库只有单连接，会自锁）。
// 未知 action 直接返回错误且不写入。
func auditWrite(ctx context.Context, tx *sql.Tx, actorID int64, action string, targetUserID *int64, detail string) error {
	if tx == nil {
		return fmt.Errorf("写入审计失败: 缺少事务")
	}
	if !auditActionAllowed(action) {
		return fmt.Errorf("写入审计失败: 未知动作 %q", action)
	}
	if actorID <= 0 {
		return fmt.Errorf("写入审计失败: 非法的操作者 %d", actorID)
	}

	var target sql.NullInt64
	if targetUserID != nil {
		if *targetUserID <= 0 {
			return fmt.Errorf("写入审计失败: 非法的目标账号 %d", *targetUserID)
		}
		target = sql.NullInt64{Int64: *targetUserID, Valid: true}
	}

	var detailValue sql.NullString
	if detail != "" {
		detailValue = sql.NullString{String: detail, Valid: true}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "AuditLog" (actorId, action, targetUserId, detail, createdAt)
		 VALUES (?, ?, ?, ?, ?)`,
		actorID, action, target, detailValue, coreNow()); err != nil {
		return fmt.Errorf("写入审计失败: %w", err)
	}
	return nil
}
