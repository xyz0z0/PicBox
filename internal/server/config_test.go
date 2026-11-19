package server

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearCoreConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"PICBOX_ADDR", "PICBOX_DATA_DIR", "AUTH_COOKIE_NAME", "BASEURL", "UPLOAD_MAX_BYTES",
		"MAX_IMAGE_PIXELS", "USER_QUOTA_BYTES", "COOKIE_SECURE", "JWT_SECRET",
		"EXPORT_COOLDOWN_HOURS", "TRASH_RETENTION_DAYS", "TRUSTED_PROXIES",
		"SMTP_HOST", "SMTP_PORT", "SMTP_SECURE", "SMTP_REQUIRE_TLS", "SMTP_USER", "SMTP_PASSWORD", "SMTP_FROM",
	} {
		t.Setenv(name, "")
	}
}

func TestNormalizeConfigDefaults(t *testing.T) {
	normalized, err := normalizeConfig(Config{DataDir: "./data"})
	if err != nil {
		t.Fatalf("默认配置应当合法: %v", err)
	}
	if normalized.Addr != "127.0.0.1:3001" {
		t.Fatalf("默认监听地址必须是环回: %q", normalized.Addr)
	}
	if normalized.AuthCookieName != "picbox_token" {
		t.Fatalf("默认 Cookie 名应为 picbox_token: %q", normalized.AuthCookieName)
	}
	if normalized.UploadMaxBytes != 5<<20 {
		t.Fatalf("默认上传上限应为 5MiB: %d", normalized.UploadMaxBytes)
	}
	if normalized.MaxImagePixels != 16_000_000 {
		t.Fatalf("默认像素上限应为 16000000: %d", normalized.MaxImagePixels)
	}
	if normalized.UserQuotaBytes != 0 {
		t.Fatalf("默认配额应为 0（不限）: %d", normalized.UserQuotaBytes)
	}
	if normalized.CookieSecure {
		t.Fatalf("默认 BASEURL 为空时 CookieSecure 应为 false")
	}
	if normalized.ExportCooldownHours != 12 {
		t.Fatalf("导出冷却默认应为 12 小时: %d", normalized.ExportCooldownHours)
	}
	if normalized.TrashRetentionDays != 30 {
		t.Fatalf("回收站保留默认应为 30 天: %d", normalized.TrashRetentionDays)
	}
	if normalized.SMTP.Configured() {
		t.Fatalf("默认不应配置 SMTP: %+v", normalized.SMTP)
	}
	if !normalized.SMTP.RequireTLS {
		t.Fatalf("SMTP 默认必须要求 TLS，不能静默明文降级")
	}
}

func TestExportCooldownExplicitZeroIsNotDefault(t *testing.T) {
	// Config 字面量的 0 表示“未设置”，走默认 12 小时。
	normalized, err := normalizeConfig(Config{})
	if err != nil {
		t.Fatalf("默认配置应当合法: %v", err)
	}
	if normalized.ExportCooldownHours != 12 {
		t.Fatalf("未显式设置时应默认 12 小时: %d", normalized.ExportCooldownHours)
	}

	disabled, err := normalizeConfig(Config{}.WithExportCooldown(0))
	if err != nil {
		t.Fatalf("显式 0 应当合法: %v", err)
	}
	if disabled.ExportCooldownHours != 0 {
		t.Fatalf("显式 0 必须保留为关闭冷却: %d", disabled.ExportCooldownHours)
	}

	if _, err := normalizeConfig(Config{}.WithExportCooldown(-1)); err == nil {
		t.Fatalf("负数冷却必须报错")
	}
}

func TestTrashRetentionDaysMinimum(t *testing.T) {
	normalized, err := normalizeConfig(Config{TrashRetentionDays: 1})
	if err != nil || normalized.TrashRetentionDays != 1 {
		t.Fatalf("最小保留期 1 天应当合法: %d %v", normalized.TrashRetentionDays, err)
	}
	if _, err := normalizeConfig(Config{TrashRetentionDays: -1}); err == nil {
		t.Fatalf("负保留期必须报错")
	}
}

func TestTrustedProxiesConfigNormalization(t *testing.T) {
	// 默认（未配置）：不信任任何转发头。
	none, err := normalizeConfig(Config{})
	if err != nil {
		t.Fatalf("默认配置应当合法: %v", err)
	}
	if len(none.TrustedProxies) != 0 {
		t.Fatalf("默认不应信任任何转发代理: %v", none.TrustedProxies)
	}

	parsed, err := parseTrustedProxies(" 127.0.0.1 , ::1/128 , 10.1.2.3/8 , ::ffff:127.0.0.1/128 , 127.0.0.1/32 , 2001:db8::5 ")
	if err != nil {
		t.Fatalf("合法 TRUSTED_PROXIES 不应报错: %v", err)
	}
	want := []string{"127.0.0.1/32", "::1/128", "10.0.0.0/8", "2001:db8::5/128"}
	if len(parsed) != len(want) {
		t.Fatalf("归一化后应为 %v（去重、masked、v4-mapped 还原），实际 %v", want, parsed)
	}
	for i, expected := range want {
		if parsed[i].String() != expected {
			t.Fatalf("第 %d 项应为 %s，实际 %s", i, expected, parsed[i])
		}
	}

	// Config 字面量同样归一化：未 masked 的前缀按网段补齐。
	normalized, err := normalizeConfig(Config{
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.168.1.200/24")},
	})
	if err != nil {
		t.Fatalf("Config 字面量应当合法: %v", err)
	}
	if len(normalized.TrustedProxies) != 1 || normalized.TrustedProxies[0].String() != "192.168.1.0/24" {
		t.Fatalf("Config 字面量应与环境变量得到同样的规范化结果: %v", normalized.TrustedProxies)
	}

	cases := map[string]string{
		"主机名":           "localhost",
		"带端口":           "127.0.0.1:3000",
		"带 zone":        "::1%eth0",
		"空项":            "127.0.0.1/32,,::1/128",
		"非法 CIDR":       "127.0.0.1/33",
		"信任全部 IPv4":     "0.0.0.0/0",
		"信任全部 IPv6":     "::/0",
		"过宽的 v4-mapped": "::ffff:10.0.0.0/64",
		"只信任全部映射":       "::ffff:0.0.0.0/96",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseTrustedProxies(raw); err == nil {
				t.Fatalf("非法 TRUSTED_PROXIES 应当报错: %q", raw)
			}
		})
	}
}

func TestLoadConfigReadsTrustedProxies(t *testing.T) {
	clearCoreConfigEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Fatalf("未设置 TRUSTED_PROXIES 时应为空: %v", cfg.TrustedProxies)
	}

	t.Setenv("TRUSTED_PROXIES", "127.0.0.1/32,::1/128")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if len(cfg.TrustedProxies) != 2 || cfg.TrustedProxies[0].String() != "127.0.0.1/32" || cfg.TrustedProxies[1].String() != "::1/128" {
		t.Fatalf("TRUSTED_PROXIES 未按环境变量读取: %v", cfg.TrustedProxies)
	}
}

func TestSMTPConfigValidation(t *testing.T) {
	valid, err := normalizeConfig(Config{
		SMTP: SMTPConfig{Host: "smtp.example.com", From: "noreply@example.com"},
	})
	if err != nil {
		t.Fatalf("合法 SMTP 配置不应报错: %v", err)
	}
	if valid.SMTP.Port != 587 || !valid.SMTP.RequireTLS {
		t.Fatalf("默认端口应为 587 且要求 TLS: %+v", valid.SMTP)
	}

	secure, err := normalizeConfig(Config{SMTP: SMTPConfig{Host: "smtp.example.com", From: "n@example.com", Secure: true}})
	if err != nil {
		t.Fatalf("secure SMTP 配置不应报错: %v", err)
	}
	if secure.SMTP.Port != 465 {
		t.Fatalf("secure 模式默认端口应为 465: %d", secure.SMTP.Port)
	}

	illegal := map[string]SMTPConfig{
		"缺少 from": {Host: "smtp.example.com"},
		"缺少 host": {From: "n@example.com"},
		"只配 user": {Host: "smtp.example.com", From: "n@example.com", Username: "u"},
		"端口越界":    {Host: "smtp.example.com", From: "n@example.com", Port: 70000},
	}
	for name, cfg := range illegal {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := New(Config{DataDir: dir, SMTP: cfg}); err == nil {
				t.Fatalf("非法 SMTP 配置应当启动失败: %+v", cfg)
			}
			if entries := coreDirEntries(t, dir); len(entries) != 0 {
				t.Fatalf("配置校验失败时不应创建文件: %v", entries)
			}
		})
	}
}

func TestConfigRejectsIllegalValues(t *testing.T) {
	cases := map[string]Config{
		"负上传上限":         {UploadMaxBytes: -1},
		"负像素上限":         {MaxImagePixels: -1},
		"负配额":           {UserQuotaBytes: -1},
		"缺少端口":          {Addr: "localhost"},
		"缺少主机":          {Addr: ":3001"},
		"端口越界":          {Addr: "127.0.0.1:70000"},
		"非 http scheme": {BaseURL: "ftp://pic.example.com"},
		"带用户信息":         {BaseURL: "https://user:pass@pic.example.com"},
		"带查询串":          {BaseURL: "https://pic.example.com/?a=1"},
		"Cookie名带空格":    {AuthCookieName: "picbox token"},
		"Cookie名带分号":    {AuthCookieName: "picbox;token"},
		"Cookie名带等号":    {AuthCookieName: "picbox=token"},
		"Cookie名带换行":    {AuthCookieName: "picbox\ntoken"},
		"Cookie名过长":     {AuthCookieName: "p" + strings.Repeat("x", 100)},
		"信任全部地址":        {TrustedProxies: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfg.DataDir = dir
			if _, err := New(cfg); err == nil {
				t.Fatalf("非法配置应当启动失败: %+v", cfg)
			}
			entries := coreDirEntries(t, dir)
			if len(entries) != 0 {
				t.Fatalf("配置校验失败时不应创建文件: %v", entries)
			}
		})
	}
}

func TestLoadConfigReadsEnvironment(t *testing.T) {
	clearCoreConfigEnv(t)
	t.Setenv("PICBOX_ADDR", "127.0.0.1:4321")
	t.Setenv("PICBOX_DATA_DIR", "./preview-data")
	t.Setenv("AUTH_COOKIE_NAME", "picbox_preview_token")
	t.Setenv("BASEURL", "https://pic.example.com")
	t.Setenv("UPLOAD_MAX_BYTES", "1048576")
	t.Setenv("MAX_IMAGE_PIXELS", "4000000")
	t.Setenv("USER_QUOTA_BYTES", "2048")
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if cfg.Addr != "127.0.0.1:4321" || cfg.DataDir != "./preview-data" {
		t.Fatalf("地址/目录未按环境变量读取: %+v", cfg)
	}
	if cfg.AuthCookieName != "picbox_preview_token" {
		t.Fatalf("AUTH_COOKIE_NAME 未按环境变量读取: %q", cfg.AuthCookieName)
	}
	if cfg.BaseURL != "https://pic.example.com" {
		t.Fatalf("BASEURL 未按环境变量读取: %q", cfg.BaseURL)
	}
	if cfg.UploadMaxBytes != 1048576 || cfg.MaxImagePixels != 4_000_000 || cfg.UserQuotaBytes != 2048 {
		t.Fatalf("数值配置未按环境变量读取: %+v", cfg)
	}
	if cfg.ExportCooldownHours != 12 || cfg.TrashRetentionDays != 30 {
		t.Fatalf("未设置时冷却/保留期应使用默认值: %+v", cfg)
	}
	if !cfg.CookieSecure {
		t.Fatalf("https BASEURL 应默认启用 Secure Cookie")
	}
}

func TestLoadConfigReadsExportTrashAndSMTP(t *testing.T) {
	clearCoreConfigEnv(t)
	t.Setenv("EXPORT_COOLDOWN_HOURS", "0")
	t.Setenv("TRASH_RETENTION_DAYS", "7")
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_PORT", "2525")
	t.Setenv("SMTP_SECURE", "true")
	t.Setenv("SMTP_REQUIRE_TLS", "false")
	t.Setenv("SMTP_USER", "mailer")
	t.Setenv("SMTP_PASSWORD", "secret")
	t.Setenv("SMTP_FROM", "PicBox <noreply@example.com>")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if cfg.ExportCooldownHours != 0 {
		t.Fatalf("EXPORT_COOLDOWN_HOURS=0 必须显式关闭冷却: %d", cfg.ExportCooldownHours)
	}
	if cfg.TrashRetentionDays != 7 {
		t.Fatalf("TRASH_RETENTION_DAYS 未生效: %d", cfg.TrashRetentionDays)
	}
	if cfg.SMTP.Host != "smtp.example.com" || cfg.SMTP.Port != 2525 ||
		!cfg.SMTP.Secure || cfg.SMTP.RequireTLS ||
		cfg.SMTP.Username != "mailer" || cfg.SMTP.Password != "secret" ||
		cfg.SMTP.From != "PicBox <noreply@example.com>" {
		t.Fatalf("SMTP 配置未按环境变量读取: %+v", cfg.SMTP)
	}
	if !cfg.SMTP.Configured() {
		t.Fatalf("完整 SMTP 配置应当可用")
	}
}

func TestSMTPDefaultPortFromEnvironment(t *testing.T) {
	clearCoreConfigEnv(t)
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_FROM", "noreply@example.com")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if cfg.SMTP.Port != 587 || !cfg.SMTP.RequireTLS || cfg.SMTP.Secure {
		t.Fatalf("默认应为 587 + STARTTLS: %+v", cfg.SMTP)
	}

	t.Setenv("SMTP_SECURE", "true")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if cfg.SMTP.Port != 465 {
		t.Fatalf("secure 模式默认端口应为 465: %+v", cfg.SMTP)
	}
}

func TestCookieSecureExplicitOverride(t *testing.T) {
	clearCoreConfigEnv(t)
	t.Setenv("BASEURL", "https://pic.example.com")
	t.Setenv("COOKIE_SECURE", "false")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if cfg.CookieSecure {
		t.Fatalf("显式 COOKIE_SECURE=false 应覆盖 BASEURL 推导")
	}

	clearCoreConfigEnv(t)
	t.Setenv("BASEURL", "http://pic.example.com")
	t.Setenv("COOKIE_SECURE", "true")
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if !cfg.CookieSecure {
		t.Fatalf("显式 COOKIE_SECURE=true 应生效")
	}
}

func TestIllegalEnvironmentValuesFailStartup(t *testing.T) {
	cases := map[string]map[string]string{
		"非数字上传上限":        {"UPLOAD_MAX_BYTES": "many"},
		"负上传上限":          {"UPLOAD_MAX_BYTES": "-1"},
		"负配额":            {"USER_QUOTA_BYTES": "-5"},
		"非法布尔":           {"COOKIE_SECURE": "sometimes"},
		"短 JWT":          {"JWT_SECRET": "too-short"},
		"非法地址":           {"PICBOX_ADDR": "localhost"},
		"非法 Cookie 名":    {"AUTH_COOKIE_NAME": "bad name"},
		"负冷却小时":          {"EXPORT_COOLDOWN_HOURS": "-1"},
		"冷却非整数":          {"EXPORT_COOLDOWN_HOURS": "twelve"},
		"保留期为 0":         {"TRASH_RETENTION_DAYS": "0"},
		"保留期为负":          {"TRASH_RETENTION_DAYS": "-3"},
		"SMTP 缺 from":    {"SMTP_HOST": "smtp.example.com"},
		"SMTP 端口非法":      {"SMTP_HOST": "smtp.example.com", "SMTP_FROM": "n@example.com", "SMTP_PORT": "0"},
		"SMTP 半套凭据":      {"SMTP_HOST": "smtp.example.com", "SMTP_FROM": "n@example.com", "SMTP_USER": "u"},
		"SMTP secure 非法": {"SMTP_HOST": "smtp.example.com", "SMTP_FROM": "n@example.com", "SMTP_SECURE": "sometimes"},
		"非法可信代理":         {"TRUSTED_PROXIES": "localhost"},
		"信任全部代理":         {"TRUSTED_PROXIES": "0.0.0.0/0"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			clearCoreConfigEnv(t)
			for key, value := range env {
				t.Setenv(key, value)
			}
			if _, err := LoadConfig(); err == nil {
				t.Fatalf("非法环境变量应当启动失败: %v", env)
			}
		})
	}
}

func TestJWTSecretEnvOverridesGeneratedKey(t *testing.T) {
	clearCoreConfigEnv(t)
	secret := "0123456789abcdef0123456789abcdef"
	t.Setenv("JWT_SECRET", secret)
	dir := t.TempDir()
	app := coreTestApp(t, Config{DataDir: dir})
	if string(app.secret) != secret {
		t.Fatalf("JWT_SECRET 应当覆盖自动生成的密钥")
	}
	if _, err := os.Stat(filepath.Join(dir, authKeyFileName)); !os.IsNotExist(err) {
		t.Fatalf("使用 JWT_SECRET 时不应再创建 auth.key: %v", err)
	}
}

func TestShortJWTSecretFailsBeforeCreatingFiles(t *testing.T) {
	clearCoreConfigEnv(t)
	t.Setenv("JWT_SECRET", "short-secret")
	dir := t.TempDir()
	if _, err := New(Config{DataDir: dir}); err == nil {
		t.Fatalf("短 JWT_SECRET 应当启动失败")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("短 JWT_SECRET 失败时不应创建数据库或密钥: %v", entries)
	}
}
