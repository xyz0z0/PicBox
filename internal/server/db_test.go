package server

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestRejectsForeignDatabaseWithoutModifyingIt(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, databaseFileName)

	foreign, err := sql.Open("sqlite", "file:"+dbPath+"?mode=rwc")
	if err != nil {
		t.Fatalf("创建外部数据库失败: %v", err)
	}
	if _, err := foreign.Exec(`CREATE TABLE "User" (id INTEGER PRIMARY KEY, email TEXT NOT NULL)`); err != nil {
		_ = foreign.Close()
		t.Fatalf("写入外部 schema 失败: %v", err)
	}
	if _, err := foreign.Exec(`INSERT INTO "User" (email) VALUES ('legacy@example.com')`); err != nil {
		_ = foreign.Close()
		t.Fatalf("写入外部数据失败: %v", err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatalf("关闭外部数据库失败: %v", err)
	}

	beforeHash := coreFileHash(t, dbPath)
	beforeEntries := coreDirEntries(t, dir)
	sort.Strings(beforeEntries)

	_, err = New(Config{DataDir: dir})
	if err == nil {
		t.Fatalf("外部数据库必须被拒绝")
	}
	if !errors.Is(err, ErrForeignDatabase) {
		t.Fatalf("应当返回 ErrForeignDatabase，实际: %v", err)
	}

	afterHash := coreFileHash(t, dbPath)
	if afterHash != beforeHash {
		t.Fatalf("拒绝外部数据库时不得修改数据库文件")
	}
	afterEntries := coreDirEntries(t, dir)
	sort.Strings(afterEntries)
	if strings.Join(afterEntries, ",") != strings.Join(beforeEntries, ",") {
		t.Fatalf("拒绝外部数据库时不得在数据目录新增文件: before=%v after=%v", beforeEntries, afterEntries)
	}
}

func TestRejectsDatabaseWithoutMarker(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, databaseFileName)

	foreign, err := sql.Open("sqlite", "file:"+dbPath+"?mode=rwc")
	if err != nil {
		t.Fatalf("创建数据库失败: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE picbox_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO picbox_meta (key, value) VALUES ('schema_owner', 'someone-else')`,
		`INSERT INTO picbox_meta (key, value) VALUES ('schema_version', '1')`,
	} {
		if _, err := foreign.Exec(statement); err != nil {
			_ = foreign.Close()
			t.Fatalf("准备数据库失败: %v", err)
		}
	}
	if err := foreign.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}

	if _, err := New(Config{DataDir: dir}); !errors.Is(err, ErrForeignDatabase) {
		t.Fatalf("非本服务标记的数据库必须被拒绝，实际: %v", err)
	}
}

func TestFreshDatabaseIsInitializedWithRestrictedPermissions(t *testing.T) {
	dir := t.TempDir()
	app := coreTestApp(t, Config{DataDir: dir})

	dbPath := filepath.Join(dir, databaseFileName)
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat 数据库失败: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("数据库文件权限应为 0600，实际 %o", mode)
	}
	if stats := app.db.Stats(); stats.MaxOpenConnections != 1 {
		t.Fatalf("数据库必须是单连接，实际 MaxOpenConnections=%d", stats.MaxOpenConnections)
	}

	var foreignKeys int
	if err := app.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("读取 foreign_keys 失败: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys 必须开启")
	}
	var busyTimeout int
	if err := app.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("读取 busy_timeout 失败: %v", err)
	}
	if busyTimeout <= 0 {
		t.Fatalf("busy_timeout 必须大于 0，实际 %d", busyTimeout)
	}
	var cacheSize int
	if err := app.db.QueryRow(`PRAGMA cache_size`).Scan(&cacheSize); err != nil {
		t.Fatalf("读取 cache_size 失败: %v", err)
	}
	if cacheSize >= 0 {
		t.Fatalf("cache_size 应当使用有限上限（负值表示 KiB），实际 %d", cacheSize)
	}

	var owner string
	if err := app.db.QueryRow(`SELECT value FROM picbox_meta WHERE key = 'schema_owner'`).Scan(&owner); err != nil {
		t.Fatalf("读取 schema 标记失败: %v", err)
	}
	if owner != schemaOwner {
		t.Fatalf("schema_owner 标记不正确: %q", owner)
	}

	for _, table := range []string{"User", "ApiKey", "Image", "ImageTag", "AuditLog", "ActivationCode", "AccountToken"} {
		var name string
		if err := app.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("缺少表 %s: %v", table, err)
		}
	}

	// 导出预约列必须在 User 上存在且允许 NULL（导出 worker 共用的共享契约）。
	userColumns := map[string]int{}
	rows, err := app.db.Query(`PRAGMA table_info("User")`)
	if err != nil {
		t.Fatalf("读取 User 列失败: %v", err)
	}
	for rows.Next() {
		var (
			cid       int
			name      string
			typ       string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &pk); err != nil {
			_ = rows.Close()
			t.Fatalf("读取 User 列失败: %v", err)
		}
		userColumns[name] = notNull
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("关闭 User 列查询失败: %v", err)
	}
	for _, column := range []string{"lastExportAt", "exportReservationAt", "exportReservationToken"} {
		notNull, ok := userColumns[column]
		if !ok {
			t.Fatalf("User 缺少列 %s", column)
		}
		if notNull != 0 {
			t.Fatalf("%s 必须允许 NULL", column)
		}
	}

	tmpInfo, err := os.Stat(filepath.Join(dir, "tmp"))
	if err != nil {
		t.Fatalf("缺少上传暂存目录: %v", err)
	}
	if mode := tmpInfo.Mode().Perm(); mode != 0o700 {
		t.Fatalf("暂存目录权限应为 0700，实际 %o", mode)
	}
}

func TestEmptyDatabaseFileIsInitialized(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, databaseFileName)
	if err := os.WriteFile(dbPath, nil, 0o600); err != nil {
		t.Fatalf("创建空数据库文件失败: %v", err)
	}
	app := coreTestApp(t, Config{DataDir: dir})
	var owner string
	if err := app.db.QueryRow(`SELECT value FROM picbox_meta WHERE key = 'schema_owner'`).Scan(&owner); err != nil {
		t.Fatalf("空文件应被初始化为本服务数据库: %v", err)
	}
	if owner != schemaOwner {
		t.Fatalf("schema_owner 不正确: %q", owner)
	}
}

func TestHealthReflectsDatabaseState(t *testing.T) {
	app := coreTestApp(t, Config{})

	healthy := coreDo(t, app.Handler(), http.MethodGet, "/api/health", nil, nil)
	if healthy.status != http.StatusOK || healthy.body["ok"] != true {
		t.Fatalf("健康检查应当 200: %d %v", healthy.status, healthy.body)
	}

	if err := app.db.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}
	unhealthy := coreDo(t, app.Handler(), http.MethodGet, "/api/health", nil, nil)
	if unhealthy.status != http.StatusServiceUnavailable {
		t.Fatalf("数据库不可用时健康检查应当 503: %d %v", unhealthy.status, unhealthy.body)
	}
	if coreErrorCode(t, unhealthy) != "DB_UNAVAILABLE" {
		t.Fatalf("健康检查错误码不正确: %v", unhealthy.body)
	}
}

func TestUnknownAPIReturnsJSONNotFound(t *testing.T) {
	app := coreTestApp(t, Config{})
	for _, target := range []string{"/api/not-implemented", "/api", "/api/images-not-real"} {
		response := coreDo(t, app.Handler(), http.MethodGet, target, nil, nil)
		if response.status != http.StatusNotFound {
			t.Fatalf("%s 应当 404: %d", target, response.status)
		}
		contentType := response.recorder.Header().Get("Content-Type")
		if !strings.HasPrefix(contentType, "application/json") {
			t.Fatalf("%s 未知 API 必须返回 JSON，实际 %q", target, contentType)
		}
		if coreErrorCode(t, response) != "NOT_FOUND" {
			t.Fatalf("%s 错误码不正确: %v", target, response.body)
		}
	}
}

func TestRejectsVersion1DatabaseWithoutModifyingIt(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, databaseFileName)

	legacy, err := sql.Open("sqlite", "file:"+dbPath+"?mode=rwc")
	if err != nil {
		t.Fatalf("创建旧版数据库失败: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE picbox_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO picbox_meta (key, value) VALUES ('schema_owner', 'picbox')`,
		`INSERT INTO picbox_meta (key, value) VALUES ('schema_version', '1')`,
		`CREATE TABLE "User" (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT NOT NULL)`,
		`INSERT INTO "User" (email) VALUES ('preview@example.com')`,
	} {
		if _, err := legacy.Exec(statement); err != nil {
			_ = legacy.Close()
			t.Fatalf("准备旧版数据库失败: %v", err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("关闭旧版数据库失败: %v", err)
	}

	beforeHash := coreFileHash(t, dbPath)
	beforeEntries := coreDirEntries(t, dir)
	sort.Strings(beforeEntries)

	_, err = New(Config{DataDir: dir})
	if err == nil {
		t.Fatalf("schema_version=1 的数据库必须被拒绝")
	}
	if !errors.Is(err, ErrIncompatibleSchemaVersion) {
		t.Fatalf("应当返回 ErrIncompatibleSchemaVersion，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "1") || !strings.Contains(err.Error(), schemaVersion) {
		t.Fatalf("错误信息应说明旧版本与当前版本: %v", err)
	}

	afterHash := coreFileHash(t, dbPath)
	if afterHash != beforeHash {
		t.Fatalf("拒绝旧版数据库时不得修改数据库文件")
	}
	afterEntries := coreDirEntries(t, dir)
	sort.Strings(afterEntries)
	if strings.Join(afterEntries, ",") != strings.Join(beforeEntries, ",") {
		t.Fatalf("拒绝旧版数据库时不得在数据目录新增文件: before=%v after=%v", beforeEntries, afterEntries)
	}
}

func TestRejectsFutureSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, databaseFileName)

	future, err := sql.Open("sqlite", "file:"+dbPath+"?mode=rwc")
	if err != nil {
		t.Fatalf("创建数据库失败: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE picbox_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT INTO picbox_meta (key, value) VALUES ('schema_owner', 'picbox')`,
		`INSERT INTO picbox_meta (key, value) VALUES ('schema_version', '99')`,
	} {
		if _, err := future.Exec(statement); err != nil {
			_ = future.Close()
			t.Fatalf("准备数据库失败: %v", err)
		}
	}
	if err := future.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}

	if _, err := New(Config{DataDir: dir}); !errors.Is(err, ErrIncompatibleSchemaVersion) {
		t.Fatalf("未知 schema 版本必须被拒绝，实际: %v", err)
	}
}
