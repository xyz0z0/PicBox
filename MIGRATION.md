# 内部契约

本文件记录 PicBox 的 Go 服务内部契约（package `server`）、数据库 schema 与 HTTP 接口。
它源自 PicHome 的 Go 版本（源提交 `9a920c9`），拆分时只改产品名与包路径。

PicBox 是独立产品，**只使用自己新建的数据目录**：不导入旧 Prisma 数据库、旧 `uploads/`、
旧 URL/API 或旧 Android 客户端数据，也不做就地升级。已有外部数据库会在任何改写前被拒绝。

## 实现范围

- 单二进制：Go HTTP + 内嵌 React 静态包 + pure-Go SQLite + pure-Go WebP 缩略图。
  运行机不需要 Node、Go、SQLite 服务、Docker 或图片处理库；Node 只在构建 `frontend/dist` 时使用。
- 账号：首账号免激活码成为管理员，之后注册必须使用激活码（USER）；邮箱密码登录/退出/当前用户、
  API Key 创建/列表/撤销、管理员登录审计。
- 账号闭环：`change-password`（重签当前会话、其他旧会话失效、保留 API Key）、
  忘记密码重置与改邮箱（SMTP 可选；重置会撤销全部 API Key）、30 天回收站自动清理。
- 图片：上传（单处理槽、限长限像素）、URL 导入、分页图库、多标签与批量改标签、可见性、
  批量操作、软删除、私有回收站恢复/彻底删除、原图/缩略图分发、流式 ZIP 导出与成功冷却。
- 管理端 11 条路由：统计、用户、激活码、图片、审计日志（14 个动作）。
- 不做数据迁移、不自动切换任何现有部署，不宣称已经通过生产验收。

## Go 共享接口（package server，目录 internal/server）

```go
type Config struct {
    Addr string
    DataDir string
    BaseURL string
    UploadMaxBytes int64
    MaxImagePixels int64
    UserQuotaBytes int64
    CookieSecure bool
    AuthCookieName string
    ExportCooldownHours int   // 默认 12；WithExportCooldown(0) / EXPORT_COOLDOWN_HOURS=0 关闭
    TrashRetentionDays int    // 默认 30，最小 1
    SMTP SMTPConfig           // 零值表示未配置邮件
}
func LoadConfig() (Config, error)
func New(cfg Config) (*App, error) // 0 值字段使用安全默认值；非法配置报错
func (a *App) Handler() http.Handler
func (a *App) Close() error
// App 字段：db *sql.DB, cfg Config, mux *http.ServeMux,
// uploadSlots chan struct{} (capacity 1), secret []byte,
// backgroundCtx/backgroundCancel/backgroundWG（后台任务唯一生命周期）
type User struct {
    ID int64 `json:"id"`
    Email string `json:"email"`
    Username string `json:"username"`
    Role string `json:"role"`
    SessionVersion int64 `json:"-"`
    MustChangePassword bool `json:"mustChangePassword"`
}
func (a *App) authenticate(w http.ResponseWriter, r *http.Request, allowAPIKey bool) (*User, bool)
func writeJSON(w http.ResponseWriter, status int, value any)
func writeError(w http.ResponseWriter, status int, code, message string)
func (a *App) registerAuthRoutes()
func (a *App) registerImageRoutes()
func (a *App) registerImagesExtraRoutes()
func (a *App) registerAdminRoutes()
func (a *App) registerWebRoutes()
```

`New` 依次注册上述路由，并在启动恢复中断的删除之后启动邮件 worker 与回收站保留期清理。
错误格式为 `{ok:false,error:{code,message}}`，`/api/auth/me` 沿用顶层 `code/message`。
Cookie 默认名 `picbox_token`，可用 `AUTH_COOKIE_NAME` 配置。
HS256 JWT 要求 userId/sessionVersion/exp，缺少必需字段时拒绝。
Bearer 仅认证 API Key；个人列表、回收站、改密、API Key 管理与管理端只接受 Cookie。
写入接口拒绝跨源 Origin（无 Origin 的 CLI 请求允许）。限流分桶：默认只用 TCP 对端地址，
只有直连对端在 `TRUSTED_PROXIES`（IP/CIDR，默认空）范围内时才解析 `X-Forwarded-For` 并从右往左
剔除可信代理，首个不可信地址即真实客户端；不读 `X-Real-IP`，未设可信代理时反代部署的限流桶会全站共享。

**后台任务生命周期**：`backgroundCtx`/`backgroundCancel`/`backgroundWG` 由 `New` 只创建一次，
邮件 worker 与回收站保留期清理共用；子模块不得覆盖 `backgroundCancel`。`Close` 只 cancel 一次，
再 `backgroundWG.Wait()`，最后关闭数据库。回归用例 `TestCloseStopsMailAndRetentionPromptly`
守住「两组后台任务同时启动时 Close 仍及时返回」。

## 数据库（core 创建，各 worker 只能使用本契约）

所有日期存储为 **UTC 毫秒时间戳字符串**（`2006-01-02T15:04:05.000Z`，字面 `Z`，
`imagesTimeLayout` 与 `coreTimeLayout` 完全一致）；因此 TEXT 列的字符串比较等价于时间先后，
同一秒内的先后也能区分。单连接 SQLite，foreign_keys=ON，busy_timeout，有限页缓存。

`User`：id INTEGER PK AUTOINCREMENT、email TEXT UNIQUE NOT NULL、username TEXT UNIQUE NOT NULL、
passwordHash TEXT NOT NULL、role TEXT NOT NULL、disabledAt TEXT NULL、sessionVersion INTEGER DEFAULT 0、
mustChangePassword INTEGER DEFAULT 0、quotaBytes INTEGER NULL、lastExportAt TEXT NULL、
exportReservationAt TEXT NULL、exportReservationToken TEXT NULL、createdAt TEXT NOT NULL。

`ApiKey`：id TEXT PK、userId INTEGER FK User、name TEXT、keyHash TEXT UNIQUE、prefix TEXT、
createdAt TEXT、lastUsedAt TEXT NULL、revokedAt TEXT NULL。rawKey 永不入库。

`Image`：id TEXT PK、uploaderId INTEGER FK User、originalName TEXT、originalMimeType TEXT、size INTEGER、
sha256 TEXT、width INTEGER、height INTEGER、originalPath TEXT、thumbPath TEXT、createdAt TEXT、
updatedAt TEXT、deletedAt TEXT NULL、deletedByAdmin INTEGER DEFAULT 0、isVisible INTEGER DEFAULT 1。

`ImageTag`：imageId TEXT FK Image ON DELETE CASCADE、name TEXT、PRIMARY KEY(imageId,name)。

`AuditLog`：id INTEGER PK AUTOINCREMENT、actorId FK User、action TEXT CHECK（原版 14 个动作）、
targetUserId FK User NULL、detail TEXT NULL、createdAt TEXT；索引 createdAt / action。

`ActivationCode`：id TEXT PK、codeHash TEXT UNIQUE、maskedCode TEXT、note TEXT NULL、
createdById FK User、usedById FK User NULL、createdAt TEXT、expiresAt TEXT NULL、usedAt TEXT NULL、
revokedAt TEXT NULL；索引 createdAt / codeHash。只保存哈希，完整码只在生成响应里出现一次。

`AccountToken`：id TEXT PK、userId FK User、purpose CHECK('PASSWORD_RESET','EMAIL_CHANGE')、
tokenHash TEXT UNIQUE、email TEXT、currentEmail TEXT、sessionVersion INTEGER、expiresAt TEXT、
createdAt TEXT、UNIQUE(userId,purpose)。

### schema 版本与打开校验

- 当前 `schema_version = 2`，由 Go 服务自建并在 `picbox_meta` 写入标记。
- 打开**非空**数据库时，在任何改写型 PRAGMA/建表之前校验标记：
  外部/Prisma 数据库 → `ErrForeignDatabase`；`schema_version=1`（旧版 schema）或未来版本 →
  `ErrIncompatibleSchemaVersion`，错误信息写明新旧版本。
- **没有就地升级**。请用另一个全新的空数据目录启动本版；不要删除或覆盖用户原有数据，
  需要旧数据时先整体备份。不要通过 `CREATE IF NOT EXISTS` 静默接管外部 schema。

## HTTP 闭环

账号与凭据：

- POST `/api/auth/register`：`{email,password,activationCode?}` → `{ok:true,user}` + Cookie。
  首个账号免码且 `role=ADMIN`；之后缺码 400 `ACTIVATION_CODE_REQUIRED`、无效码
  `INVALID_ACTIVATION_CODE`、并发冲突 `ACTIVATION_CODE_CONFLICT`、邮箱占用 409 `EMAIL_EXISTS`；
  消耗激活码时写 `ACTIVATION_CODE_USED` 审计（actor = 新用户）。
- POST `/api/auth/login`：成功 `{ok:true,user}` + Cookie；`INVALID_CREDENTIALS` 400、
  `ACCOUNT_DISABLED` 403、`RATE_LIMITED` 429；管理员登录写 `ADMIN_LOGIN` 审计。
- GET `/api/auth/me` → `{user}`（错误用顶层 `code/message`）；POST `/api/auth/logout` → `{ok:true}`。
- POST `/api/auth/change-password` `{oldPassword,newPassword}`：仅 Cookie。成功后清
  `mustChangePassword`、`sessionVersion+1`、重签当前 Cookie（当前会话保持）、**保留 API Key**；
  其他旧 JWT 401。错误码 `INVALID_PASSWORD` / `SAME_PASSWORD` / `INVALID_CREDENTIALS`。
- POST `/api/auth/forgot-password` `{email}`：恒 200（不泄露账号存在性），非法邮箱 400、
  限流 429、未配置邮件 503 `MAIL_UNAVAILABLE`。
- POST `/api/auth/reset-password` `{token,password}`：64 hex + purpose/过期/邮箱/sessionVersion/未禁用校验；
  成功删令牌、改密、`sessionVersion+1`、撤销全部 API Key、清 Cookie。
- POST `/api/auth/change-email` `{email,password}`、POST `/api/auth/change-email/confirm` `{token}`：
  一次性令牌发到新邮箱，确认后改邮箱 + `sessionVersion+1` + 清 Cookie（不撤销 API Key）。
- POST `/api/auth/password` `{currentPassword,newPassword}`：**历史内部入口**，语义为改密后撤销全部
  API Key 并退出登录；界面不调用，保留仅为兼容早期脚本。
- API Key：GET `/api/apikeys`、POST `/api/apikeys` `{name}`（返回一次 `rawKey`）、
  DELETE `/api/apikeys/{id}`。

图片：

- POST `/api/images`：multipart `file` + `tags`（JSON 数组字符串）；兼容单值 `tag`，拒绝混用。
  与 `/api/images/from-url` 共用 100 次/小时/IP 的上传频率桶（超限 429 `RATE_LIMITED` +
  `Retry-After`，不读请求体）；单处理槽的 429 `UPLOAD_BUSY` 仍保留，两者互不替代。
- POST `/api/images/from-url` `{url,tags?}`：仅 http/https、无用户信息、长度有界；复用真实上传
  校验与配额。
- GET `/api/images?mine=true&limit=&cursor=&visibility=&query=&tag=&tagMatch=&sort=&deleted=` →
  `{ok:true,items,nextCursor,total}`；字段含 id/originalName/tag/tags/createdAt/isVisible/deletedAt/
  width/height/size/originalMimeType/url/pageUrl/thumbUrl。limit 上限 50。
- GET `/api/images/tags` → `{ok:true,tags}`（本人未删除图片的标签并集）。
- GET `/api/images/usage` → `{ok:true,usedBytes,quotaBytes}`；用量含回收站与管理员下架。
- GET/HEAD `/api/images/{id}?variant=original|thumb`：原图字节保留、支持 Range/ETag/304；
  隐藏图片返回固定 SVG 占位；软删除后仅上传者 Cookie 可读，API Key 不行。
- GET `/api/images/{id}/meta`：详情页元数据（id/originalName/originalMimeType/size/width/height/
  createdAt/tags/isVisible/url/pageUrl/thumbUrl）。不存在、软删除、管理员下架一律 404；
  隐藏图片只有上传者本人 Cookie 可读。
- PATCH `/api/images/{id}` `{visible?,tags?}` → 整体替换标签；拒绝 `visible:null`/`tags:null`。
- DELETE `/api/images/{id}`：本人软删除 → `{ok:true}`。
- POST `/api/images/{id}/restore`：仅本人 Cookie；未软删 409。
- DELETE `/api/images/{id}?permanent=true`：仅回收站记录；成功
  `{ok:true,cleanupPending}`。先写持久化清单，再暂存字节，数据库提交后清理；
  进程中断后启动恢复。`cleanupPending:true` 表示记录/标签已删、逻辑配额已释放，但暂存文件
  仍占物理磁盘。启动恢复也覆盖空的 `data/tmp/purge-*` 目录（无清单时只清理空目录，
  不动其他文件）；清单非法或无法恢复时拒绝启动并保留现场。
- PATCH `/api/images/batch` `{ids,action,visible?,tags?,mode?}`：action ∈
  visibility/delete/restore/purge/tag；ids 去重、上限 500；响应 `{ok:true,updatedCount}`。
  `restore`/`purge` 与单张接口一样**只接受登录 Cookie**（API Key 不能恢复或删除字节，
  对应 `TestImagesBatchTrashActionsRequireCookie`）；`visibility`/`delete`/`tag` 允许 Bearer。
  purge 另报 `updatedIds`（精确成功、可从 UI 移除的 id）、`cleanupPending`、`failedCount`，
  `updatedCount` 只计真正删掉的行，失败项不得出现在 `updatedIds` 中。
- GET `/api/images/export`：`scope=all` 或重复 `ids=`（≤100）。成功头含
  `X-Export-Count` / `X-Export-Bytes` / `X-Export-Cooldown-Hours`；HEAD 只预检、不 claim 预约、
  不消耗冷却；成功后才写冷却，进行中导出有独立预约与 10 分钟可续租租约，并发只有一个赢家。
  归档含 `README.txt`、`manifest.json`、`images/<id>.<ext>`；隐藏图片也导出，
  回收站/管理员下架不导出。
- GET `/api/config`：公开、无需登录的安全配置 `{ok:true,uploadMaxBytes}`。
  静态页用它把当前上传上限传给原组件，因此改 `UPLOAD_MAX_BYTES` 不需要重新构建前端。
- 未实现的 API 明确返回 JSON 404，不能回落到成功的网页。

管理端（仅 Cookie 管理员，非管理员 403 `FORBIDDEN`，API Key 401）：

- GET `/api/admin/stats`
- GET/POST `/api/admin/users`、PATCH `/api/admin/users/{id}`
  （operation: `disable`/`enable`/`reset_password`/`reset_export_cooldown`/`set_quota`/`set_email`）
- GET/POST `/api/admin/activation-codes`、PATCH `/api/admin/activation-codes/{id}`（`revoke`）
- GET `/api/admin/images`、PATCH `/api/admin/images/{id}`（`hide`/`show`）、
  DELETE `/api/admin/images/{id}`
- GET `/api/admin/audit-logs`

幂等写操作的成功标记必须反映「这次真的改了什么」：`set_quota`、`reset_export_cooldown`、
激活码撤销都返回 `changed`，no-op 时 `changed:false` 并仍写审计。
业务写入与对应审计行在同一事务内提交，审计失败整体回滚。

页面：`/`、`/dashboard`、`/admin`、`/forgot-password`、`/reset-password`、`/verify-email`
与 `/images/{id}` 都由嵌入的 `frontend/dist/index.html` 静态提供。已注册页面的尾斜杠地址返回
HTTP 308，保留查询参数。未知网页地址、无效图片 ID 和当前访问者不可见的图片详情页返回 HTTP 404
与同一个静态壳。未注册的 `/api/*` 返回 JSON 404。`/assets/{name}` 只服务白名单扩展名
（`.css/.js/.svg`）的单段文件。

上传默认最大 5MiB、最大 1600 万像素（解码前检查，宽高相乘防溢出），最多 1 个上传处理任务；
满载立即 429 `UPLOAD_BUSY`。原图只接受真实 JPEG/PNG/GIF/WebP；SVG 拒绝；缩略图不超过 400×400，
小图不放大。GIF/WebP 原图动画字节保持、缩略图取首帧；动画 WebP 按 RFC 9649 只解码首帧、
合成到 `VP8X` 画布（支持 `ANMF` 偏移与 `ALPH`），数据库宽高为画布尺寸。
WebP 缩略图用 nativewebp 的 lossless 编码。配额统计含软删除图片；
写入事务内再次校验配额，失败清理文件。存储路径只由服务生成。
