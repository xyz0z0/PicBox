package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	tokenTTL           = 7 * 24 * time.Hour
	bcryptCost         = 10
	minPasswordBytes   = 6
	maxPasswordBytes   = 72 // bcrypt 只使用前 72 字节，超长必须显式拒绝
	maxEmailBytes      = 254
	maxAPIKeyNameBytes = 100
	maxBearerBytes     = 512
	bcryptWaitTimeout  = 5 * time.Second
)

// 邮箱不存在时也执行一次同成本 bcrypt 比较，避免通过响应时间探测账号存在性。
const dummyPasswordHash = "$2b$10$mbU/oRePwIsd7xQm4OyyKO84QOIevRtUgg.pWeSHZrfm5FJRXHb1e"

var (
	emailPattern  = regexp.MustCompile(`^[a-z0-9.!#$%&'*+/=?^_{|}~-]+@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
	errBcryptBusy = errors.New("bcrypt 并发已满")
	errNoUser     = errors.New("未找到用户")
)

// User 是对外暴露的账号视图；SessionVersion 不进入 JSON。
type User struct {
	ID                 int64  `json:"id"`
	Email              string `json:"email"`
	Username           string `json:"username"`
	Role               string `json:"role"`
	SessionVersion     int64  `json:"-"`
	MustChangePassword bool   `json:"mustChangePassword"`
}

type authClaims struct {
	UserID         claimInt64 `json:"userId"`
	SessionVersion claimInt64 `json:"sessionVersion"`
	jwt.RegisteredClaims
}

// claimInt64 记录字段是否真的出现在 JWT JSON 中。
// jwt/v5 会把缺失字段解成零值，单靠 int64 无法区分“缺字段”和“合法 0”；
// 认证契约要求 userId/sessionVersion 必须存在且为非负整数，因此这里显式跟踪 presence，
// 并只接受 JSON 整数（小数、字符串、布尔值都会让验签后的解析失败）。
type claimInt64 struct {
	value   int64
	present bool
}

func (c *claimInt64) UnmarshalJSON(data []byte) error {
	// 只接受 JSON 数字字面量：json.Number 会接受带引号的数字字符串，因此这里先看原始字节。
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || (trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9')) {
		return fmt.Errorf("需要整数: %s", data)
	}
	parsed, err := strconv.ParseInt(string(trimmed), 10, 64)
	if err != nil {
		return fmt.Errorf("需要整数: %w", err)
	}
	c.value = parsed
	c.present = true
	return nil
}

func (c claimInt64) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(c.value, 10)), nil
}

type userRecord struct {
	user         User
	passwordHash string
	disabled     bool
	quotaBytes   sql.NullInt64
}

func (a *App) signToken(userID, sessionVersion int64) (string, error) {
	now := time.Now()
	claims := authClaims{
		UserID:         claimInt64{value: userID, present: true},
		SessionVersion: claimInt64{value: sessionVersion, present: true},
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(tokenTTL)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.secret)
}

func (a *App) parseToken(raw string) (authClaims, error) {
	var claims authClaims
	// 固定 HS256，并强制要求 exp。
	if _, err := jwt.ParseWithClaims(raw, &claims, func(token *jwt.Token) (any, error) {
		return a.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired(), jwt.WithLeeway(30*time.Second)); err != nil {
		return authClaims{}, err
	}
	if !claims.UserID.present || claims.UserID.value <= 0 ||
		!claims.SessionVersion.present || claims.SessionVersion.value < 0 {
		return authClaims{}, errors.New("token 载荷非法")
	}
	return claims, nil
}

func (a *App) setAuthCookie(w http.ResponseWriter, raw string) {
	http.SetCookie(w, &http.Cookie{
		Name:     a.cfg.AuthCookieName,
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(tokenTTL / time.Second),
		Expires:  time.Now().Add(tokenTTL),
	})
}

func (a *App) clearAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     a.cfg.AuthCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

// authFailure 描述认证失败，便于 /api/auth/me 使用顶层 code/message 返回。
type authFailure struct {
	status  int
	code    string
	message string
}

// authenticate 实现 Cookie 会话与（可选）API Key 认证。
// allowAPIKey=false 时 Bearer 头不会被接受，API Key 管理接口因此只能用 Cookie。
func (a *App) authenticate(w http.ResponseWriter, r *http.Request, allowAPIKey bool) (*User, bool) {
	user, failure := a.resolveUser(r, allowAPIKey)
	if failure != nil {
		writeError(w, failure.status, failure.code, failure.message)
		return nil, false
	}
	return user, true
}

func (a *App) resolveUser(r *http.Request, allowAPIKey bool) (*User, *authFailure) {
	if rawKey, ok := bearerToken(r); ok {
		if !allowAPIKey {
			return nil, &authFailure{http.StatusUnauthorized, "UNAUTHORIZED", "此接口仅接受登录 Cookie，不接受 API Key"}
		}
		user, status, code, message := a.authenticateAPIKey(r.Context(), rawKey)
		if user == nil {
			return nil, &authFailure{status, code, message}
		}
		return user, nil
	}

	cookie, err := r.Cookie(a.cfg.AuthCookieName)
	if err != nil || cookie.Value == "" {
		return nil, &authFailure{http.StatusUnauthorized, "UNAUTHORIZED", "未登录或登录已过期"}
	}

	claims, err := a.parseToken(cookie.Value)
	if err != nil {
		return nil, &authFailure{http.StatusUnauthorized, "UNAUTHORIZED", "未登录或登录已过期"}
	}

	record, err := a.loadUserByID(r.Context(), claims.UserID.value)
	switch {
	case errors.Is(err, errNoUser):
		return nil, &authFailure{http.StatusUnauthorized, "UNAUTHORIZED", "未登录或登录已过期"}
	case err != nil:
		return nil, &authFailure{http.StatusInternalServerError, "INTERNAL_ERROR", "读取账号失败"}
	}

	// 递增 sessionVersion 会让此前签发的所有 token 失效。
	if record.user.SessionVersion != claims.SessionVersion.value {
		return nil, &authFailure{http.StatusUnauthorized, "UNAUTHORIZED", "未登录或登录已过期"}
	}
	if record.disabled {
		return nil, &authFailure{http.StatusForbidden, "ACCOUNT_DISABLED", "账号已被禁用"}
	}
	return &record.user, nil
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}

func (a *App) authenticateAPIKey(ctx context.Context, rawKey string) (*User, int, string, string) {
	if rawKey == "" || len(rawKey) > maxBearerBytes {
		return nil, http.StatusUnauthorized, "INVALID_API_KEY", "API Key 无效或已撤销"
	}

	record, keyID, err := a.loadUserByAPIKey(ctx, hashAPIKey(rawKey))
	switch {
	case errors.Is(err, errNoUser):
		return nil, http.StatusUnauthorized, "INVALID_API_KEY", "API Key 无效或已撤销"
	case err != nil:
		return nil, http.StatusInternalServerError, "INTERNAL_ERROR", "读取 API Key 失败"
	}

	if record.disabled {
		return nil, http.StatusForbidden, "ACCOUNT_DISABLED", "账号已被禁用"
	}

	// lastUsedAt 只是审计信息，写入失败不影响本次认证。
	_, _ = a.db.ExecContext(ctx,
		`UPDATE "ApiKey" SET lastUsedAt = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), keyID)

	return &record.user, 0, "", ""
}

func hashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (a *App) loadUserByID(ctx context.Context, id int64) (userRecord, error) {
	row := a.db.QueryRowContext(ctx, `SELECT `+userSelectColumns+` FROM "User" WHERE id = ?`, id)
	return scanUser(row)
}

func (a *App) loadUserByEmail(ctx context.Context, email string) (userRecord, error) {
	row := a.db.QueryRowContext(ctx, `SELECT `+userSelectColumns+` FROM "User" WHERE email = ?`, email)
	return scanUser(row)
}

func (a *App) loadUserByAPIKey(ctx context.Context, keyHash string) (userRecord, string, error) {
	row := a.db.QueryRowContext(ctx, `SELECT k.id, `+prefixedUserSelectColumns+`
		FROM "ApiKey" k JOIN "User" u ON u.id = k.userId
		WHERE k.keyHash = ? AND k.revokedAt IS NULL`, keyHash)

	var keyID string
	var record userRecord
	var mustChange int
	var disabled sql.NullString
	if err := row.Scan(
		&keyID,
		&record.user.ID,
		&record.user.Email,
		&record.user.Username,
		&record.user.Role,
		&record.user.SessionVersion,
		&mustChange,
		&record.passwordHash,
		&disabled,
		&record.quotaBytes,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return userRecord{}, "", errNoUser
		}
		return userRecord{}, "", err
	}
	record.user.MustChangePassword = mustChange != 0
	record.disabled = disabled.Valid
	return record, keyID, nil
}

const userSelectColumns = `id, email, username, role, sessionVersion, mustChangePassword, passwordHash, disabledAt, quotaBytes`

const prefixedUserSelectColumns = `u.id, u.email, u.username, u.role, u.sessionVersion, u.mustChangePassword, u.passwordHash, u.disabledAt, u.quotaBytes`

func scanUser(row *sql.Row) (userRecord, error) {
	var record userRecord
	var mustChange int
	var disabled sql.NullString
	if err := row.Scan(
		&record.user.ID,
		&record.user.Email,
		&record.user.Username,
		&record.user.Role,
		&record.user.SessionVersion,
		&mustChange,
		&record.passwordHash,
		&disabled,
		&record.quotaBytes,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return userRecord{}, errNoUser
		}
		return userRecord{}, err
	}
	record.user.MustChangePassword = mustChange != 0
	record.disabled = disabled.Valid
	return record, nil
}

func (a *App) hashPassword(password string) ([]byte, error) {
	var hashed []byte
	err := a.withBcryptSlot(func() error {
		var err error
		hashed, err = bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
		return err
	})
	if err != nil {
		return nil, err
	}
	return hashed, nil
}

func (a *App) comparePassword(hash, password string) error {
	return a.withBcryptSlot(func() error {
		return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	})
}

// withBcryptSlot 给高成本哈希/比较加并发上限，避免大量并发请求把 CPU 打满。
func (a *App) withBcryptSlot(fn func() error) error {
	select {
	case a.bcryptSlots <- struct{}{}:
		defer func() { <-a.bcryptSlots }()
		return fn()
	case <-time.After(bcryptWaitTimeout):
		return errBcryptBusy
	}
}

func normalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if len(email) < 3 || len(email) > maxEmailBytes || !emailPattern.MatchString(email) {
		return "", errors.New("邮箱格式不正确")
	}
	return email, nil
}

func validatePassword(password string) error {
	if len(password) < minPasswordBytes {
		return fmt.Errorf("密码至少 %d 位", minPasswordBytes)
	}
	if len(password) > maxPasswordBytes {
		return fmt.Errorf("密码最多 %d 字节", maxPasswordBytes)
	}
	return nil
}

// X-Forwarded-For 只在直连对端是可信代理（TRUSTED_PROXIES）时才解析，
// 并且跳数与单项长度都有上界：一个超长头不能带来无界的解析成本。
const (
	maxForwardedHops      = 32
	maxForwardedEntrySize = 64
)

// peerIP 严格解析 r.RemoteAddr 里的直连对端地址。
func peerIP(r *http.Request) (netip.Addr, bool) {
	raw := strings.TrimSpace(r.RemoteAddr)
	if raw == "" {
		return netip.Addr{}, false
	}
	host := raw
	if splitHost, _, err := net.SplitHostPort(raw); err == nil {
		host = splitHost
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap().WithZone(""), true
}

// clientIP 返回直连对端地址，不读取任何转发头。
// 它是不信任转发头的纯直连实现，也是 a.clientIP 的兜底。
func clientIP(r *http.Request) string {
	if addr, ok := peerIP(r); ok {
		return addr.String()
	}
	return strings.TrimSpace(r.RemoteAddr)
}

// clientIP 返回用于限流分桶的客户端地址。
//
// 直连对端不在 TRUSTED_PROXIES 内时一律忽略所有转发头（伪造 X-Forwarded-For /
// X-Real-IP 不能换桶）。对端可信时才解析 X-Forwarded-For，并从右往左剥离受信任的
// 代理，第一个不可信的地址就是真实客户端：左侧前缀由客户端自带、可任意伪造，因此
// 永远不会被采用。X-Real-IP 之类的单值头不参与判定——代理必须正确设置 XFF。
func (a *App) clientIP(r *http.Request) string {
	peer, ok := peerIP(r)
	if !ok {
		// RemoteAddr 不是合法 IP（测试或非常规传输）：保持原样，且不据此信任任何头。
		return strings.TrimSpace(r.RemoteAddr)
	}
	if !a.cfg.trustsProxy(peer) {
		return peer.String()
	}
	hops, ok := forwardedHops(r)
	if !ok {
		return peer.String()
	}

	current := peer
	for i := len(hops) - 1; i >= 0; i-- {
		if !a.cfg.trustsProxy(current) {
			return current.String()
		}
		current = hops[i]
	}
	return current.String()
}

// forwardedHops 解析并严格校验 X-Forwarded-For。
// 任何一项不是裸 IP（主机名、带端口、带 zone、空项、超长）都视为整条链不可信并
// 返回 false，避免按“部分可信”的链分桶。
func forwardedHops(r *http.Request) ([]netip.Addr, bool) {
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return nil, false
	}
	// 多个同名头按 HTTP 语义并成一条链；代理追加的地址在最后。
	raw := strings.Join(values, ",")
	if strings.TrimSpace(raw) == "" {
		return nil, false
	}
	entries := strings.Split(raw, ",")
	if len(entries) > maxForwardedHops {
		return nil, false
	}
	hops := make([]netip.Addr, 0, len(entries))
	for _, entry := range entries {
		value := strings.TrimSpace(entry)
		if value == "" || len(value) > maxForwardedEntrySize || strings.Contains(value, "%") {
			return nil, false
		}
		addr, err := netip.ParseAddr(value)
		if err != nil {
			return nil, false
		}
		hops = append(hops, addr.Unmap())
	}
	return hops, true
}

func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"ok": false,
		"error": map[string]string{
			"code":    "RATE_LIMITED",
			"message": "请求过于频繁，请稍后再试",
		},
		"retryAfterMs": retryAfter.Milliseconds(),
	})
}

// decodeJSONBody 限制请求体大小，并把超限与格式错误区分开。
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, defaultJSONBodyLimit)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "请求体超过大小限制")
			return false
		}
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "请求格式不正确")
		return false
	}
	return true
}
