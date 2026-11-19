# PicBox 浏览器前端（Go 单二进制用）

PicBox 的界面是一个**构建期生成的静态 React 包**：首页、`/dashboard` 工作台（画廊墙 / 接触印相表两种视图）、
`/admin` 管理端、图片详情页、三个账号恢复页都沿用 PicHome 原版组件与原样式表（现位于本目录的
`app/`、`lib/`），只改了产品名与路径。构建产物是 `dist/index.html + dist/assets/*`，由 `embed.go` 嵌入 Go 二进制，
`internal/server/web.go` 直接对外提供；浏览器只与同源 `/api/*` 通信。

- 运行期**不依赖 Node、Prisma、Docker 或任何 CDN**；Node 只在重建 `dist/` 时使用。
- 页面与资源全部来自 `dist/`；Go 侧没有第二套 HTML/JS。
- 组件来自 PicHome（源提交 `9a920c9`，MIT 许可，见仓库根 `LICENSE`）；本目录只做静态化改造。
- React 等运行时依赖的许可原文见 [`THIRD_PARTY_LICENSES.txt`](./THIRD_PARTY_LICENSES.txt)，
  Vite 将其保留在 JavaScript 文件头，`check-dist` 检查声明完整且与原文一致。
  更新运行时依赖时须同步这份声明与根目录 [`THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md)。
- 本目录的 `pnpm verify` 只覆盖构建、类型与 jsdom 行为测试，**不代表**浏览器交互已验收。

## 构建与检查

```sh
cd frontend
npx -y pnpm@10 install --frozen-lockfile   # 只装本目录依赖
npx -y pnpm@10 run typecheck               # 类型检查（TypeScript 7.0.2）
npx -y pnpm@10 run test                    # 离线行为测试（Vitest + jsdom，不启动浏览器）
npx -y pnpm@10 run build                   # 输出 dist/
npx -y pnpm@10 run check:dist              # 产物断言（见下）
npx -y pnpm@10 verify                      # = 以上四步
```

前端改动（含 `app/**`、`lib/**`、`src/**` 与 `package.json` 的 `0.0.1` 版本号）之后必须重新
`pnpm verify` 并提交 `dist/`，否则 Go 二进制会嵌入陈旧 UI。Go CI（`.github/workflows/go.yml`）
会安装 Node 24 + pnpm 10、以 `--frozen-lockfile` 只安装本目录依赖、运行 `pnpm verify`
（类型检查、离线行为测试、重建、产物断言），并用
`git status --porcelain --untracked-files=all` + `git diff --exit-code` 检查 `frontend/dist`
与提交内容逐字节一致（未跟踪的新哈希文件也算不一致）。

上传上限是**运行时**配置：工作台加载时请求 `GET /api/config`，把 `uploadMaxBytes`
作为可选 prop 传给 `DashboardClient` → `UploadDialog`。修改 `UPLOAD_MAX_BYTES` 不需要重新构建前端；
配置缺失/非 2xx/非法时显示明确错误与重试，不静默回落到构建期默认值。小于 1MiB 的合法值按 KB
显示（不会出现 `0MB`），例如 `512KiB` → 「512KB」、`10MiB` → 「10MB」。
组件里原有的 `process.env.NEXT_PUBLIC_UPLOAD_MAX_BYTES` 读法由 `vite.config.ts` 在构建期替换为
空字符串，只作为「未传 prop 时的历史默认」；PicBox 页面总会传运行时值。

当前产物（Vite 7.3.8，production）：

| 文件 | 体积 | gzip |
| --- | --- | --- |
| `dist/index.html` | 0.62 kB | 0.45 kB |
| `dist/assets/index-*.css` | 47.89 kB | 8.93 kB |
| `dist/assets/index-*.js` | 369.71 kB | 112.09 kB |
| `dist/assets/favicon-*.svg` | 0.28 kB | 0.22 kB |

`dist/` 与源码一起提交：Go 发布脚本和 CI 都不把 Node 当成运行/编译期依赖。仓库根
`.gitignore` 只忽略根目录的 `/dist/`（发布包输出），本目录的 `dist/` 因此保持被跟踪。

### 依赖隔离与单一 React 副本

`tsconfig.json` 不读取父目录的 `node_modules`：`types: []` + `paths` 把
`react` / `react-dom` 指到本目录的 `node_modules/@types`，把
`@/app/generated/prisma/enums` 指到 `src/shims/prisma-enums.ts`（`app/generated` 不存在于本仓库，
构建也不需要 `prisma generate`）。`app/**`、`lib/**` 只依赖 React 与纯函数，
因此在一个只有本目录内容的干净环境里也能 typecheck + build。
`scripts/check-dist.mjs` 另外断言产物里不存在 `process.env`、`NEXT_PUBLIC_`、`next/*`、
`PrismaClient` 或 Node 内建模块。

### 离线行为测试

`src/tests/dashboard-behavior.test.tsx` 用 Vitest + jsdom 直接渲染 `DashboardPage` /
`DashboardClient` / `AccountRecoveryScreen` / `LoginCard` 并 mock `fetch`，不启动浏览器或预览服务；
`src/tests/ui-fallbacks.test.tsx` 覆盖未知路径的 404 页与渲染异常时的错误边界。共 30 条用例
（22 + 8）：

- 运行时 `10MiB` 显示为「单张最大 10MB」，`512KiB` 显示为「512KB」而不是 `0MB`；
- `/api/config` 失败或字段非法时显示明确错误，点「重试」后恢复；
- `mailEnabled:false` 时登录页与找回密码页给出「联系管理员」提示（不提交必然 503 的请求）、
  账号设置的「修改邮箱」禁用；`mailEnabled:true` 保持原入口；配置读取失败时保留原入口；
- 彻底删除部分成功时只移除 `updatedIds`、失败项仍可见且保持选中、`failedCount` /
  `cleanupPending` 提示可见（明确说明需管理员检查日志并重启服务重试，不暗示定时自动重试）；
  重试只提交失败项；`updatedIds: []` 时一张都不移除；purge 请求整体失败时确认框关闭、丢弃旧 id
  快照，不会留下可再次提交旧 id 的确认按钮；
- 改密成功提示可见，且这次不触发整页 reload。
- 注册说明区分首账号免激活码与后续账号；邮件未启用时找回提示位于密码框下方并关联辅助说明；
  登录网络失败、服务端报错均可重试，请求期间禁止切换登录/注册，失败后恢复切换。

`vitest` / `jsdom` 只是 `devDependencies`，不会进入 `dist` 或 Go 二进制。

## 组件复用方式

不重写 UI，直接打包本目录内的原组件（`vite.config.ts` 的别名与 JSX 配置）：

| 文件 | 处理 |
| --- | --- |
| `app/dashboard/ChangePasswordScreen.tsx`、`ChangePasswordDialog.tsx`、`EmailChangeDialog.tsx`、`UploadDialog.tsx`、`ExportDialog.tsx`、`ImageDialogs.tsx`、`ImageLightbox.tsx`、`loading.tsx` | 原样打包 |
| `app/dashboard/DashboardClient.tsx`、`AccountRecoveryScreen.tsx` | 保留原 UI，新增运行时配置的可选属性（见下表） |
| `app/dashboard/LoginCard.tsx` | 保留原 UI；补充邮件能力属性、注册说明、网络失败反馈与提交期间的切换保护 |
| `app/admin/AdminClient.tsx`、`ActivationCodesTab.tsx`、`loading.tsx` | 原样打包 |
| `app/images/[id]/CopyButtons.tsx` | 原样打包 |
| `app/components/Dialogs.tsx`、`ui.ts`、`HomeView.tsx` | 原样打包（`HomeView.tsx` 是原首页的展示层） |
| `app/globals.css` | 保留原样式，补齐静态入口 `#root` 的弹性布局，修复暗色登录背景的覆盖顺序 |
| `app/home.css`、`app/images/[id]/image-page.css` | 原样引入 |
| `lib/{imageTag,quota,clipboardText,clipboardImage,uploadLimit,uploadConcurrency}.ts` | 原样打包（纯浏览器函数） |
| `next/navigation` | `src/shims/next-navigation.ts`：`useRouter().refresh()` 改成真实的 `location.reload()`，登录/退出/改密后会重新读 Cookie，不是空函数 |
| `@/app/generated/prisma/enums` | `src/shims/prisma-enums.ts`：静态包只需要 `UserRole` 两个字符串 |

这些账号与工作台组件通过**可选**属性接入 Go 部署的运行时配置；不传时保留原配置分支。
登录卡片另补充了错误反馈与说明排版，激活码注册、登录及邮件接口契约不变：

| 属性 | 作用 |
| --- | --- |
| `LoginCard.mailEnabled?: boolean` | `false` 时把「忘记密码？」换成「请联系管理员重置密码」文案；`true`/未传保持原链接 |
| `DashboardClient.mailEnabled?: boolean` | `false` 时账号设置里的「修改邮箱」按钮禁用，并提示联系管理员在后台修改 |
| `AccountRecoveryScreen`（`forgot` 模式） | 先读 `GET /api/config`；`mailEnabled:false` 时用提示替换表单，不再提交必然 503 的请求 |

`reset`/`verify` 两种模式（从邮件链接进入）不看这个标志；激活码注册与登录完全不依赖它。

构建期校验过的两点：

- **React 只有一份**：`resolve.alias` 把 `react` / `react/jsx-runtime` / `react-dom` / `react-dom/client`
  指向 `frontend/node_modules`。打包模块列表里只有该目录下的
  `react.production.js` / `react-dom-client.production.js`。
- **没有 Node 侧代码进浏览器**：见上文 `check-dist.mjs` 的断言。

### 上传并发

服务端只提供 1 个上传处理槽，满载返回 429 `UPLOAD_BUSY`。`lib/uploadConcurrency.ts` 因此把
`DEFAULT_UPLOAD_CONCURRENCY` 定为 **1**；`UploadDialog` 仍接受可选 `concurrency` 属性、
`DashboardClient` 显式传入该常量，UI 的排队/进度/重试逻辑保持原样。

## 路由表与 Go 页面 handler

`src/main.tsx` 通过 `src/routes.tsx` 按 `location.pathname` 挂载页面，页面之间沿用原 markup 的 `<a href>` 整页跳转，
每次加载都会重新读 `/api/auth/me`，登录态不会过期。

| 路径 | 渲染 | 数据来源 |
| --- | --- | --- |
| `/` | `HomeView`（原首页） | `GET /api/auth/me` 决定 CTA 与「管理后台」入口 |
| `/dashboard` | 未登录 `LoginCard`（按 `mailEnabled` 决定是否给出找回密码入口）；禁用「账号已禁用」；`mustChangePassword` 整页改密；否则 `DashboardClient` | `GET /api/auth/me` + `GET /api/config` |
| `/admin` | 非登录 → 跳 `/dashboard`；非管理员「无权限访问」；否则 `AdminClient` | `GET /api/auth/me` + `/api/admin/*` |
| `/images/{id}` | 原详情页布局 + `CopyButtons` | `GET /api/images/{id}/meta`（必需）+ `HEAD /api/images/{id}` |
| `/forgot-password`、`/reset-password`、`/verify-email` | `AccountRecoveryScreen` | `/api/auth/forgot-password`、`/reset-password`、`/change-email/confirm`；`forgot` 模式另读 `GET /api/config`（`mailEnabled`） |
| 其他 | `src/pages/not-found-page.tsx` 的 404 | — |

`web.go` 用 `dist/index.html` 提供页面。已注册页面的尾斜杠地址用 HTTP 308 跳转到规范地址，
保留查询参数。未知网页地址、无效图片 ID 和当前访问者不可见的图片详情页返回 HTTP 404，
浏览器渲染带「返回首页」的 404 页面。图片详情页与 `/meta` 共用可见性判断。
页面渲染异常时显示「页面出错了」，点「重试」可重新挂载页面。

`GET /assets/{name}` 只服务 `dist/assets/` 下白名单扩展名（`.css/.js/.svg`）的单段文件，
拒绝点开头、斜杠/反斜杠、非白名单扩展名与不存在的文件。页面用 `no-store`，
资源用 `no-cache` + ETag（`If-None-Match` → 304）。`HEAD` 与 `GET` 同状态，无响应体。
未注册的 `/api/*` 继续返回 JSON 404。

### 详情页元数据（必需，不是回退）

`GET /api/images/{id}/meta` → `{ok:true,image:{id,originalName,originalMimeType,size,width,height,createdAt,tags,isVisible,url,pageUrl,thumbUrl}}`。
不存在、已软删除、管理员下架一律 404；隐藏图片只有上传者本人 Cookie 可读，其他人 404。

前端把它当作权威结果：**404 直接渲染 404 页**（不存在的图片、别人的隐藏图片、回收站图片都
不会变成详情页），其他非 2xx 或网络错误显示明确错误并提供重试；成功后仍 `HEAD` 原图，
文件不存在（404）同样按 404 页处理，其他失败显示错误。没有「meta 失败就用字节接口兜底」的逻辑，
因为字节接口对隐藏图片返回占位 SVG，会把本应 404 的页面渲染成看似成功的详情页。

### CSP

`web.go` 统一下发：

```
default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline';
img-src 'self' data: blob:; connect-src 'self'; form-action 'self';
base-uri 'none'; frame-ancestors 'none'; object-src 'none'
```

- `script-src 'self'`：产物只有一个外链 module script，没有内联脚本（构建关闭了 Vite 的
  modulePreload 内联 polyfill，所有资源都是真实文件而不是 `data:` URL）。
- `style-src` 需要 `'unsafe-inline'`：复用的 React 组件大量使用 `style` 属性。
- `img-src` 需要 `data:` 与 `blob:`：上传预览与剪贴板粘贴图；不引入外部域。
- `connect-src 'self'`：不宽放外部连接。

## Go 侧接入

1. **嵌入包**：`frontend/embed.go`（`package frontend`）用 `//go:embed dist` 导出
   `frontend.Dist`；`web.go` 从该 FS 读取，不用 `embed ../`（Go 不允许）。
2. **运行时配置**：`GET /api/config` → `{ok:true,uploadMaxBytes,mailEnabled}`
   （`Cache-Control: no-store`）。只暴露这两个非敏感值：上传上限与「邮件是否可用」布尔值
   （`mailEnabled` = `SMTP` 已配置且 `BASEURL` 非空，与邮件接口的判断完全一致）；
   不包含 SMTP 主机、端口、账号、密码或发件人。`web_test.go` 覆盖 10MiB、512KiB、
   默认 5MiB 与字段集合，未配置 SMTP / 缺少 `BASEURL` / 完整启用三种 `mailEnabled`，
   HEAD 用真实 `httptest.Server` 验证无响应体。
3. **页面/资源**：`registerWebRoutes` 注册上表全部路径；`/assets/{name}` 按嵌入 FS 服务。
4. **安全头**：CSP 见上，另含 `X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、
   `Referrer-Policy: no-referrer`、ETag、`Cache-Control`。
5. **Go 测试**：`internal/server/web_test.go` 用 `httptest` 覆盖每个页面、
   HEAD 与 GET 头一致、资源字节/MIME/ETag/304、index.html 引用资源可服务、路径穿越与非法资源名、
   `/images/{id}` id 校验、未知页面与未知 API 仍 404（API 为 JSON）。
   运行：`go test ./internal/server -run TestWeb`。
6. **发布脚本**：`scripts/build-release.sh` 在任何写入之前校验
   `frontend/dist/index.html`、`frontend/dist/assets` 以及 index.html 引用的每个 `/assets/*` 文件
   都存在，然后才编译；`scripts/test-build-release.sh` 覆盖缺失/不完整产物的拒绝分支与
   「完整 fixture 越过前置校验」的用例。生产运行机仍不需要 Node。

## 前端调用的接口与 Go 现状

全部 fetch 路径来自原组件（唯一新增是详情页必需的 `/meta` 契约）：

| 接口 | Go 现状 |
| --- | --- |
| `/api/auth/{register,login,logout,me}`、`change-password`、`forgot-password`、`reset-password`、`change-email`、`change-email/confirm` | 已实现（含激活码注册分支与邮件恢复链路） |
| `POST /api/images`（multipart `file` + `tags`）、`GET /api/images?mine=true&…`、`GET/HEAD /api/images/{id}`、`PATCH/DELETE /api/images/{id}`、`/api/images/{id}/restore`、`GET/DELETE /api/apikeys` | 已实现，参数/字段与 UI 对齐 |
| `GET /api/images/{id}/meta`、`GET /api/images/tags`、`GET /api/images/usage`、`PATCH /api/images/batch`、`POST /api/images/from-url`、`GET/HEAD /api/images/export` | 已实现（`images_extra.go`、`images_export.go`、`images_meta.go`） |
| `/api/admin/{stats,users,images,activation-codes,audit-logs}`（11 条） | 已实现（`admin_*.go`，含 14 个审计动作） |

前端没有为了「编译通过」删掉任何按钮、tab、对话框或页面。

彻底删除批量接口额外返回 `updatedIds`：只有真正删掉数据库行的 id（包括
`cleanupPending` 的条目）。前端只移除这些 id、保留失败项的选择，并在原 `image-notice` 容器里
按 `failedCount` / `cleanupPending` 显示精确提示；`cleanupPending` 表示数据库行已删除、文件字节
清理未完成，只会在下次服务启动时重试，因此提示要求管理员检查日志并重启服务，不承诺定时自动重试。

## 已知差异与未验证项

- **视觉/交互验收**：本仓库的真实 Chromium DOM/键盘交互与布局检查见
  [VALIDATION.md 的 A11 节](../VALIDATION.md#a11-真实浏览器交互与界面修正2026-11-18本仓库独立实测)。
  当前桌面截图/指针通道异常，视觉使用浏览器 PDF 渲染导出辅助复核，尚无原生截图与完整指针流程证据。
- `router.refresh()` 变成整页重新加载：登录/退出等仍靠它重新读会话；改密成功通过
  `refreshAfterPasswordChange={false}` 跳过这次 reload，保留成功提示。其余 refresh 仍多一次文档请求。
- 首页首屏等 `/api/auth/me` 返回后才绘制，避免登录用户先看到「登录 / 注册」再跳变。
- 详情页依赖 `/meta`：该接口不可用时显示明确错误，不会退化成无名称/无标签的页面。
- 彻底删除确认文案是一般性的「不可恢复」，不声称文件立即从磁盘移除；若后台物理清理未完成，
  会在操作后的 notice 里提示需要管理员检查日志并重启服务重试。部分成功后只保留失败项；
  purge 请求失败时会重新拉取列表、清空选择并关闭确认框，丢弃旧 id 快照，避免用旧 id 重试导致整批 403。
- 缩略图使用无损 WebP 编码；动画 WebP 也可上传，列表缩略图取首帧。
