package server

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

const (
	schemaOwner   = "picbox"
	schemaVersion = "2"
)

// ErrForeignDatabase 表示数据目录里已存在不是本服务创建的数据库。
// 服务不会对它执行任何改写型 PRAGMA 或 schema 语句。
var ErrForeignDatabase = errors.New("数据目录中已存在非本服务创建的数据库")

// ErrIncompatibleSchemaVersion 表示数据库是本服务创建的，但 schema 版本不是当前版本。
// 本服务不做就地升级：旧版数据在任何写入之前被拒绝，避免半迁移状态。
var ErrIncompatibleSchemaVersion = errors.New("数据库 schema 版本与当前服务不兼容")

func openDatabase(dataDir string) (*sql.DB, error) {
	if err := ensureDataDir(dataDir); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(dataDir, databaseFileName)

	fresh, err := databaseIsFresh(dbPath)
	if err != nil {
		return nil, err
	}
	if !fresh {
		// 在打开读写连接、执行 journal_mode 等会写文件的 PRAGMA 之前完成校验。
		if err := verifyDatabaseMarker(dbPath); err != nil {
			return nil, err
		}
	}

	if fresh {
		// 以 0600 创建，避免出现短暂的 0644 窗口。
		file, err := os.OpenFile(dbPath, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("创建数据库文件失败: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("创建数据库文件失败: %w", err)
		}
	}

	db, err := sql.Open("sqlite", readWriteDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// 单连接：所有事务串行化，配合首个账号注册的事务即可避免并发产生多个管理员。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("数据库不可用: %w", err)
	}

	if fresh {
		if err := applySchema(ctx, db); err != nil {
			_ = db.Close()
			return nil, err
		}
	}

	if err := tightenFileMode(dbPath); err != nil {
		_ = db.Close()
		return nil, err
	}

	// 上传暂存目录由 core 统一创建，images worker 直接写入 data/tmp。
	if err := os.MkdirAll(filepath.Join(dataDir, "tmp"), 0o700); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("创建暂存目录失败: %w", err)
	}

	return db, nil
}

func ensureDataDir(dir string) error {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建数据目录失败: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("检查数据目录失败: %w", err)
	case !info.IsDir():
		return fmt.Errorf("数据目录不是目录: %s", dir)
	}
	return nil
}

func databaseIsFresh(path string) (bool, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("检查数据库文件失败: %w", err)
	case !info.Mode().IsRegular():
		return false, fmt.Errorf("数据库路径不是普通文件: %s", path)
	}
	// 0 字节文件没有 schema，可以按全新数据库初始化；非空文件必须带本服务标记。
	return info.Size() == 0, nil
}

func verifyDatabaseMarker(path string) error {
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return fmt.Errorf("%w: 无法以只读方式打开 (%v)", ErrForeignDatabase, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var owner, version string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM picbox_meta WHERE key = 'schema_owner'`).Scan(&owner); err != nil {
		return fmt.Errorf("%w: 缺少 schema_owner 标记 (%v)", ErrForeignDatabase, err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM picbox_meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		return fmt.Errorf("%w: 缺少 schema_version 标记 (%v)", ErrForeignDatabase, err)
	}
	if owner != schemaOwner {
		return fmt.Errorf("%w: schema_owner=%q", ErrForeignDatabase, owner)
	}
	if version != schemaVersion {
		// version=1 是本服务早期预览库；它缺少激活码/审计/账号令牌/导出预约，
		// 无法原地补齐，必须明确拒绝而不是带着缺失表启动。
		return fmt.Errorf("%w: 数据库 schema_version=%q，当前服务只支持 %q；"+
			"不支持就地升级，请备份后使用全新的数据目录重新初始化",
			ErrIncompatibleSchemaVersion, version, schemaVersion)
	}
	return nil
}

func readOnlyDSN(path string) string {
	return sqliteDSN(path, "mode=ro", "_pragma=busy_timeout(5000)")
}

func readWriteDSN(path string) string {
	return sqliteDSN(path,
		"mode=rwc",
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(1)",
		// 单连接下 WAL 没有收益，DELETE 日志还能让只读校验可靠地读取外部/自建库。
		"_pragma=journal_mode(DELETE)",
		"_pragma=synchronous(FULL)",
		"_pragma=cache_size(-8192)",
	)
}

func sqliteDSN(path string, params ...string) string {
	// 只转义会破坏 DSN 查询串的字符；路径其余部分原样传给 SQLite。
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(filepath.ToSlash(path))
	return "file:" + escaped + "?" + strings.Join(params, "&")
}

func applySchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("初始化数据库 schema 失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, statement := range splitSQLStatements(schemaSQL) {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("初始化数据库 schema 失败: %w", err)
		}
	}

	for key, value := range map[string]string{
		"schema_owner":   schemaOwner,
		"schema_version": schemaVersion,
		"created_at":     time.Now().UTC().Format(time.RFC3339),
	} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO picbox_meta (key, value) VALUES (?, ?)`, key, value); err != nil {
			return fmt.Errorf("写入 schema 标记失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交数据库 schema 失败: %w", err)
	}
	return nil
}

// splitSQLStatements 只用于拆分本仓库内受控的 schema.sql（语句中不含分号字符串）。
func splitSQLStatements(script string) []string {
	var statements []string
	for _, part := range strings.Split(script, ";") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		statements = append(statements, trimmed)
	}
	return statements
}

func tightenFileMode(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("设置数据库文件权限失败: %w", err)
	}
	return nil
}
