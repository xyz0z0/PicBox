package server

// GET/POST /api/admin/users 与 PATCH /api/admin/users/{id}。
//
// 契约与原版 app/api/admin/users/route.ts、app/api/admin/users/[id]/route.ts 一致：
//   - 列表：page/limit(1..100，默认 20)/q(≤64，匹配 email 或 username)；
//     每页带生效配额、覆盖值、存储用量（不过滤 deletedAt）与 _count；
//   - 建号：只需邮箱，固定 USER 角色 + 内部 username + mustChangePassword=true，
//     一次性临时密码只在响应里出现一次，建号与审计同事务；
//   - 五个/六个 operation：disable/enable、reset_password、reset_export_cooldown、
//     set_quota、set_email。所有写入与审计同一事务，审计失败整体回滚。
//
// 与原版的一处有意差异：原版把"不能禁用自己"只放在 UI（行内菜单对当前管理员隐藏），
// 服务端直接调用仍会把唯一管理员锁死。这里在服务端拒绝 disable 自己（403 FORBIDDEN），
// 其余 operation 的边界与原版一致（可以重置自己的密码/改自己的邮箱）。

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// adminMinUserQuotaBytes 与 lib/quota.ts 的 MIN_USER_QUOTA_BYTES 一致：1 MiB。
	adminMinUserQuotaBytes int64 = 1 << 20
	// 一次性临时密码：20 位，去掉 0/O/1/l/I 等易混字符。
	adminTemporaryPasswordLength   = 20
	adminTemporaryPasswordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
)

const (
	adminOperationDisable             = "disable"
	adminOperationEnable              = "enable"
	adminOperationResetPassword       = "reset_password"
	adminOperationResetExportCooldown = "reset_export_cooldown"
	adminOperationSetQuota            = "set_quota"
	adminOperationSetEmail            = "set_email"
)

const adminInvalidOperationMessage = "operation must be disable, enable, reset_password, reset_export_cooldown, set_quota, or set_email"

// ---- 列表 ----

type adminUserCounts struct {
	Images  int64 `json:"images"`
	APIKeys int64 `json:"apiKeys"`
}

type adminUserListItem struct {
	ID                 int64           `json:"id"`
	Email              string          `json:"email"`
	Username           string          `json:"username"`
	Role               string          `json:"role"`
	DisabledAt         *string         `json:"disabledAt"`
	CreatedAt          string          `json:"createdAt"`
	QuotaBytes         int64           `json:"quotaBytes"`
	QuotaOverrideBytes *int64          `json:"quotaOverrideBytes"`
	StorageUsedBytes   int64           `json:"storageUsedBytes"`
	Count              adminUserCounts `json:"_count"`
}

type adminUserSummary struct {
	ID         int64   `json:"id"`
	Email      string  `json:"email"`
	Username   string  `json:"username"`
	Role       string  `json:"role"`
	DisabledAt *string `json:"disabledAt"`
	CreatedAt  string  `json:"createdAt"`
}

type adminUserPasswordSummary struct {
	adminUserSummary
	SessionVersion int64 `json:"sessionVersion"`
}

type adminUsersListResponse struct {
	OK                bool                `json:"ok"`
	QuotaBytes        int64               `json:"quotaBytes"`
	DefaultQuotaBytes int64               `json:"defaultQuotaBytes"`
	Total             int64               `json:"total"`
	Page              int                 `json:"page"`
	PageSize          int                 `json:"pageSize"`
	TotalPages        int                 `json:"totalPages"`
	Users             []adminUserListItem `json:"users"`
}

func (a *App) adminHandleUsersList(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdminSession(w, r); !ok {
		return
	}
	ctx := r.Context()
	paging := adminParsePaging(r.URL.Query(), adminUsersDefaultLimit, adminMaxQueryLength)

	whereSQL := ""
	args := make([]any, 0, 2)
	if paging.Query != "" {
		pattern := "%" + imagesLikeEscape(paging.Query) + "%"
		whereSQL = ` WHERE (u.email LIKE ? ESCAPE '\' OR u.username LIKE ? ESCAPE '\')`
		args = append(args, pattern, pattern)
	}

	var total int64
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "User" u`+whereSQL, args...).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, "USERS_FAILED", "读取账号列表失败")
		return
	}
	page, totalPages := adminClampPage(paging.Page, total, paging.Limit)
	offset := (page - 1) * paging.Limit

	statement := `SELECT u.id, u.email, u.username, u.role, u.disabledAt, u.createdAt, u.quotaBytes,
		(SELECT COUNT(*) FROM "Image" i WHERE i.uploaderId = u.id),
		(SELECT COUNT(*) FROM "ApiKey" k WHERE k.userId = u.id),
		(SELECT COALESCE(SUM(i.size), 0) FROM "Image" i WHERE i.uploaderId = u.id)
		FROM "User" u` + whereSQL + ` ORDER BY u.createdAt DESC, u.id DESC LIMIT ? OFFSET ?`
	queryArgs := append(append([]any{}, args...), paging.Limit, offset)

	rows, err := a.db.QueryContext(ctx, statement, queryArgs...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USERS_FAILED", "读取账号列表失败")
		return
	}
	users := make([]adminUserListItem, 0, paging.Limit)
	for rows.Next() {
		var (
			item       adminUserListItem
			disabledAt sql.NullString
			override   sql.NullInt64
		)
		if err := rows.Scan(&item.ID, &item.Email, &item.Username, &item.Role, &disabledAt, &item.CreatedAt,
			&override, &item.Count.Images, &item.Count.APIKeys, &item.StorageUsedBytes); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, "USERS_FAILED", "读取账号列表失败")
			return
		}
		item.DisabledAt = adminNullableString(disabledAt)
		item.QuotaBytes = a.cfg.UserQuotaBytes
		if override.Valid {
			value := override.Int64
			item.QuotaOverrideBytes = &value
			item.QuotaBytes = value
		}
		users = append(users, item)
	}
	scanErr := rows.Err()
	rows.Close()
	if scanErr != nil {
		writeError(w, http.StatusInternalServerError, "USERS_FAILED", "读取账号列表失败")
		return
	}

	writeJSON(w, http.StatusOK, adminUsersListResponse{
		OK:                true,
		QuotaBytes:        a.cfg.UserQuotaBytes,
		DefaultQuotaBytes: a.cfg.UserQuotaBytes,
		Total:             total,
		Page:              page,
		PageSize:          paging.Limit,
		TotalPages:        totalPages,
		Users:             users,
	})
}

// ---- 建号 ----

type adminUserCreateResponse struct {
	OK                bool             `json:"ok"`
	User              adminCreatedUser `json:"user"`
	TemporaryPassword string           `json:"temporaryPassword"`
}

type adminCreatedUser struct {
	ID                 int64  `json:"id"`
	Email              string `json:"email"`
	Username           string `json:"username"`
	Role               string `json:"role"`
	CreatedAt          string `json:"createdAt"`
	MustChangePassword bool   `json:"mustChangePassword"`
}

func (a *App) adminHandleUsersCreate(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireAdminSession(w, r)
	if !ok {
		return
	}
	body, ok := adminReadJSONObject(w, r, "INVALID_BODY", "请求格式不正确")
	if !ok {
		return
	}

	rawEmail, _ := adminStringField(body, "email")
	email, err := normalizeEmail(rawEmail)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_EMAIL", "请输入有效的邮箱地址")
		return
	}

	createdAt := coreNow()
	username := newInternalUsername()
	temporaryPassword := adminGenerateTemporaryPassword()
	// bcrypt 在事务外完成：本服务数据库只有单连接，事务内做长时间哈希会占住连接。
	passwordHash, err := a.hashPassword(temporaryPassword)
	if err != nil {
		if errors.Is(err, errBcryptBusy) {
			writeRateLimited(w, time.Second)
			return
		}
		writeError(w, http.StatusInternalServerError, "USER_CREATE_FAILED", "创建账号失败，请稍后重试")
		return
	}

	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USER_CREATE_FAILED", "创建账号失败，请稍后重试")
		return
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx,
		`INSERT INTO "User" (email, username, passwordHash, role, disabledAt, sessionVersion, mustChangePassword, quotaBytes, createdAt)
		 VALUES (?, ?, ?, ?, NULL, 0, 1, NULL, ?)`,
		email, username, string(passwordHash), roleUser, createdAt)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			writeError(w, http.StatusConflict, "EMAIL_EXISTS", "该邮箱已被其他账号使用")
			return
		}
		writeError(w, http.StatusInternalServerError, "USER_CREATE_FAILED", "创建账号失败，请稍后重试")
		return
	}
	userID, err := result.LastInsertId()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USER_CREATE_FAILED", "创建账号失败，请稍后重试")
		return
	}

	// 审计与建号同事务：审计写失败时账号一起回滚，不会留下"建了号却拿不到临时密码"的孤儿账号。
	if err := auditWrite(ctx, tx, actor.ID, auditActionAdminUserCreate, &userID,
		fmt.Sprintf("email=%s; role=%s; must_change_password=true", email, roleUser)); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_CREATE_FAILED", "创建账号失败，请稍后重试")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_CREATE_FAILED", "创建账号失败，请稍后重试")
		return
	}

	writeJSON(w, http.StatusOK, adminUserCreateResponse{
		OK: true,
		User: adminCreatedUser{
			ID:                 userID,
			Email:              email,
			Username:           username,
			Role:               roleUser,
			CreatedAt:          createdAt,
			MustChangePassword: true,
		},
		TemporaryPassword: temporaryPassword,
	})
}

// ---- PATCH 分派 ----

func (a *App) adminHandleUserUpdate(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireAdminSession(w, r)
	if !ok {
		return
	}
	targetID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || targetID <= 0 {
		writeError(w, http.StatusBadRequest, "INVALID_USER_ID", "Invalid user id")
		return
	}

	body := adminLooseJSONObject(r)
	operation, _ := adminStringField(body, "operation")
	operation = strings.TrimSpace(operation)

	switch operation {
	case adminOperationResetPassword:
		a.adminUserResetPassword(w, r, actor, targetID, body)
	case adminOperationResetExportCooldown:
		a.adminUserResetExportCooldown(w, r, actor, targetID)
	case adminOperationSetQuota:
		a.adminUserSetQuota(w, r, actor, targetID, body)
	case adminOperationSetEmail:
		a.adminUserSetEmail(w, r, actor, targetID, body)
	case adminOperationDisable, adminOperationEnable:
		a.adminUserSetDisabled(w, r, actor, targetID, operation)
	default:
		writeError(w, http.StatusBadRequest, "INVALID_OPERATION", adminInvalidOperationMessage)
	}
}

// ---- disable / enable ----

func (a *App) adminUserSetDisabled(w http.ResponseWriter, r *http.Request, actor *User, targetID int64, operation string) {
	// 原版把这条边界只放在 UI；服务端拒绝可以避免把唯一管理员（自己）锁在门外。
	if operation == adminOperationDisable && targetID == actor.ID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "不能禁用自己的账号")
		return
	}

	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "更新账号失败，请稍后重试")
		return
	}
	defer func() { _ = tx.Rollback() }()

	var (
		email      string
		username   string
		role       string
		createdAt  string
		disabledAt sql.NullString
	)
	err = tx.QueryRowContext(ctx,
		`SELECT email, username, role, createdAt, disabledAt FROM "User" WHERE id = ?`, targetID).
		Scan(&email, &username, &role, &createdAt, &disabledAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "User not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "更新账号失败，请稍后重试")
		return
	}

	var nextDisabled sql.NullString
	if operation == adminOperationDisable {
		nextDisabled = sql.NullString{String: coreNow(), Valid: true}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE "User" SET disabledAt = ? WHERE id = ?`, nextDisabled, targetID); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "更新账号失败，请稍后重试")
		return
	}

	action := auditActionAdminUserEnable
	if operation == adminOperationDisable {
		action = auditActionAdminUserDisable
	}
	if err := auditWrite(ctx, tx, actor.ID, action, &targetID, ""); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "更新账号失败，请稍后重试")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "更新账号失败，请稍后重试")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"user": adminUserSummary{
			ID:         targetID,
			Email:      email,
			Username:   username,
			Role:       role,
			DisabledAt: adminNullableString(nextDisabled),
			CreatedAt:  createdAt,
		},
	})
}

// ---- reset_password ----

func (a *App) adminUserResetPassword(w http.ResponseWriter, r *http.Request, actor *User, targetID int64, body map[string]json.RawMessage) {
	password, _ := adminStringField(body, "password")
	if err := validatePassword(password); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PASSWORD", err.Error())
		return
	}
	passwordHash, err := a.hashPassword(password)
	if err != nil {
		if errors.Is(err, errBcryptBusy) {
			writeRateLimited(w, time.Second)
			return
		}
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "重置密码失败，请稍后重试")
		return
	}

	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "重置密码失败，请稍后重试")
		return
	}
	defer func() { _ = tx.Rollback() }()

	var exists int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM "User" WHERE id = ?`, targetID).Scan(&exists); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "重置密码失败，请稍后重试")
		return
	}
	if exists == 0 {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "User not found")
		return
	}

	// 递增 sessionVersion：此前签发的 JWT 在下一次请求时全部失效。
	if _, err := tx.ExecContext(ctx,
		`UPDATE "User" SET passwordHash = ?, sessionVersion = sessionVersion + 1 WHERE id = ?`,
		string(passwordHash), targetID); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "重置密码失败，请稍后重试")
		return
	}
	// 密码重置同时撤销该账号全部仍有效的 API Key。
	revokeResult, err := tx.ExecContext(ctx,
		`UPDATE "ApiKey" SET revokedAt = ? WHERE userId = ? AND revokedAt IS NULL`, coreNow(), targetID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "重置密码失败，请稍后重试")
		return
	}
	revokedAPIKeys, err := revokeResult.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "重置密码失败，请稍后重试")
		return
	}

	var (
		email      string
		username   string
		role       string
		createdAt  string
		version    int64
		disabledAt sql.NullString
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT email, username, role, createdAt, disabledAt, sessionVersion FROM "User" WHERE id = ?`, targetID).
		Scan(&email, &username, &role, &createdAt, &disabledAt, &version); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "重置密码失败，请稍后重试")
		return
	}

	if err := auditWrite(ctx, tx, actor.ID, auditActionAdminUserPasswordReset, &targetID,
		fmt.Sprintf("session_version=%d; api_keys_revoked=%d", version, revokedAPIKeys)); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "重置密码失败，请稍后重试")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "重置密码失败，请稍后重试")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"user": adminUserPasswordSummary{
			adminUserSummary: adminUserSummary{
				ID:         targetID,
				Email:      email,
				Username:   username,
				Role:       role,
				DisabledAt: adminNullableString(disabledAt),
				CreatedAt:  createdAt,
			},
			SessionVersion: version,
		},
		"revokedApiKeys": revokedAPIKeys,
	})
}

// ---- reset_export_cooldown ----

type adminCooldownUser struct {
	ID         int64   `json:"id"`
	Username   string  `json:"username"`
	Role       string  `json:"role"`
	DisabledAt *string `json:"disabledAt"`
	CreatedAt  string  `json:"createdAt"`
}

type adminExportCooldownStatus struct {
	// Reset 必须反映这次调用是否真的解除了限制：账号本来没有冷却时为 false。
	Reset                bool    `json:"reset"`
	PreviousLastExportAt *string `json:"previousLastExportAt"`
	ReservationActive    bool    `json:"reservationActive"`
	ReservationExpiresAt *string `json:"reservationExpiresAt"`
	RetryAfterSeconds    *int    `json:"retryAfterSeconds"`
}

func (a *App) adminUserResetExportCooldown(w http.ResponseWriter, r *http.Request, actor *User, targetID int64) {
	ctx := r.Context()
	now := time.Now().UTC()

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "解除导出限制失败，请稍后重试")
		return
	}
	defer func() { _ = tx.Rollback() }()

	var (
		username      string
		role          string
		createdAt     string
		disabledAt    sql.NullString
		lastExportAt  sql.NullString
		reservationAt sql.NullString
	)
	err = tx.QueryRowContext(ctx,
		`SELECT username, role, createdAt, disabledAt, lastExportAt, exportReservationAt FROM "User" WHERE id = ?`, targetID).
		Scan(&username, &role, &createdAt, &disabledAt, &lastExportAt, &reservationAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "User not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "解除导出限制失败，请稍后重试")
		return
	}

	previousReservationActive := false
	if reservationAt.Valid {
		if at, ok := imagesParseTime(reservationAt.String); ok && at.Add(imagesExportLease).After(now) {
			previousReservationActive = true
		}
	}

	if previousReservationActive {
		// 预约仍在租约内：说明确实有导出正在进行，只清成功冷却，绝不打断它。
		if _, err := tx.ExecContext(ctx, `UPDATE "User" SET lastExportAt = NULL WHERE id = ?`, targetID); err != nil {
			writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "解除导出限制失败，请稍后重试")
			return
		}
	} else {
		// 条件 UPDATE 带 CAS（比对读到的预约值）：与"刚 claim 成功"的并发导出互不污染。
		statement := `UPDATE "User" SET lastExportAt = NULL, exportReservationAt = NULL, exportReservationToken = NULL WHERE id = ?`
		updateArgs := []any{targetID}
		if reservationAt.Valid {
			statement += ` AND exportReservationAt = ?`
			updateArgs = append(updateArgs, reservationAt.String)
		} else {
			statement += ` AND exportReservationAt IS NULL`
		}
		result, updateErr := tx.ExecContext(ctx, statement, updateArgs...)
		if updateErr != nil {
			writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "解除导出限制失败，请稍后重试")
			return
		}
		affected, affectedErr := result.RowsAffected()
		if affectedErr != nil {
			writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "解除导出限制失败，请稍后重试")
			return
		}
		if affected == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE "User" SET lastExportAt = NULL WHERE id = ?`, targetID); err != nil {
				writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "解除导出限制失败，请稍后重试")
				return
			}
		}
	}

	var updatedReservation sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT exportReservationAt FROM "User" WHERE id = ?`, targetID).Scan(&updatedReservation); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "解除导出限制失败，请稍后重试")
		return
	}

	reservationActive := false
	var reservationExpiresAt *string
	var retryAfterSeconds *int
	if updatedReservation.Valid {
		if at, ok := imagesParseTime(updatedReservation.String); ok {
			expires := at.Add(imagesExportLease)
			if expires.After(now) {
				reservationActive = true
				formatted := expires.UTC().Format(imagesTimeLayout)
				reservationExpiresAt = &formatted
				seconds := int(math.Ceil(expires.Sub(now).Seconds()))
				if seconds < 1 {
					seconds = 1
				}
				retryAfterSeconds = &seconds
			}
		}
	}

	changed := lastExportAt.Valid || (reservationAt.Valid && !previousReservationActive)

	var previousLastExportAt *string
	if lastExportAt.Valid {
		formatted := lastExportAt.String
		if parsed, ok := imagesParseTime(lastExportAt.String); ok {
			formatted = parsed.UTC().Format(imagesTimeLayout)
		}
		previousLastExportAt = &formatted
	}

	reservationDetail := "cleared"
	if reservationActive {
		reservationDetail = "kept_active"
	}
	if err := auditWrite(ctx, tx, actor.ID, auditActionAdminUserExportCooldownReset, &targetID,
		fmt.Sprintf("lastExportAt=%s; reservation=%s; changed=%t",
			adminDetailValue(previousLastExportAt), reservationDetail, changed)); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "解除导出限制失败，请稍后重试")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "解除导出限制失败，请稍后重试")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"user": adminCooldownUser{
			ID:         targetID,
			Username:   username,
			Role:       role,
			DisabledAt: adminNullableString(disabledAt),
			CreatedAt:  createdAt,
		},
		"exportCooldown": adminExportCooldownStatus{
			Reset:                changed,
			PreviousLastExportAt: previousLastExportAt,
			ReservationActive:    reservationActive,
			ReservationExpiresAt: reservationExpiresAt,
			RetryAfterSeconds:    retryAfterSeconds,
		},
	})
}

// ---- set_quota ----

// adminParseQuotaOverride 复刻原版 parseUserQuotaOverride：
//   - 缺字段 / null 之外的非整数（小数、负数、字符串、布尔）→ 非法；
//   - null → 继承全局配额；
//   - 0 → 该账号不限；
//   - 正整数 → 专属上限，低于 1MiB 抬到 1MiB。
func adminParseQuotaOverride(body map[string]json.RawMessage) (*int64, bool) {
	raw, present := body["quotaBytes"]
	if !present {
		return nil, false
	}
	if strings.EqualFold(strings.TrimSpace(string(raw)), "null") {
		return nil, true
	}
	value, ok := adminParseJSONInt(raw)
	if !ok || value < 0 {
		return nil, false
	}
	if value == 0 {
		zero := int64(0)
		return &zero, true
	}
	if value < adminMinUserQuotaBytes {
		value = adminMinUserQuotaBytes
	}
	return &value, true
}

func (a *App) adminUserSetQuota(w http.ResponseWriter, r *http.Request, actor *User, targetID int64, body map[string]json.RawMessage) {
	override, ok := adminParseQuotaOverride(body)
	if !ok {
		writeError(w, http.StatusBadRequest, "INVALID_QUOTA",
			"quotaBytes must be null, 0, or an integer >= 1048576")
		return
	}

	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "保存配额失败，请稍后重试")
		return
	}
	defer func() { _ = tx.Rollback() }()

	var previous sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT quotaBytes FROM "User" WHERE id = ?`, targetID).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "User not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "保存配额失败，请稍后重试")
		return
	}

	changed := true
	switch {
	case override == nil && !previous.Valid:
		changed = false
	case override != nil && previous.Valid && previous.Int64 == *override:
		changed = false
	}
	if changed {
		var next any
		if override != nil {
			next = *override
		}
		if _, err := tx.ExecContext(ctx, `UPDATE "User" SET quotaBytes = ? WHERE id = ?`, next, targetID); err != nil {
			writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "保存配额失败，请稍后重试")
			return
		}
	}

	// 用量包含回收站与管理员下架：物理删除前仍占磁盘，口径与 quotaUsedBytes 一致。
	var usedBytes int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size), 0) FROM "Image" WHERE uploaderId = ?`, targetID).Scan(&usedBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "保存配额失败，请稍后重试")
		return
	}

	effectiveBytes := a.cfg.UserQuotaBytes
	if override != nil {
		effectiveBytes = *override
	}
	exceeded := effectiveBytes > 0 && usedBytes > effectiveBytes

	if err := auditWrite(ctx, tx, actor.ID, auditActionAdminUserQuotaUpdate, &targetID,
		fmt.Sprintf("previous=%s; quotaBytes=%s; effective=%d; changed=%t",
			adminQuotaDetail(previous), adminQuotaOverrideDetail(override), effectiveBytes, changed)); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "保存配额失败，请稍后重试")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "保存配额失败，请稍后重试")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"quota": map[string]any{
			"changed":        changed,
			"effectiveBytes": effectiveBytes,
			"overrideBytes":  override,
			"usedBytes":      usedBytes,
			"exceeded":       exceeded,
		},
	})
}

// ---- set_email ----

func (a *App) adminUserSetEmail(w http.ResponseWriter, r *http.Request, actor *User, targetID int64, body map[string]json.RawMessage) {
	rawEmail, _ := adminStringField(body, "email")
	email, err := normalizeEmail(rawEmail)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_EMAIL", "请输入有效的邮箱地址")
		return
	}

	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "保存邮箱失败，请稍后重试")
		return
	}
	defer func() { _ = tx.Rollback() }()

	var previous string
	err = tx.QueryRowContext(ctx, `SELECT email FROM "User" WHERE id = ?`, targetID).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "User not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "保存邮箱失败，请稍后重试")
		return
	}

	changed := previous != email
	if changed {
		var conflict int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM "User" WHERE email = ? AND id <> ?`, email, targetID).Scan(&conflict); err != nil {
			writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "保存邮箱失败，请稍后重试")
			return
		}
		if conflict > 0 {
			writeError(w, http.StatusConflict, "EMAIL_EXISTS", "该邮箱已被其他账号使用")
			return
		}
		if _, err := tx.ExecContext(ctx, `UPDATE "User" SET email = ? WHERE id = ?`, email, targetID); err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				writeError(w, http.StatusConflict, "EMAIL_EXISTS", "该邮箱已被其他账号使用")
				return
			}
			writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "保存邮箱失败，请稍后重试")
			return
		}
		// 登录标识变了，此前签发的一次性令牌（找回密码/改邮箱）全部作废。
		if _, err := tx.ExecContext(ctx, `DELETE FROM "AccountToken" WHERE userId = ?`, targetID); err != nil {
			writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "保存邮箱失败，请稍后重试")
			return
		}
	}

	if err := auditWrite(ctx, tx, actor.ID, auditActionAdminUserEmailUpdate, &targetID,
		fmt.Sprintf("previous=%s; email=%s; changed=%t", previous, email, changed)); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "保存邮箱失败，请稍后重试")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "USER_UPDATE_FAILED", "保存邮箱失败，请稍后重试")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"email":   email,
		"changed": changed,
	})
}

// ---- 小工具 ----

// adminGenerateTemporaryPassword 生成 20 位一次性临时密码（与原版同字母表/同长度）。
func adminGenerateTemporaryPassword() string {
	buffer := make([]byte, adminTemporaryPasswordLength)
	if _, err := rand.Read(buffer); err != nil {
		// crypto/rand 失败时退化为时间派生值没有意义：这里用固定长度占位并让登录必然失败
		// 也不是可接受的语义。安全随机不可用时只能 panic，由 recoverPanics 返回 500。
		panic("crypto/rand 不可用: " + err.Error())
	}
	password := make([]byte, adminTemporaryPasswordLength)
	for index, value := range buffer {
		password[index] = adminTemporaryPasswordAlphabet[int(value)%len(adminTemporaryPasswordAlphabet)]
	}
	return string(password)
}

func adminNullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	text := value.String
	return &text
}

func adminDetailValue(value *string) string {
	if value == nil {
		return "null"
	}
	return *value
}

func adminQuotaDetail(value sql.NullInt64) string {
	if !value.Valid {
		return "inherit"
	}
	return strconv.FormatInt(value.Int64, 10)
}

func adminQuotaOverrideDetail(value *int64) string {
	if value == nil {
		return "inherit"
	}
	return strconv.FormatInt(*value, 10)
}
