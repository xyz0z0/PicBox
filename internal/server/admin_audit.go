package server

// GET /api/admin/audit-logs：管理端审计日志（UI：AdminClient.tsx 的 AdminAuditLogsTab）。
//
// 契约与原版 app/api/admin/audit-logs/route.ts 一致：
//   - action 为 all 或 audit.go 里的 14 个动作之一，非法 → 400 INVALID_ACTION；
//   - q 匹配 detail 以及操作者/目标账号的 email、username；
//   - 游标分页、createdAt desc；这里在游标里额外带上 id，保证同一秒的多行审计
//     （一次请求可能写多条）在页边界不重复也不丢失。
//
// 只读接口：不写审计，也不修改任何状态。

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
)

type adminAuditUser struct {
	ID       int64  `json:"id"`
	Email    string `json:"email"`
	Username string `json:"username"`
}

type adminAuditLogItem struct {
	ID         int64           `json:"id"`
	Action     string          `json:"action"`
	Detail     *string         `json:"detail"`
	CreatedAt  string          `json:"createdAt"`
	Actor      adminAuditUser  `json:"actor"`
	TargetUser *adminAuditUser `json:"targetUser"`
}

type adminAuditLogsResponse struct {
	OK         bool                `json:"ok"`
	Logs       []adminAuditLogItem `json:"logs"`
	NextCursor *string             `json:"nextCursor"`
}

func (a *App) adminHandleAuditLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdminSession(w, r); !ok {
		return
	}
	ctx := r.Context()
	query := r.URL.Query()
	paging := adminParsePaging(query, adminAuditDefaultLimit, adminMaxSearchLength)

	action, ok := adminAuditActionParam(query)
	if !ok {
		writeError(w, http.StatusBadRequest, "INVALID_ACTION", "unknown audit action")
		return
	}

	conditions := make([]string, 0, 3)
	args := make([]any, 0, 6)
	if action != "all" {
		conditions = append(conditions, `log.action = ?`)
		args = append(args, action)
	}
	if paging.Query != "" {
		pattern := "%" + imagesLikeEscape(paging.Query) + "%"
		conditions = append(conditions,
			`(log.detail LIKE ? ESCAPE '\'
			  OR actor.email LIKE ? ESCAPE '\' OR actor.username LIKE ? ESCAPE '\'
			  OR target.email LIKE ? ESCAPE '\' OR target.username LIKE ? ESCAPE '\')`)
		args = append(args, pattern, pattern, pattern, pattern, pattern)
	}
	if createdAt, id, ok := adminParseCursor(query.Get("cursor")); ok {
		if numericID, valid := adminCursorIDInt(id); valid {
			conditions = append(conditions, `(log.createdAt < ? OR (log.createdAt = ? AND log.id < ?))`)
			args = append(args, createdAt, createdAt, numericID)
		} else {
			conditions = append(conditions, `log.createdAt < ?`)
			args = append(args, createdAt)
		}
	}

	whereSQL := ""
	if len(conditions) > 0 {
		whereSQL = " WHERE " + strings.Join(conditions, " AND ")
	}

	statement := `SELECT log.id, log.action, log.detail, log.createdAt,
		actor.id, actor.email, actor.username,
		target.id, target.email, target.username
		FROM "AuditLog" log
		JOIN "User" actor ON actor.id = log.actorId
		LEFT JOIN "User" target ON target.id = log.targetUserId` +
		whereSQL + ` ORDER BY log.createdAt DESC, log.id DESC LIMIT ?`
	queryArgs := append(append([]any{}, args...), paging.Limit)

	rows, err := a.db.QueryContext(ctx, statement, queryArgs...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "AUDIT_LOGS_FAILED", "读取审计日志失败")
		return
	}
	logs := make([]adminAuditLogItem, 0, paging.Limit)
	for rows.Next() {
		var (
			item       adminAuditLogItem
			detail     sql.NullString
			targetID   sql.NullInt64
			targetMail sql.NullString
			targetName sql.NullString
		)
		if err := rows.Scan(&item.ID, &item.Action, &detail, &item.CreatedAt,
			&item.Actor.ID, &item.Actor.Email, &item.Actor.Username,
			&targetID, &targetMail, &targetName); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, "AUDIT_LOGS_FAILED", "读取审计日志失败")
			return
		}
		item.Detail = adminNullableString(detail)
		if targetID.Valid {
			item.TargetUser = &adminAuditUser{
				ID:       targetID.Int64,
				Email:    targetMail.String,
				Username: targetName.String,
			}
		}
		logs = append(logs, item)
	}
	scanErr := rows.Err()
	rows.Close()
	if scanErr != nil {
		writeError(w, http.StatusInternalServerError, "AUDIT_LOGS_FAILED", "读取审计日志失败")
		return
	}

	var nextCursor *string
	if len(logs) == paging.Limit && len(logs) > 0 {
		last := logs[len(logs)-1]
		cursor := adminEncodeCursor(last.CreatedAt, strconv.FormatInt(last.ID, 10))
		nextCursor = &cursor
	}

	writeJSON(w, http.StatusOK, adminAuditLogsResponse{OK: true, Logs: logs, NextCursor: nextCursor})
}
