# PicBox

自托管的图片库：默认单二进制部署，浏览器界面是内嵌的静态 React 包，运行机不需要
Node、Go、SQLite 服务、Docker 或图片处理库。Go 只在构建期使用，Node/pnpm 只在重建前端产物时使用。

- **保留原图**：上传原件按字节保存，缩略图单独生成。
- **用标签整理**：多标签、批量改标签、按标签筛选、可见性控制。
- **可找回、可打包**：软删除进回收站（默认保留 30 天），流式 ZIP 导出带 `manifest.json`。
- **单二进制**：HTTP 服务、HTTP 静态资源、SQLite（modernc，pure Go）与 WebP 缩略图（pure Go）都在一个进程里。

当前源码版本为 **0.0.1**（首个发布版本）。构建身份以 `picbox --version` 为准。

## 来源与许可

PicBox 的 Go 服务、原版界面与验收脚本来自 PicHome 项目的 Go 版本实现
（源提交 `9a920c9`「保存拆分 PicBox 前的项目状态」，其前一版为 `3b60b0b`），在拆分时复制到本仓库
并改名为独立产品。本次迁移只做品牌、模块路径、目录布局与配置前缀调整，未改动业务行为。

- 许可证：沿用源项目的 MIT 许可证（[`LICENSE`](./LICENSE)，版权人按原文保留）。
- 第三方组件保留各自许可与版权，完整声明见 [`THIRD_PARTY_NOTICES.md`](./THIRD_PARTY_NOTICES.md)。
  浏览器 JavaScript 自带运行时依赖的许可声明；原生发布包同时附带项目 `LICENSE` 与第三方声明。
- 界面组件与样式沿用源项目的 `app/**`、`lib/**` 纯浏览器闭包，现位于 [`frontend/app`](./frontend/app)
  与 [`frontend/lib`](./frontend/lib)；Next.js 后端、Prisma、Android 客户端与旧数据不在本仓库内。
- 源项目的验收记录与本次迁移的独立验收分开记录，见 [`VALIDATION.md`](./VALIDATION.md)。

## 快速开始（本机）

```sh
git clone https://github.com/xyz0z0/PicBox.git
cd PicBox
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/picbox ./cmd/picbox
./dist/picbox
```

不想自己构建时，可以直接下载 [Releases](https://github.com/xyz0z0/PicBox/releases) 里的
Linux amd64/arm64 压缩包（见下文「原生安装包」）。

打开 <http://127.0.0.1:3001>，选择「创建管理员」。默认只监听环回地址，先在可信环境完成首次建号。
程序在当前目录的 `data/` 中创建 `picbox.sqlite`、认证密钥 `auth.key` 与图片；默认 Cookie 名为
`picbox_token`。配置项见 [`.env.example`](./.env.example)，全部可省略。

数据目录是一个整体持久化单元：备份、恢复、迁移都按目录处理。

## 目录结构

| 路径 | 内容 |
| --- | --- |
| `cmd/picbox/` | 入口：`--version`、配置加载、HTTP 服务生命周期 |
| `internal/server/` | HTTP 路由、认证、图片、管理端、SQLite、邮件、审计 |
| `frontend/` | 静态 React 前端可重建源码 + 已提交的 `dist/`（嵌入二进制） |
| `deploy/` | systemd 模板与原生安装说明 |
| `scripts/` | 发布打包、打包守卫、HTTP 回归、Docker 资源验收 |
| `testdata/` | 资源验收原始记录（来源阶段，见 [`VALIDATION.md`](./VALIDATION.md)） |

## 界面与前端产物

界面来自源项目的原版 React 组件、markup 与样式表：首页、`/dashboard` 工作台、`/admin` 管理端、
`/images/{id}` 详情页和三个账号恢复页；工作台提供「画廊墙」（CSS 多列瀑布流）与
「接触印相表」（表格）两种视图，选择保存在浏览器 `localStorage`。它们由 Vite 构建成 `frontend/dist`，
`frontend/embed.go` 把它嵌入二进制，`internal/server/web.go` 静态提供；浏览器只与同源 `/api/*` 通信。

- Go 构建直接使用**已提交的 `dist/`**，不需要 Node；发布包与 CI 都不把 Node 当运行/编译期依赖。
- 前端源码（含 `frontend/app/**`、`frontend/lib/**`）有改动时，必须在 `frontend/` 内用 pnpm 10
  单独 `pnpm install --frozen-lockfile && pnpm verify`，并提交重新生成的 `dist/`，否则二进制会嵌入陈旧 UI。
- **运行时配置**：页面通过公开的 `GET /api/config` 读取
  `{ok:true,uploadMaxBytes,mailEnabled}`。`uploadMaxBytes` 用于上传提示，改 `UPLOAD_MAX_BYTES`
  **不需要重新构建前端**；`mailEnabled` 是「`SMTP_*` 与 `BASEURL` 都配好」的能力布尔值，
  前端据此决定是否提供找回密码/改邮箱入口（未启用时给出「联系管理员」提示）。响应里没有
  SMTP 主机、端口、账号或密码。只有前端源码变更才需要重新构建。
- 页面是静态壳，单图元数据由 `GET /api/images/{id}/meta` 提供；`/images/{id}` 的可见性以该接口为准。

细节（路由表、CSP、组件复用方式、`dist` 新鲜度检查）见 [frontend/README.md](./frontend/README.md)。

## 已实现范围

- **账号**：首个账号免激活码并成为管理员；之后注册必须使用管理员生成的激活码（角色 USER）。
  邮箱密码登录、退出、当前用户、API Key 创建/列表/撤销、管理员登录审计。
  **注册与登录不依赖邮件**：默认不配置 SMTP 也能完整建号与使用。
- **账号闭环**：修改密码、忘记密码重置、修改邮箱（含确认链接）；30 天回收站自动清理。
  找回密码与改邮箱是仅有的邮件功能，未配置 SMTP 时 UI 直接提示联系管理员。
- **图片工作台**：上传（单张 ≤5MiB、≤1600 万像素）、URL 导入、分页图库（画廊墙 / 接触印相表视图切换）、多标签与批量改标签、
  可见性、批量操作、软删除、私有回收站恢复/彻底删除、原图/缩略图分发、流式 ZIP 导出与冷却。
- **管理端（5 个 tab 全部可用）**：统计、用户管理（建号/禁用/启用/重置密码/改邮箱/配额/解除导出冷却）、
  激活码生成与撤销、图片下架与删除、审计日志（原版 14 个动作）。

### 账号与凭据语义

- `POST /api/auth/change-password` `{oldPassword,newPassword}`：成功后重签**当前会话** Cookie
  （当前浏览器保持登录）、递增 `sessionVersion` 使其他设备旧 JWT 失效，并**保留 API Key**。
- `POST /api/auth/forgot-password` / `reset-password`：重置密码会撤销该账号全部 API Key
  （凭据被怀疑泄露时的路径）；未配置 SMTP 时返回 503 `MAIL_UNAVAILABLE`，
  管理员仍可在用户管理里直接重置密码或改邮箱。前端读到 `mailEnabled:false` 时不会给出
  这两个入口，所以默认部署不会把用户带进必然 503 的流程。
- 旧 `POST /api/auth/password` 只作为历史内部入口保留（语义为「改密后撤销全部 Key 并退出登录」），
  原版 UI 不调用它；新代码请用 `change-password`。

### 数据目录与 schema

- 当前 schema 版本为 **2**，由 PicBox 服务自建（`picbox_meta` 标记）。
- 打开时拒绝外部/Prisma 生成的数据库，以及旧版 schema 1 数据库与未来版本；
  错误信息写明新旧版本与「不支持就地升级」。
- **没有就地升级**：请使用另一个**全新的空数据目录**启动。不要删除或覆盖原有数据；
  需要保留旧数据时先整体备份原目录再另存。
- 目录里的 `picbox.sqlite`、`auth.key` 与图片是一个整体持久化单元，备份/恢复都按目录处理。

### 删除与清理语义

图库删除先进入回收站，可恢复或确认永久删除；软删除仍占配额，回收站记录默认 30 天后自动清理。

永久删除先写持久化清单，再把原图与缩略图暂存到 `data/tmp/purge-*`，数据库提交后清理字节。
批量彻底删除的响应带 `updatedIds`（精确成功、可以从 UI 移除的 id）；失败项不会出现在
`updatedIds` 里，UI 不把它当作已删除。若响应 `cleanupPending:true`，表示记录与标签已删除、
逻辑配额已释放，但暂存文件仍占物理磁盘：请检查服务日志与文件权限，下次启动会重试。
启动时也会恢复中断的删除（记录还在则搬回文件，记录已删除则继续清理；只有空目录、没有清单时
只清理空目录，不动其他文件）；清单非法或恢复失败会拒绝启动并保留现场，**不要直接清空 `data/tmp/`**。

批量恢复与批量彻底删除和单张接口一样**只接受登录 Cookie**：API Key 不能把记录放回图库，
更不能物理删除字节；可见性、软删除与改标签仍允许 Bearer。

数据库里的时间统一存为 **UTC 毫秒时间戳字符串**（例如 `2026-11-17T03:44:35.000Z`，字面 `Z`，
与 `imagesTimeLayout` / `coreTimeLayout` 一致），因此 TEXT 列的字符串比较等价于时间先后。

## 配置

配置读取进程环境变量，无必填项。完整模板见 [`.env.example`](./.env.example)。
密钥首次启动随机生成并保存在 `data/auth.key`（0600），重启继续使用。

| 变量 | 默认值 | 含义 |
| --- | --- | --- |
| `PICBOX_ADDR` | `127.0.0.1:3001` | HTTP 监听地址 |
| `PICBOX_DATA_DIR` | `./data` | 数据、认证密钥及图片目录 |
| `BASEURL` | 空 | 对外访问的完整站点源地址；用于链接、写请求来源校验与邮件链接；当前只支持域名根路径部署 |
| `UPLOAD_MAX_BYTES` | `5242880` | 单个上传文件上限，字节 |
| `MAX_IMAGE_PIXELS` | `16000000` | 解码前允许的最大像素数 |
| `USER_QUOTA_BYTES` | `0` | 账号默认配额，0 为不限；管理端可按账号覆盖；软删除仍占空间 |
| `TRUSTED_PROXIES` | 空 | 可信反向代理的 IP/CIDR（逗号分隔）；空表示忽略所有转发头，只按 TCP 对端分桶 |
| `EXPORT_COOLDOWN_HOURS` | `12` | 导出**成功**后的冷却小时数；`0` 表示关闭冷却，失败/取消不消耗 |
| `TRASH_RETENTION_DAYS` | `30` | 回收站保留天数，最小 1；超期由后台任务物理删除 |
| `AUTH_COOKIE_NAME` | `picbox_token` | Cookie 名 |
| `COOKIE_SECURE` | 按 `BASEURL` 是否 HTTPS | 仅 HTTPS 发送 Cookie |
| `JWT_SECRET` | 自动生成 | 可选覆盖密钥，至少 32 字节；丢失或更换会使原 Cookie 失效 |
| `SMTP_HOST` / `SMTP_PORT` / `SMTP_FROM` | 空 / 587 / 空 | 找回密码与改邮箱的外发邮件；`SMTP_HOST`、`SMTP_FROM` 都设置才算启用 |
| `SMTP_SECURE` | `false` | `true` 为连接即 TLS（通常 465）；`false` 为 STARTTLS（通常 587） |
| `SMTP_REQUIRE_TLS` | `true` | 必须 STARTTLS 成功后才发送；只有显式设为 `false` 才接受明文投递，证书校验始终开启 |
| `SMTP_USER` / `SMTP_PASSWORD` | 空 | 两者必须同时设置；为空表示不做认证 |

**邮件是完全可选的**：默认不配置 SMTP，注册仍只需激活码（首账号免激活码），
登录、上传、导出和管理端都不受影响；只有找回密码与改邮箱不可用（接口返回
503 `MAIL_UNAVAILABLE`），此时由管理员在用户管理里直接重置密码或改邮箱。

`SMTP_*` 只配置一部分会在启动时报错，避免「以为能发信、实际一直 503」的静默部署事故。
`SMTP_*` 与 `BASEURL` 都配好后**需要重启服务**才生效，`GET /api/config` 的 `mailEnabled`
随即变为 `true`，原有邮件流程（含确认链接）即可用。

程序**不会自动读取 `.env` 文件**：配置只来自进程环境变量，必须由调用方注入
（shell `export`/前缀赋值、systemd 的 `EnvironmentFile=`、容器 `env`）。

`TRUSTED_PROXIES` 决定限流（登录、注册、改密、找回、上传）按哪个地址分桶：

- 默认空：不信任 `X-Forwarded-For` / `X-Real-IP`，一律用直连对端地址。直接启动
  （无反向代理）时这是正确行为；但反代后所有浏览器请求的对端都是环回地址，限流
  会变成**全站共享桶**，因此必须在反代部署时配置本项，否则可用性会静默退化。
- 只有列出的直连对端才能提供 XFF：程序从右往左剥离可信代理，第一个不可信地址即
  真实客户端，左侧条目由客户端自带、不能改变分桶。代理必须正确设置
  `X-Forwarded-For $proxy_add_x_forwarded_for`（追加/覆盖真实对端），服务不读
  `X-Real-IP`。
- 只列出实际代理地址；`0.0.0.0/0`、主机名、非法 IP/CIDR 会在启动前报错，
  不要用整个内网段扩大信任范围。Nginx 最小片段见 `deploy/INSTALL.md`。

例如自定义目录：

```sh
PICBOX_DATA_DIR=/path/to/new-picbox-data ./dist/picbox
```

远程访问需显式设置监听地址及真实的 `BASEURL`；浏览器写请求仅允许该源。
`.env.example` 可按需复制为部署环境文件，但不要提交真实密钥。

## 上传与格式边界

- 上传并发：服务端只有**一个上传处理槽**，满载立即 429 `UPLOAD_BUSY`，不建立无界等待队列。
  静态前端把上传并发参数固定为 **1**，UI 的排队/进度/重试逻辑保持原样。单张上限则由
  `GET /api/config` 在运行时下发，改 `UPLOAD_MAX_BYTES` 不需要重新构建前端。
- 上传频率：按客户端 IP 限制 **100 次/小时**，multipart 上传与 URL 导入共用一个桶；
  超限返回 429 `RATE_LIMITED` + `Retry-After`，且不读取请求体、不写临时文件。
  频率桶不替代上面的单处理槽背压，两者同时生效。
- 支持 JPEG、PNG、GIF 和 WebP（含动画 WebP）。SVG 拒绝。GIF 原图动画字节保留、缩略图取首帧。
- 动画 WebP 的首帧按 RFC 9649 解码并合成到 `VP8X` 画布：支持 `ANMF` 帧偏移、`ALPH` +
  有损 VP8 帧与无损 VP8L 帧；只解码首帧，原图字节原样保存，数据库宽高记录画布尺寸。
  缩略图沿用不放大、最长边 400 的限制。
- 缩略图使用 pure-Go 无损 WebP 编码，体积可能比有损编码更大。
- 导出 ZIP 包含 `README.txt`、`manifest.json` 与 `images/<id>.<ext>`；隐藏图片也导出，
  回收站与管理员下架不导出。HEAD 只做预检、**不消耗冷却**；成功后才进入
  `EXPORT_COOLDOWN_HOURS` 冷却，进行中的导出使用独立预约与租约，并发只有一个赢家。

## 原生安装包

在仓库根目录运行：

```sh
./scripts/build-release.sh --version 0.0.1-dev.1
```

输出位于 `dist/release/<version>/`，包含 Linux amd64、arm64 压缩包及 `SHA256SUMS`。
包内有二进制、systemd 模板和安装说明；安装、升级与回滚操作见 [deploy/INSTALL.md](./deploy/INSTALL.md)。
构建脚本不安装服务，也不发布。`./dist/picbox --version` 可查看构建身份；含未提交源码的构建显示 `-dirty`。

独立 [Go CI](./.github/workflows/go.yml) 验证依赖、竞态测试、静态检查、路径边界、前端产物新鲜度、
打包和健康检查。`main` 分支、PR 与手动运行将安装包保存为 Actions artifact（保留 7 天）；
推送版本 tag 后，通过同一套完整检查才自动发布 GitHub Release。ARM64 当前仅交叉编译验收。

发布前先提交并推送待发布源码、重建的 `frontend/dist` 和本工作流，确认 tag 去掉 `v` 后与
`frontend/package.json` 的 `version` 完全一致。例如当前版本为 `0.0.1`：

```sh
git tag -a v0.0.1 -m "发布 PicBox 0.0.1"
git push origin v0.0.1
```

任务成功后，到 [Releases](https://github.com/xyz0z0/PicBox/releases) 页面下载对应架构的 `.tar.gz` 与 `SHA256SUMS`；
包内包含安装说明与许可文件。`v0.2.0-rc.1` 这样的预发布 tag 会标记为 prerelease，
其前端版本也须先改为 `0.2.0-rc.1` 并重新运行 `pnpm verify`、提交产物。
其他名称的 tag 不触发自动发布；手动选择版本 tag 运行也只验证和保留 artifact。

发布 job 使用 GitHub 自动提供的 `GITHUB_TOKEN`，仅该 job 申请 `contents: write`；
不需要额外配置个人访问令牌。已有同名 Release 时任务报错，不覆盖已有资产，正常发版请使用新版本。
工作流不会自动创建 tag 或安装、部署服务。

相关行为见 GitHub 官方的 [tag 触发规则](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#onpushbranchestagsbranches-ignoretags-ignore)
与 [Release CLI](https://cli.github.com/manual/gh_release_create) 文档。

systemd 模板见 [deploy/picbox.service](./deploy/picbox.service)：以专用的 `picbox` 用户运行，
使用 `/var/lib/picbox`，可通过 `/etc/picbox.env` 设置配置。模板不安装或覆盖任何当前服务。

## Docker 资源验收（可选）

Docker 只用于可重复的资源测试，默认部署仍是二进制。先在宿主机构建目标架构的静态 Linux 二进制；
镜像只复制该产物，不在受限容器内编译。需要 Docker Engine、Compose v2 和 Python 3。

```sh
CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o dist/picbox ./cmd/picbox
docker compose -f compose.test.yml build
python3 scripts/test-resources.py --output testdata/resource-results.json
```

脚本依次创建 **1 核配额 / 256MiB、512MiB / 禁用 swap** 的容器，端口随机且只绑定环回。
每次只挂载新建的临时目录，运行用户是该目录的宿主机 UID/GID。结束后自动清理自己创建的容器和数据；
不挂载现有图片，不修改现有数据库。

负载覆盖当前界面的接口面：静态壳与 `dist/assets` 读取、未知 API 404、创建账号、API Key、
64×64 / 4000×3000 / 4000×4000 RGBA 上传、原图哈希一致、WebP 缩略图、条件 HEAD、
每批 24 次/8 并发下载、大图重复上传、4 并发上传的 200/429 背压、分页、`usage` / `tags`、
批量可见性、导出 HEAD 与小 ZIP 基础路径、可见性、软删除权限、回收站恢复/永久删除、
改密后**当前会话保留、其他旧会话失效、API Key 保留**及新密码登录。任何断言失败返回非零。
负载生成器运行在容器外。

JSON 报告记录实际镜像 ID、启动至健康耗时、各尺寸上传耗时、OOM 状态与内存。
本机 Linux cgroup v2 可读时使用 `memory.current` / `memory.peak`（含文件页缓存）；
远程 daemon 或无读取权限时这两项为 null，报告最终 Docker stats，不伪称测得精确峰值。
测试默认不设置 `GOMEMLIMIT`。这不是吞吐极限测试，也不能代替真实 512MB VPS 的系统开销和磁盘验收。
本仓库**未重新运行**资源测量；[`VALIDATION.md`](./VALIDATION.md) 明确标注了哪些数字来自源项目历史记录。

只想临时开网页，可运行：

```sh
docker compose -f compose.test.yml up -d
```

此 Compose 使用 256MiB、禁用 swap、64MiB 的临时内存盘，**容器重建后数据会丢失**，
不用于持久部署或正式内存报告。结束后只清理这个 Compose 项目：

```sh
docker compose -f compose.test.yml down
```

## HTTP 回归脚本

```sh
node scripts/test-web-v2.cjs                                # 临时构建并启动隔离服务
PICBOX_BIN=/path/to/picbox node scripts/test-web-v2.cjs     # 用已有产物（如解包后的发布包）
```

脚本自己创建临时数据目录与环回端口，结束后回收临时服务与数据。它验证静态壳、`/assets/*`
实际可取、CSP/安全头、公开的 `GET /api/config`、未知 API 的 JSON 404，以及原 UI 实际调用的
API 闭环：管理员生成激活码 → 用户凭码注册 → 改密（当前会话/旧会话/API Key 语义）→
批量与 `usage`/`tags` → 单图 `meta` 可见性 → 导出 HEAD + 真实 ZIP（解析中央目录与
`manifest.json`）→ 批量彻底删除的 `updatedIds` 精确性 → 配额/禁用/启用与管理审计。

**边界**：该脚本只做 HTTP 与静态资源断言，**不执行浏览器 JavaScript**，也不代表弹窗、导航、
窄屏或暗色主题已经验收；UI 交互必须有真实浏览器证据。

## 验证命令

```sh
cd frontend && npx -y pnpm@10 install --frozen-lockfile && npx -y pnpm@10 verify && cd ..
go mod verify
go vet ./...
go test -race ./... -timeout 450s
./scripts/test-build-release.sh
./scripts/build-release.sh --version 0.0.1-dev.1
node scripts/test-web-v2.cjs
```

前端 `pnpm verify` = `tsc --noEmit` + 30 条 jsdom 行为测试 + `vite build` + `check-dist` 产物断言。
实际命令与结果见 [`VALIDATION.md`](./VALIDATION.md)。内部接口约定见 [MIGRATION.md](./MIGRATION.md)。

## 安全

安全问题的报告方式、支持版本与处理流程见 [`SECURITY.md`](./SECURITY.md)。请通过 GitHub 的
私有漏洞报告通道提交，不要在公开 Issue 中贴出可利用细节或真实数据。

## 未验证 / 已知限制

- 真实 512MB VPS、生产部署、反向代理、持续高负载与断电持久性未验收；容器资源数字只覆盖限额本身。
- 未与真实证书的 SMTP 端点做 TLS 握手；ARM64 只交叉编译 + `file` 校验，未在实机运行。
- 真实浏览器验收见 [`VALIDATION.md`](./VALIDATION.md) A11（登录/注册、上传、详情、删除与恢复、
  导出下载事件、390px 与暗色布局）；之后的画廊墙/接触印相表视图重构只做了 `pnpm verify` 与
  HTTP/静态壳回归，没有重新在真实浏览器验收。原生截图、完整指针流程与完整键盘可访问性仍未覆盖。
- 缩略图仍为无损 WebP。
- 反代限流默认按直连对端分桶；需配置 `TRUSTED_PROXIES` 才会按 `X-Forwarded-For`
  里的真实客户端分桶（`X-Real-IP` 不参与）。`IMAGE_ACCEL_REDIRECT_PREFIX` 一类反代直出未实现。
- 远端仓库为 <https://github.com/xyz0z0/PicBox>；首版 `v0.0.1` 已于 2026-11-19 发布，
  安装包见 [Releases](https://github.com/xyz0z0/PicBox/releases/tag/v0.0.1)。未做生产部署。
