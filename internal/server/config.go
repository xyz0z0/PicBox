package server

import (
	"fmt"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const (
	defaultAddr                    = "127.0.0.1:3001"
	defaultDataDir                 = "./data"
	defaultUploadMaxBytes          = int64(5 << 20) // 5 MiB
	defaultMaxImagePixels          = int64(16_000_000)
	minJWTSecretBytes              = 32
	authKeyFileName                = "auth.key"
	databaseFileName               = "picbox.sqlite"
	defaultRateLimitCapacity       = 4096
	defaultJSONBodyLimit     int64 = 1 << 20

	// 导出成功冷却与回收站保留期的默认值（与原版 EXPORT_COOLDOWN_HOURS /
	// TRASH_RETENTION_DAYS 一致）。
	defaultExportCooldownHours = 12
	defaultTrashRetentionDays  = 30
	minTrashRetentionDays      = 1

	// Cookie 名默认是产品名，可用 AUTH_COOKIE_NAME 覆盖（部署在同一主机的多个
	// localhost 服务共享 Cookie 域，自定义名称可避免互相覆盖）。
	defaultAuthCookieName   = "picbox_token"
	authCookieNameMaxLength = 100
)

// authCookieName 是默认 Cookie 名，供同包测试引用；运行时读写一律使用
// Config.AuthCookieName，以便 AUTH_COOKIE_NAME 覆盖。
const authCookieName = defaultAuthCookieName

// authCookieNamePattern 是 RFC 6265 token 允许的字符集（不含分隔符、空白与控制字符）。
var authCookieNamePattern = regexp.MustCompile("^[!#$%&'*+\\-.^_\\x60|~0-9A-Za-z]+$")

// SMTPConfig 是找回密码 / 修改邮箱所需的外发邮件配置。
// Host 为空表示未配置邮件能力：相关接口返回 503 MAIL_UNAVAILABLE，
// 普通登录部署不需要配置任何 SMTP 字段。
//
// Secure 表示从连接建立即使用 TLS（原版 SMTP_SECURE，通常是 465 端口）；
// RequireTLS 表示必须通过 STARTTLS 升级后才发送（原版 SMTP_REQUIRE_TLS，默认 true）。
// 两者都关闭等价于显式接受明文投递——这只能由部署配置显式选择，服务不会静默降级，
// 并且证书校验始终启用（不存在 InsecureSkipVerify 开关）。
type SMTPConfig struct {
	Host       string
	Port       int
	Secure     bool
	RequireTLS bool
	Username   string
	Password   string
	From       string

	// requireTLSSet 表示 RequireTLS 已由环境变量显式指定，用来区分
	// “未设置（默认要求 TLS）”与“显式关闭（部署明确接受明文）”。
	requireTLSSet bool
}

// AllowExplicitPlaintext 表示部署显式接受明文 SMTP 投递（RequireTLS=false）。
// 只有显式调用它才会关闭 STARTTLS 要求；零值配置与 SMTP_REQUIRE_TLS 未设置时始终要求 TLS。
func (s SMTPConfig) AllowExplicitPlaintext() SMTPConfig {
	s.RequireTLS = false
	s.requireTLSSet = true
	return s
}

// Configured 表示邮件能力是否可用。
func (s SMTPConfig) Configured() bool {
	return strings.TrimSpace(s.Host) != "" && strings.TrimSpace(s.From) != ""
}

// Config 是服务启动配置。零值字段使用安全默认值；负数或格式非法的字段
// 会在 New 阶段直接报错，不做静默修正。
type Config struct {
	Addr           string
	DataDir        string
	AuthCookieName string
	BaseURL        string
	UploadMaxBytes int64
	MaxImagePixels int64
	UserQuotaBytes int64
	CookieSecure   bool

	// ExportCooldownHours 是导出成功后的冷却小时数，0 表示关闭冷却。
	// 零值在未显式指定时按默认 12 小时处理；要表达“显式关闭”请在包内用
	// Config.WithExportCooldown(0)（或 LoadConfig 的 EXPORT_COOLDOWN_HOURS=0）。
	ExportCooldownHours int
	// TrashRetentionDays 是回收站保留天数，默认 30，最小 1。
	TrashRetentionDays int
	// TrustedProxies 是允许提供 X-Forwarded-For 的直连代理地址范围（TRUSTED_PROXIES，
	// 逗号分隔的 IP/CIDR，已归一化为 masked 前缀）。为空（默认）表示不信任任何转发头，
	// 限流键一律取 TCP 对端地址；只有列在这里的对端才会解析 XFF。
	TrustedProxies []netip.Prefix
	// SMTP 是找回密码 / 修改邮箱的邮件配置；零值表示未配置。
	SMTP SMTPConfig

	// cookieSecureSet 表示 CookieSecure 已被显式指定（来自 COOKIE_SECURE），
	// 此时 New 不再按 BaseURL 的 scheme 推导。
	cookieSecureSet bool
	// exportCooldownSet 表示 ExportCooldownHours 已被显式指定，用来区分
	// “未设置（默认 12）”与“显式 0（关闭冷却）”。
	exportCooldownSet bool
}

// WithExportCooldown 显式指定导出冷却小时数（0 = 关闭冷却）。
// 同包内构造测试配置时用它表达“显式 0”，避免与未设置混淆。
func (c Config) WithExportCooldown(hours int) Config {
	c.ExportCooldownHours = hours
	c.exportCooldownSet = true
	return c
}

// LoadConfig 读取环境变量并返回归一化后的配置。
// JWT_SECRET 不进入 Config，由 New 读取；这里只做长度校验以便尽早失败。
func LoadConfig() (Config, error) {
	cfg := Config{
		Addr:           strings.TrimSpace(os.Getenv("PICBOX_ADDR")),
		DataDir:        strings.TrimSpace(os.Getenv("PICBOX_DATA_DIR")),
		AuthCookieName: strings.TrimSpace(os.Getenv("AUTH_COOKIE_NAME")),
		BaseURL:        strings.TrimSpace(os.Getenv("BASEURL")),
		CookieSecure:   false,
	}

	var err error
	if cfg.UploadMaxBytes, err = envInt64("UPLOAD_MAX_BYTES", defaultUploadMaxBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxImagePixels, err = envInt64("MAX_IMAGE_PIXELS", defaultMaxImagePixels); err != nil {
		return Config{}, err
	}
	if cfg.UserQuotaBytes, err = envInt64("USER_QUOTA_BYTES", 0); err != nil {
		return Config{}, err
	}

	// EXPORT_COOLDOWN_HOURS 与其它数值项不同：0 是有效值（关闭冷却），
	// 因此必须显式记录“环境变量已设置”，不能靠零值判断。
	if raw := strings.TrimSpace(os.Getenv("EXPORT_COOLDOWN_HOURS")); raw != "" {
		hours, parseErr := strconv.Atoi(raw)
		if parseErr != nil {
			return Config{}, fmt.Errorf("EXPORT_COOLDOWN_HOURS 非法: %q（需要整数，0 表示关闭冷却）", raw)
		}
		cfg.ExportCooldownHours = hours
		cfg.exportCooldownSet = true
	}
	if cfg.TrashRetentionDays, err = envInt("TRASH_RETENTION_DAYS", 0); err != nil {
		return Config{}, err
	}
	if raw := strings.TrimSpace(os.Getenv("TRASH_RETENTION_DAYS")); raw != "" && cfg.TrashRetentionDays < minTrashRetentionDays {
		return Config{}, fmt.Errorf("TRASH_RETENTION_DAYS 不能小于 %d: %q", minTrashRetentionDays, raw)
	}
	if cfg.SMTP, err = loadSMTPConfig(); err != nil {
		return Config{}, err
	}

	// 不设置时为空：程序直接启动（无反向代理）时不信任任何转发头。
	if cfg.TrustedProxies, err = parseTrustedProxies(os.Getenv("TRUSTED_PROXIES")); err != nil {
		return Config{}, err
	}

	if raw := strings.TrimSpace(os.Getenv("COOKIE_SECURE")); raw != "" {
		secure, err := parseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("COOKIE_SECURE 非法: %q（只接受 true/false/1/0）", raw)
		}
		cfg.CookieSecure = secure
		cfg.cookieSecureSet = true
	}

	if err := validateJWTSecretEnv(); err != nil {
		return Config{}, err
	}

	return normalizeConfig(cfg)
}

func envInt64(name string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s 非法: %q（需要整数）", name, raw)
	}
	return value, nil
}

func envInt(name string, fallback int) (int, error) {
	value, err := envInt64(name, int64(fallback))
	if err != nil {
		return 0, err
	}
	if value > math.MaxInt32 || value < math.MinInt32 {
		return 0, fmt.Errorf("%s 超出范围: %d", name, value)
	}
	return int(value), nil
}

// loadSMTPConfig 读取 SMTP_* 环境变量。Host/From 都为空表示未配置邮件；
// 只配置一部分则启动失败，避免“以为能发信、实际 503”的静默部署事故。
func loadSMTPConfig() (SMTPConfig, error) {
	cfg := SMTPConfig{
		Host:       strings.TrimSpace(os.Getenv("SMTP_HOST")),
		Username:   os.Getenv("SMTP_USER"),
		Password:   os.Getenv("SMTP_PASSWORD"),
		From:       strings.TrimSpace(os.Getenv("SMTP_FROM")),
		RequireTLS: true,
	}

	if raw := strings.TrimSpace(os.Getenv("SMTP_SECURE")); raw != "" {
		secure, err := parseBool(raw)
		if err != nil {
			return SMTPConfig{}, fmt.Errorf("SMTP_SECURE 非法: %q（只接受 true/false）", raw)
		}
		cfg.Secure = secure
	}
	if raw := strings.TrimSpace(os.Getenv("SMTP_REQUIRE_TLS")); raw != "" {
		requireTLS, err := parseBool(raw)
		if err != nil {
			return SMTPConfig{}, fmt.Errorf("SMTP_REQUIRE_TLS 非法: %q（只接受 true/false）", raw)
		}
		cfg.RequireTLS = requireTLS
		cfg.requireTLSSet = true
	}

	port := 0
	if raw := strings.TrimSpace(os.Getenv("SMTP_PORT")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return SMTPConfig{}, fmt.Errorf("SMTP_PORT 非法: %q（需要整数）", raw)
		}
		if parsed < 1 || parsed > 65535 {
			return SMTPConfig{}, fmt.Errorf("SMTP_PORT 必须在 1~65535: %q", raw)
		}
		port = parsed
	}
	if port == 0 {
		if cfg.Secure {
			port = 465
		} else {
			port = 587
		}
	}
	cfg.Port = port

	return cfg, nil
}

func parseBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid bool %q", raw)
	}
}

func validateJWTSecretEnv() error {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return nil
	}
	if len(secret) < minJWTSecretBytes {
		return fmt.Errorf("JWT_SECRET 至少需要 %d 字节，当前 %d 字节", minJWTSecretBytes, len(secret))
	}
	return nil
}

// normalizeConfig 填充默认值并校验配置；New 与 LoadConfig 共用这一套规则，
// 保证 Config 字面量（例如测试里的 Config{DataDir: t.TempDir()}）行为一致。
func normalizeConfig(cfg Config) (Config, error) {
	if cfg.Addr == "" {
		cfg.Addr = defaultAddr
	}
	if err := validateAddr(cfg.Addr); err != nil {
		return Config{}, err
	}

	if cfg.DataDir == "" {
		cfg.DataDir = defaultDataDir
	}

	if cfg.AuthCookieName == "" {
		cfg.AuthCookieName = defaultAuthCookieName
	}
	if len(cfg.AuthCookieName) > authCookieNameMaxLength || !authCookieNamePattern.MatchString(cfg.AuthCookieName) {
		return Config{}, fmt.Errorf("AUTH_COOKIE_NAME 非法: %q（只允许 RFC 6265 Cookie 名允许的字符，最长 %d）",
			cfg.AuthCookieName, authCookieNameMaxLength)
	}

	switch {
	case cfg.UploadMaxBytes == 0:
		cfg.UploadMaxBytes = defaultUploadMaxBytes
	case cfg.UploadMaxBytes < 0:
		return Config{}, fmt.Errorf("UPLOAD_MAX_BYTES 不能为负数: %d", cfg.UploadMaxBytes)
	}

	switch {
	case cfg.MaxImagePixels == 0:
		cfg.MaxImagePixels = defaultMaxImagePixels
	case cfg.MaxImagePixels < 0:
		return Config{}, fmt.Errorf("MAX_IMAGE_PIXELS 不能为负数: %d", cfg.MaxImagePixels)
	}

	if cfg.UserQuotaBytes < 0 {
		return Config{}, fmt.Errorf("USER_QUOTA_BYTES 不能为负数: %d", cfg.UserQuotaBytes)
	}

	if !cfg.exportCooldownSet {
		cfg.ExportCooldownHours = defaultExportCooldownHours
	}
	if cfg.ExportCooldownHours < 0 {
		return Config{}, fmt.Errorf("EXPORT_COOLDOWN_HOURS 不能为负数: %d（0 表示关闭冷却）", cfg.ExportCooldownHours)
	}

	switch {
	case cfg.TrashRetentionDays == 0:
		cfg.TrashRetentionDays = defaultTrashRetentionDays
	case cfg.TrashRetentionDays < minTrashRetentionDays:
		return Config{}, fmt.Errorf("TRASH_RETENTION_DAYS 不能小于 %d: %d", minTrashRetentionDays, cfg.TrashRetentionDays)
	}

	// Config 字面量同样要归一化：未 masked 的前缀按网段补齐，非法项在打开数据库前报错。
	proxies, err := normalizeTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return Config{}, err
	}
	cfg.TrustedProxies = proxies

	if !cfg.SMTP.Secure && !cfg.SMTP.requireTLSSet {
		// 未显式关闭时必须要求 STARTTLS：不能静默降级为明文投递。
		cfg.SMTP.RequireTLS = true
	}
	if cfg.SMTP.Host != "" && cfg.SMTP.Port == 0 {
		// Config 字面量只写 Host/From 时同样使用原版默认端口。
		if cfg.SMTP.Secure {
			cfg.SMTP.Port = 465
		} else {
			cfg.SMTP.Port = 587
		}
	}
	if err := validateSMTPConfig(cfg.SMTP); err != nil {
		return Config{}, err
	}

	baseURL, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return Config{}, err
	}
	cfg.BaseURL = baseURL

	if !cfg.cookieSecureSet {
		cfg.CookieSecure = strings.HasPrefix(baseURL, "https://")
	}

	return cfg, nil
}

// parseTrustedProxies 解析 TRUSTED_PROXIES：逗号分隔的 IP 或 CIDR，空字符串表示不信任任何代理。
// 不接受主机名，也不允许 0.0.0.0/0 这类“信任全部来源”的写法——那等于让任意客户端
// 自带 X-Forwarded-For 就能换限流桶，与默认不信任转发头等价不了，必须显式列出代理网段。
func parseTrustedProxies(raw string) ([]netip.Prefix, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	entries := strings.Split(trimmed, ",")
	prefixes := make([]netip.Prefix, 0, len(entries))
	seen := make(map[netip.Prefix]struct{}, len(entries))
	for _, entry := range entries {
		prefix, err := parseTrustedProxy(entry)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[prefix]; duplicate {
			continue
		}
		seen[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

// parseTrustedProxy 解析单个 IP 或 CIDR，并归一化成可直接比对的前缀
// （精确 IP 变成 /32 或 /128，v4-mapped 地址还原成 IPv4）。
func parseTrustedProxy(entry string) (netip.Prefix, error) {
	trimmed := strings.TrimSpace(entry)
	if trimmed == "" {
		return netip.Prefix{}, fmt.Errorf("TRUSTED_PROXIES 含有空项 %q（需要逗号分隔的 IP 或 CIDR）", entry)
	}

	var prefix netip.Prefix
	if strings.Contains(trimmed, "/") {
		parsed, err := netip.ParsePrefix(trimmed)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("TRUSTED_PROXIES 非法 CIDR: %q", trimmed)
		}
		prefix = parsed
	} else {
		addr, err := netip.ParseAddr(trimmed)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("TRUSTED_PROXIES 非法 IP: %q（只接受 IP 或 CIDR，不接受主机名）", trimmed)
		}
		if addr.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("TRUSTED_PROXIES 不接受带 zone 的地址: %q", trimmed)
		}
		addr = addr.Unmap()
		bits := 32
		if addr.Is6() {
			bits = 128
		}
		prefix = netip.PrefixFrom(addr, bits)
	}

	if prefix.Addr().Is4In6() {
		// ::ffff:a.b.c.d/n 只有 n>=96 的部分才描述 IPv4 范围，更短的前缀无法等价映射到 IPv4。
		if prefix.Bits() < 96 {
			return netip.Prefix{}, fmt.Errorf("TRUSTED_PROXIES 不支持宽度小于 /96 的 IPv4-mapped CIDR: %q（请直接写 IPv4 CIDR）", trimmed)
		}
		prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
	}
	prefix = prefix.Masked()
	if !prefix.IsValid() {
		return netip.Prefix{}, fmt.Errorf("TRUSTED_PROXIES 非法: %q", trimmed)
	}
	if prefix.Bits() == 0 {
		return netip.Prefix{}, fmt.Errorf("TRUSTED_PROXIES 不能信任所有地址: %q（只列出实际的反向代理地址或网段）", trimmed)
	}
	return prefix, nil
}

// normalizeTrustedProxies 复用同一套解析规则处理 Config 字面量，保证
// “显式前缀”与“环境变量”得到完全一致的结果。
func normalizeTrustedProxies(prefixes []netip.Prefix) ([]netip.Prefix, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	normalized := make([]netip.Prefix, 0, len(prefixes))
	for _, prefix := range prefixes {
		parsed, err := parseTrustedProxy(prefix.String())
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, parsed)
	}
	return normalized, nil
}

// trustsProxy 判断某个直连对端地址是否在可信代理范围内。
// 调用方必须传入已 Unmap 的地址，保证 v4/v6 表示一致。
func (c Config) trustsProxy(addr netip.Addr) bool {
	for _, prefix := range c.TrustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func validateSMTPConfig(cfg SMTPConfig) error {
	if cfg.Port < 0 || cfg.Port > 65535 {
		return fmt.Errorf("SMTP_PORT 越界: %d", cfg.Port)
	}
	if strings.TrimSpace(cfg.Host) == "" {
		if cfg.From != "" || cfg.Username != "" || cfg.Password != "" {
			return fmt.Errorf("SMTP 配置不完整: 缺少 SMTP_HOST")
		}
		return nil
	}
	if strings.TrimSpace(cfg.From) == "" {
		return fmt.Errorf("SMTP 配置不完整: 设置了 SMTP_HOST 但缺少 SMTP_FROM")
	}
	if cfg.Port <= 0 {
		return fmt.Errorf("SMTP 配置不完整: SMTP_PORT 必须为 1~65535，当前 %d", cfg.Port)
	}
	if (cfg.Username == "") != (cfg.Password == "") {
		return fmt.Errorf("SMTP 配置不完整: SMTP_USER 与 SMTP_PASSWORD 必须同时设置")
	}
	return nil
}

func validateAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("PICBOX_ADDR 非法: %q（需要 host:port，例如 127.0.0.1:3001）", addr)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return fmt.Errorf("PICBOX_ADDR 端口非法: %q", port)
	}
	if host == "" {
		return fmt.Errorf("PICBOX_ADDR 必须显式指定监听地址，默认只监听环回: %q", addr)
	}
	if ip := net.ParseIP(host); ip == nil {
		// 允许主机名，但拒绝空白与路径风格字符。
		if strings.ContainsAny(host, " /\\?#") {
			return fmt.Errorf("PICBOX_ADDR 主机非法: %q", host)
		}
	}
	return nil
}

func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("BASEURL 非法: %q", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("BASEURL 只支持 http/https: %q", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("BASEURL 缺少主机名: %q", raw)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("BASEURL 不能包含用户信息、查询串或片段: %q", raw)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// baseURLOrigin 返回配置 BASEURL 的 scheme://host，用于跨源写请求校验。
func baseURLOrigin(baseURL string) (string, bool) {
	if baseURL == "" {
		return "", false
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), true
}
