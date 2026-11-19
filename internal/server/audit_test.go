package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"
)

// ---- helpers for audit tests ----

func coreAuditTx(t *testing.T, app *App) *sql.Tx {
	t.Helper()
	tx, err := app.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	return tx
}

func coreAuditCount(t *testing.T, app *App) int {
	t.Helper()
	var count int
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM "AuditLog"`).Scan(&count); err != nil {
		t.Fatalf("统计审计失败: %v", err)
	}
	return count
}

func TestAuditActionSetIsTheFourteenOriginalActions(t *testing.T) {
	values := auditActionValues()
	if len(values) != 14 {
		t.Fatalf("审计动作必须恰好 14 个，实际 %d: %v", len(values), values)
	}
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] {
			t.Fatalf("审计动作重复: %s", value)
		}
		seen[value] = true
		if !auditActionAllowed(value) {
			t.Fatalf("%s 应在允许集合内", value)
		}
	}
	for _, value := range []string{"ADMIN_LOGIN", "ACTIVATION_CODE_USED", "ADMIN_IMAGE_DELETE"} {
		if !seen[value] {
			t.Fatalf("缺少原版动作 %s", value)
		}
	}
	if auditActionAllowed("ADMIN_SOMETHING_ELSE") {
		t.Fatalf("未知动作不得放行")
	}
}

func TestAuditWritePersistsRowsAndRejectsUnknownActions(t *testing.T) {
	app := coreTestApp(t, Config{})
	coreRegister(t, app, "auditor@example.com", "secret123")
	actorID := int64(1)
	targetID := int64(1)

	tx := coreAuditTx(t, app)
	ctx := context.Background()
	if err := auditWrite(ctx, tx, actorID, auditActionAdminUserDisable, &targetID, "reason=test"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("写入审计失败: %v", err)
	}
	if err := auditWrite(ctx, tx, actorID, auditActionAdminLogin, nil, ""); err != nil {
		_ = tx.Rollback()
		t.Fatalf("写入无目标审计失败: %v", err)
	}
	// 未知动作必须拒绝，且不写入。
	if err := auditWrite(ctx, tx, actorID, "ADMIN_NOT_REAL", nil, ""); err == nil {
		_ = tx.Rollback()
		t.Fatalf("未知动作必须报错")
	}
	// 非法操作者必须拒绝。
	if err := auditWrite(ctx, tx, 0, auditActionAdminLogin, nil, ""); err == nil {
		_ = tx.Rollback()
		t.Fatalf("操作者为 0 必须报错")
	}
	// 非法目标账号必须拒绝。
	invalidTarget := int64(0)
	if err := auditWrite(ctx, tx, actorID, auditActionAdminUserEnable, &invalidTarget, ""); err == nil {
		_ = tx.Rollback()
		t.Fatalf("目标账号为 0 必须报错")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交审计失败: %v", err)
	}

	var (
		action   string
		target   sql.NullInt64
		detail   sql.NullString
		created  string
		auditAct string
		auditTar sql.NullInt64
	)
	if err := app.db.QueryRow(
		`SELECT action, targetUserId, detail, createdAt FROM "AuditLog" WHERE action = ?`,
		auditActionAdminUserDisable).Scan(&action, &target, &detail, &created); err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	if action != auditActionAdminUserDisable || !target.Valid || target.Int64 != targetID {
		t.Fatalf("审计行列值不正确: action=%s target=%v", action, target)
	}
	if !detail.Valid || detail.String != "reason=test" {
		t.Fatalf("审计 detail 不正确: %v", detail)
	}
	if _, err := time.Parse(time.RFC3339, created); err != nil {
		t.Fatalf("审计 createdAt 应为 RFC3339: %q (%v)", created, err)
	}

	if err := app.db.QueryRow(
		`SELECT action, targetUserId FROM "AuditLog" WHERE action = ?`,
		auditActionAdminLogin).Scan(&auditAct, &auditTar); err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	if auditTar.Valid {
		t.Fatalf("无目标的审计行 targetUserId 必须为 NULL: %v", auditTar)
	}

	if count := coreAuditCount(t, app); count != 2 {
		t.Fatalf("审计行数不正确: %d", count)
	}
}

func TestAuditWriteRollsBackWithBusinessTransaction(t *testing.T) {
	app := coreTestApp(t, Config{})
	coreRegister(t, app, "rollback-audit@example.com", "secret123")

	tx := coreAuditTx(t, app)
	if err := auditWrite(context.Background(), tx, 1, auditActionAdminUserCreate, nil, ""); err != nil {
		_ = tx.Rollback()
		t.Fatalf("写入审计失败: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if count := coreAuditCount(t, app); count != 0 {
		t.Fatalf("业务事务回滚时审计也必须回滚: %d", count)
	}
}

func TestAuditRequiresTransaction(t *testing.T) {
	if err := auditWrite(context.Background(), nil, 1, auditActionAdminLogin, nil, ""); err == nil {
		t.Fatalf("缺少事务时必须报错")
	}
}

func TestSchemaRejectsUnknownAuditAction(t *testing.T) {
	app := coreTestApp(t, Config{})
	coreRegister(t, app, "schema-audit@example.com", "secret123")

	if _, err := app.db.Exec(
		`INSERT INTO "AuditLog" (actorId, action, targetUserId, detail, createdAt) VALUES (1, 'NOT_AN_ACTION', NULL, NULL, ?)`,
		coreNow()); err == nil {
		t.Fatalf("schema 的 CHECK 约束必须拒绝未知动作")
	}
}

// ---- admin guard ----

func TestRequireAdminSessionOnlyAcceptsAdminCookie(t *testing.T) {
	app := coreTestApp(t, Config{})
	_, adminCookie := coreRegister(t, app, "guard-admin@example.com", "secret123")
	coreInsertUser(t, app, "guard-user@example.com", "secret123", roleUser)
	userCookie := passwordLoginCookie(t, app, "guard-user@example.com", "secret123")
	rawKey := passwordCreateAPIKey(t, app, adminCookie, "ci")

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := app.requireAdminSession(w, r)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": user.Email})
	})

	admin := coreDo(t, handler, http.MethodGet, "/api/admin/stats", nil, coreCookieHeader(adminCookie))
	if admin.status != http.StatusOK || admin.body["email"] != "guard-admin@example.com" {
		t.Fatalf("管理员 Cookie 应当放行: %d %v", admin.status, admin.body)
	}

	plain := coreDo(t, handler, http.MethodGet, "/api/admin/stats", nil, coreCookieHeader(userCookie))
	if plain.status != http.StatusForbidden || coreErrorCode(t, plain) != "FORBIDDEN" {
		t.Fatalf("普通用户必须 403 FORBIDDEN: %d %v", plain.status, plain.body)
	}

	anonymous := coreDo(t, handler, http.MethodGet, "/api/admin/stats", nil, nil)
	if anonymous.status != http.StatusUnauthorized {
		t.Fatalf("未登录必须 401: %d %v", anonymous.status, anonymous.body)
	}

	bearer := coreDo(t, handler, http.MethodGet, "/api/admin/stats", nil,
		map[string]string{"Authorization": "Bearer " + rawKey})
	if bearer.status != http.StatusUnauthorized {
		t.Fatalf("管理员接口必须拒绝 API Key: %d %v", bearer.status, bearer.body)
	}
}

// ---- quota ----

func TestQuotaEffectiveBytesInheritsAndOverrides(t *testing.T) {
	app := coreTestApp(t, Config{UserQuotaBytes: 4096})
	coreRegister(t, app, "quota@example.com", "secret123")

	effective, err := app.quotaEffectiveBytes(context.Background(), 1)
	if err != nil || effective != 4096 {
		t.Fatalf("未设置覆盖值时应继承全局配额: %d %v", effective, err)
	}

	for _, override := range []int64{0, 1 << 20, 9999999} {
		if _, err := app.db.Exec(`UPDATE "User" SET quotaBytes = ? WHERE id = 1`, override); err != nil {
			t.Fatalf("设置配额失败: %v", err)
		}
		effective, err = app.quotaEffectiveBytes(context.Background(), 1)
		if err != nil || effective != override {
			t.Fatalf("覆盖值 %d 应生效，实际 %d %v", override, effective, err)
		}
	}

	if _, err := app.quotaEffectiveBytes(context.Background(), 999); !errors.Is(err, errNoUser) {
		t.Fatalf("不存在的账号应返回 errNoUser: %v", err)
	}
}

func TestQuotaUsedBytesCountsAllImages(t *testing.T) {
	app := coreTestApp(t, Config{})
	coreRegister(t, app, "usage@example.com", "secret123")

	used, err := app.quotaUsedBytes(context.Background(), 1)
	if err != nil || used != 0 {
		t.Fatalf("没有图片时用量应为 0: %d %v", used, err)
	}

	insert := func(id string, size int64, deletedAt sql.NullString, deletedByAdmin int) {
		now := coreNow()
		if _, err := app.db.Exec(
			`INSERT INTO "Image" (id, uploaderId, originalName, originalMimeType, size, sha256, width, height,
				originalPath, thumbPath, createdAt, updatedAt, deletedAt, deletedByAdmin, isVisible)
			 VALUES (?, 1, ?, 'image/png', ?, '', 10, 10, ?, ?, ?, ?, ?, ?, 1)`,
			id, id+".png", size, "original/"+id, "thumb/"+id, now, now, deletedAt, deletedByAdmin); err != nil {
			t.Fatalf("插入图片失败: %v", err)
		}
	}
	insert("live", 100, sql.NullString{}, 0)
	insert("trashed", 200, sql.NullString{String: coreNow(), Valid: true}, 0)
	insert("admin-hidden", 300, sql.NullString{}, 1)

	// 回收站与管理员下架、尚未物理删除的图片仍然占用配额。
	used, err = app.quotaUsedBytes(context.Background(), 1)
	if err != nil || used != 600 {
		t.Fatalf("用量应统计所有图片（含回收站/下架）: %d %v", used, err)
	}

	// 其他账号的图片不计入。
	coreInsertUser(t, app, "other-usage@example.com", "secret123", roleUser)
	if _, err := app.db.Exec(`UPDATE "Image" SET uploaderId = 2 WHERE id = 'trashed'`); err != nil {
		t.Fatalf("改上传者失败: %v", err)
	}
	used, err = app.quotaUsedBytes(context.Background(), 1)
	if err != nil || used != 400 {
		t.Fatalf("只统计该账号的图片: %d %v", used, err)
	}
}

// TestCoreTimestampMatchesImageTimeLayout 固定 core 与 images/export 共用的时间格式：
// 只要两者格式一致，SQL 里对 User.lastExportAt、ActivationCode.expiresAt 等 TEXT 列
// 的字符串比较就等价于时间先后；混用秒级 RFC3339 会让同一秒内的比较失真。
func TestCoreTimestampMatchesImageTimeLayout(t *testing.T) {
	now := time.Now()
	if formatted := coreTimestamp(now); formatted != now.UTC().Truncate(time.Millisecond).Format(imagesTimeLayout) {
		t.Fatalf("coreTimestamp 必须与 imagesTimeLayout 一致: %q", formatted)
	}
	if formatted := coreNow(); len(formatted) != len(coreTimestamp(now)) {
		t.Fatalf("coreNow 必须使用图片同一格式: %q", formatted)
	}

	// 跨秒、同秒内都可用字符串比较判定先后（毫秒精度）。
	base := time.Date(2026, 11, 17, 11, 5, 36, 0, time.UTC)
	samples := []time.Duration{
		0,
		1 * time.Millisecond,
		123 * time.Millisecond,
		999 * time.Millisecond,
		1 * time.Second,
		1500 * time.Millisecond,
	}
	for _, left := range samples {
		for _, right := range samples {
			leftString := coreTimestamp(base.Add(left))
			rightString := coreTimestamp(base.Add(right))
			if got, want := leftString < rightString, base.Add(left).Before(base.Add(right)); got != want {
				t.Fatalf("字符串比较与时间先后不一致: %s vs %s", leftString, rightString)
			}
		}
	}

	if _, err := time.Parse(time.RFC3339, coreNow()); err != nil {
		t.Fatalf("coreNow 必须能被 RFC3339 解析（供跨模块解析式比较）: %v", err)
	}
}
