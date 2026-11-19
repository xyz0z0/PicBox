package server

// 注册激活码的管理端接口（UI：app/admin/ActivationCodesTab.tsx）：
//
//	GET   /api/admin/activation-codes        分页 + 状态筛选 + 搜索（掩码/备注）
//	POST  /api/admin/activation-codes        批量生成 1..50 个，完整码只在响应里出现一次
//	PATCH /api/admin/activation-codes/{id}   撤销（已使用 → 409；重复撤销 → changed=false）
//
// 与注册消费逻辑（auth_register.go 的 normalizeActivationCode / hashActivationCode）保持同一
// 规范：库里只存 sha256 哈希，生成时同样去掉分隔符并转大写；注册因此可以直接消费这里生成的码。

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	adminActivationDefaultLimit   = 20
	adminActivationMaxCount       = 50
	adminActivationMaxExpiresDays = 3650
	adminActivationDefaultExpires = 7
	adminActivationMaxNoteLength  = 200
	// 与原版 lib/activationCode.ts 相同的字母表与长度：去掉 I/L/O/U/0/1，避免手抄歧义。
	adminActivationAlphabet  = "ABCDEFGHJKMNPQRSTVWXYZ23456789"
	adminActivationRawLength = 20
)

type adminActivationCodeUser struct {
	ID       int64  `json:"id"`
	Email    string `json:"email"`
	Username string `json:"username"`
}

type adminActivationCodeItem struct {
	ID         string                   `json:"id"`
	MaskedCode string                   `json:"maskedCode"`
	Note       *string                  `json:"note"`
	CreatedAt  string                   `json:"createdAt"`
	ExpiresAt  *string                  `json:"expiresAt"`
	UsedAt     *string                  `json:"usedAt"`
	RevokedAt  *string                  `json:"revokedAt"`
	CreatedBy  adminActivationCodeUser  `json:"createdBy"`
	UsedBy     *adminActivationCodeUser `json:"usedBy"`
}

type adminActivationCodesListResponse struct {
	OK         bool                      `json:"ok"`
	Total      int64                     `json:"total"`
	Page       int                       `json:"page"`
	PageSize   int                       `json:"pageSize"`
	TotalPages int                       `json:"totalPages"`
	Codes      []adminActivationCodeItem `json:"codes"`
}

// adminActivationStatusWhere 复刻原版状态定义。
//
//	unused  未使用 + 未撤销 + （无过期或未过期）
//	used    已使用（撤销与否不影响）
//	revoked 已撤销
//	expired 未使用 + 未撤销 + 已过期
func adminActivationStatusWhere(status, now string) (string, []any, bool) {
	switch status {
	case "all":
		return "", nil, true
	case "unused":
		return "c.usedAt IS NULL AND c.revokedAt IS NULL AND (c.expiresAt IS NULL OR c.expiresAt > ?)",
			[]any{now}, true
	case "used":
		return "c.usedAt IS NOT NULL", nil, true
	case "revoked":
		return "c.revokedAt IS NOT NULL", nil, true
	case "expired":
		return "c.usedAt IS NULL AND c.revokedAt IS NULL AND c.expiresAt IS NOT NULL AND c.expiresAt <= ?",
			[]any{now}, true
	default:
		return "", nil, false
	}
}

func (a *App) adminHandleActivationCodesList(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAdminSession(w, r); !ok {
		return
	}
	ctx := r.Context()
	query := r.URL.Query()
	paging := adminParsePaging(query, adminActivationDefaultLimit, adminMaxQueryLength)

	status := strings.TrimSpace(query.Get("status"))
	if status == "" {
		status = "all"
	}
	now := coreNow()
	statusSQL, statusArgs, ok := adminActivationStatusWhere(status, now)
	if !ok {
		writeError(w, http.StatusBadRequest, "INVALID_STATUS", "status 参数无效")
		return
	}

	conditions := make([]string, 0, 2)
	args := make([]any, 0, 4)
	if statusSQL != "" {
		conditions = append(conditions, statusSQL)
		args = append(args, statusArgs...)
	}
	if paging.Query != "" {
		pattern := "%" + imagesLikeEscape(paging.Query) + "%"
		conditions = append(conditions, `(c.maskedCode LIKE ? ESCAPE '\' OR c.note LIKE ? ESCAPE '\')`)
		args = append(args, pattern, pattern)
	}
	whereSQL := ""
	if len(conditions) > 0 {
		whereSQL = " WHERE " + strings.Join(conditions, " AND ")
	}

	var total int64
	if err := a.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM "ActivationCode" c`+whereSQL, args...).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, "ACTIVATION_CODES_FAILED", "读取激活码列表失败")
		return
	}
	page, totalPages := adminClampPage(paging.Page, total, paging.Limit)
	offset := (page - 1) * paging.Limit

	statement := `SELECT c.id, c.maskedCode, c.note, c.createdAt, c.expiresAt, c.usedAt, c.revokedAt,
		creator.id, creator.email, creator.username,
		used.id, used.email, used.username
		FROM "ActivationCode" c
		JOIN "User" creator ON creator.id = c.createdById
		LEFT JOIN "User" used ON used.id = c.usedById` +
		whereSQL + ` ORDER BY c.createdAt DESC, c.id DESC LIMIT ? OFFSET ?`
	queryArgs := append(append([]any{}, args...), paging.Limit, offset)

	rows, err := a.db.QueryContext(ctx, statement, queryArgs...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ACTIVATION_CODES_FAILED", "读取激活码列表失败")
		return
	}
	codes := make([]adminActivationCodeItem, 0, paging.Limit)
	for rows.Next() {
		var (
			item                    adminActivationCodeItem
			note, expiresAt, usedAt sql.NullString
			revokedAt               sql.NullString
			usedID                  sql.NullInt64
			usedEmail, usedUsername sql.NullString
		)
		if err := rows.Scan(&item.ID, &item.MaskedCode, &note, &item.CreatedAt, &expiresAt, &usedAt, &revokedAt,
			&item.CreatedBy.ID, &item.CreatedBy.Email, &item.CreatedBy.Username,
			&usedID, &usedEmail, &usedUsername); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, "ACTIVATION_CODES_FAILED", "读取激活码列表失败")
			return
		}
		item.Note = adminNullableString(note)
		item.ExpiresAt = adminNullableString(expiresAt)
		item.UsedAt = adminNullableString(usedAt)
		item.RevokedAt = adminNullableString(revokedAt)
		if usedID.Valid {
			item.UsedBy = &adminActivationCodeUser{
				ID:       usedID.Int64,
				Email:    usedEmail.String,
				Username: usedUsername.String,
			}
		}
		codes = append(codes, item)
	}
	scanErr := rows.Err()
	rows.Close()
	if scanErr != nil {
		writeError(w, http.StatusInternalServerError, "ACTIVATION_CODES_FAILED", "读取激活码列表失败")
		return
	}

	writeJSON(w, http.StatusOK, adminActivationCodesListResponse{
		OK:         true,
		Total:      total,
		Page:       page,
		PageSize:   paging.Limit,
		TotalPages: totalPages,
		Codes:      codes,
	})
}

type adminGeneratedActivationCode struct {
	Code       string  `json:"code"`
	MaskedCode string  `json:"maskedCode"`
	ExpiresAt  *string `json:"expiresAt"`
}

type adminActivationCodesCreateResponse struct {
	OK    bool                           `json:"ok"`
	Codes []adminGeneratedActivationCode `json:"codes"`
}

func (a *App) adminHandleActivationCodesCreate(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireAdminSession(w, r)
	if !ok {
		return
	}
	body, ok := adminReadJSONObject(w, r, "INVALID_BODY", "请求格式不正确")
	if !ok {
		return
	}

	count, ok := adminIntField(body, "count")
	if !ok || count < 1 || count > adminActivationMaxCount {
		writeError(w, http.StatusBadRequest, "INVALID_COUNT",
			fmt.Sprintf("数量必须是 1-%d 的整数", adminActivationMaxCount))
		return
	}

	expiresInDays := int64(adminActivationDefaultExpires)
	if raw, present := body["expiresInDays"]; present {
		// 原版对 null 取 Number(null)=0（=永久）；其余非整数一律 400。
		if strings.EqualFold(strings.TrimSpace(string(raw)), "null") {
			expiresInDays = 0
		} else {
			parsed, valid := adminParseJSONInt(raw)
			if !valid {
				writeError(w, http.StatusBadRequest, "INVALID_EXPIRES",
					fmt.Sprintf("有效期必须是 0-%d 天的整数（0 表示永久）", adminActivationMaxExpiresDays))
				return
			}
			expiresInDays = parsed
		}
	}
	if expiresInDays < 0 || expiresInDays > adminActivationMaxExpiresDays {
		writeError(w, http.StatusBadRequest, "INVALID_EXPIRES",
			fmt.Sprintf("有效期必须是 0-%d 天的整数（0 表示永久）", adminActivationMaxExpiresDays))
		return
	}

	note := ""
	if raw, _ := adminStringField(body, "note"); raw != "" {
		runes := []rune(strings.TrimSpace(raw))
		if len(runes) > adminActivationMaxNoteLength {
			runes = runes[:adminActivationMaxNoteLength]
		}
		note = string(runes)
	}
	var noteValue sql.NullString
	if note != "" {
		noteValue = sql.NullString{String: note, Valid: true}
	}

	now := time.Now().UTC()
	createdAt := coreTimestamp(now)
	var expiresAt sql.NullString
	if expiresInDays > 0 {
		expiresAt = sql.NullString{
			String: coreTimestamp(now.Add(time.Duration(expiresInDays) * 24 * time.Hour)),
			Valid:  true,
		}
	}

	generated := make([]adminGeneratedActivationCode, 0, count)
	type pendingCode struct {
		id         string
		codeHash   string
		maskedCode string
	}
	pending := make([]pendingCode, 0, count)
	for index := int64(0); index < count; index++ {
		code := adminGenerateActivationCode()
		normalized := normalizeActivationCode(code)
		generated = append(generated, adminGeneratedActivationCode{
			Code:       code,
			MaskedCode: adminMaskActivationCode(normalized),
			ExpiresAt:  adminNullableString(expiresAt),
		})
		pending = append(pending, pendingCode{
			id:         uuid.NewString(),
			codeHash:   hashActivationCode(normalized),
			maskedCode: adminMaskActivationCode(normalized),
		})
	}

	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ACTIVATION_CODES_FAILED", "生成激活码失败，请稍后重试")
		return
	}
	defer func() { _ = tx.Rollback() }()

	for _, entry := range pending {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "ActivationCode" (id, codeHash, maskedCode, note, createdById, createdAt, expiresAt)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			entry.id, entry.codeHash, entry.maskedCode, noteValue, actor.ID, createdAt, expiresAt); err != nil {
			writeError(w, http.StatusInternalServerError, "ACTIVATION_CODES_FAILED", "生成激活码失败，请稍后重试")
			return
		}
	}

	expiresDetail := "never"
	if expiresAt.Valid {
		expiresDetail = expiresAt.String
	}
	if err := auditWrite(ctx, tx, actor.ID, auditActionAdminActivationCodeCreate, nil,
		fmt.Sprintf("count=%d; expiresAt=%s; note=%s", count, expiresDetail, note)); err != nil {
		writeError(w, http.StatusInternalServerError, "ACTIVATION_CODES_FAILED", "生成激活码失败，请稍后重试")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "ACTIVATION_CODES_FAILED", "生成激活码失败，请稍后重试")
		return
	}

	writeJSON(w, http.StatusOK, adminActivationCodesCreateResponse{OK: true, Codes: generated})
}

func (a *App) adminHandleActivationCodeRevoke(w http.ResponseWriter, r *http.Request) {
	actor, ok := a.requireAdminSession(w, r)
	if !ok {
		return
	}
	codeID := strings.TrimSpace(r.PathValue("id"))
	if codeID == "" {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Activation code not found")
		return
	}

	body := adminLooseJSONObject(r)
	operation, _ := adminStringField(body, "operation")
	if strings.TrimSpace(operation) != "revoke" {
		writeError(w, http.StatusBadRequest, "INVALID_OPERATION", "operation must be revoke")
		return
	}

	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ACTIVATION_CODES_FAILED", "撤销激活码失败，请稍后重试")
		return
	}
	defer func() { _ = tx.Rollback() }()

	var usedAt, revokedAt sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT usedAt, revokedAt FROM "ActivationCode" WHERE id = ?`, codeID).Scan(&usedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Activation code not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ACTIVATION_CODES_FAILED", "撤销激活码失败，请稍后重试")
		return
	}
	if usedAt.Valid {
		writeError(w, http.StatusConflict, "ACTIVATION_CODE_USED", "已使用的激活码不能撤销")
		return
	}

	changed := !revokedAt.Valid
	if changed {
		if _, err := tx.ExecContext(ctx,
			`UPDATE "ActivationCode" SET revokedAt = ? WHERE id = ? AND revokedAt IS NULL`,
			coreNow(), codeID); err != nil {
			writeError(w, http.StatusInternalServerError, "ACTIVATION_CODES_FAILED", "撤销激活码失败，请稍后重试")
			return
		}
	}

	if err := auditWrite(ctx, tx, actor.ID, auditActionAdminActivationCodeRevoke, nil,
		fmt.Sprintf("codeId=%s; changed=%t", codeID, changed)); err != nil {
		writeError(w, http.StatusInternalServerError, "ACTIVATION_CODES_FAILED", "撤销激活码失败，请稍后重试")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "ACTIVATION_CODES_FAILED", "撤销激活码失败，请稍后重试")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": changed})
}

// adminGenerateActivationCode 生成形如 ABCD-EFGH-JKMN-PQRS-TVWX 的 20 位激活码。
func adminGenerateActivationCode() string {
	buffer := make([]byte, adminActivationRawLength)
	if _, err := rand.Read(buffer); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	raw := make([]byte, adminActivationRawLength)
	for index, value := range buffer {
		raw[index] = adminActivationAlphabet[int(value)%len(adminActivationAlphabet)]
	}
	groups := make([]string, 0, adminActivationRawLength/4)
	for start := 0; start < adminActivationRawLength; start += 4 {
		groups = append(groups, string(raw[start:start+4]))
	}
	return strings.Join(groups, "-")
}

// adminMaskActivationCode 只用于列表展示：完整码在生成响应之后不再出现。
func adminMaskActivationCode(normalized string) string {
	if len(normalized) <= 8 {
		prefix := normalized
		if len(prefix) > 2 {
			prefix = prefix[:2]
		}
		return prefix + "-****"
	}
	return normalized[:4] + "-****-****-****-" + normalized[len(normalized)-4:]
}
