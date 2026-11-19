# 验收记录

本文件区分两类证据，**引用数字时必须连类别与日期一起引用**：

- **A. PicBox 独立验收**：在本仓库根目录实际执行的命令与结果。
- **B. 来源历史证据**：从 PicHome 的 Go 版本（源提交 `9a920c9`，其前一版 `3b60b0b`）带入的记录。
  这些数字只对当时的源构建成立，**不是**本仓库产物的实测结果。
- **C. 未验证项**：任何一方都没有覆盖的部分，不得用「应该没问题」替代。

运行环境（A1–A8）：Linux amd64、Go 1.26.8、Node 24 + pnpm 10.34.9（`npx -y pnpm@10`）、
Vite 7.3.8、TypeScript 7.0.2。**迁移当次尚未 commit**，因此 A1–A7 构建身份是 `commit=unknown`，
产物为非正式发布（`commit=unknown` 不代表当前 `HEAD`）；后续增量的环境与构建身份各节另列。

## A. PicBox 独立验收（2026-11-17，迁移当次）

### A1. 前端源码与内嵌产物

```sh
cd frontend
npx -y pnpm@10 install --frozen-lockfile
npx -y pnpm@10 verify
npx -y pnpm@10 build   # 再跑一次用于比对哈希
```

| 项目 | 结果 |
| --- | --- |
| `install --frozen-lockfile` | 通过（587ms；只提示忽略 `esbuild@0.28.2` 安装脚本，与来源阶段一致；无依赖升级） |
| `verify` | 通过：`tsc --noEmit` 无错误；**9 条 jsdom 测试全部通过**；62 modules；`check-dist: ok` |
| 重建一致性 | `vite build` 连跑两次，`dist/` 四个文件 sha256 逐字节一致 |

产物（412K）：

| 文件 | sha256 |
| --- | --- |
| `dist/index.html` | `11a15635eddb7ce5695526f76185256a958300c343a3bafb060c1cdd8ec3f924` |
| `dist/assets/index-DqarhU-G.js` | `c1620b69b801963b3b4c34cb2eaf9f09486776b0d653d29682596713a3accd51` |
| `dist/assets/index-C7F7202u.css` | `0c4951182feb8329200b8db6fe588ccceda75bdf1e3fb49f2c2b4e18b4f8dd4e` |
| `dist/assets/favicon-B_xZ1S0S.svg` | `435f80ba178583ed32637c9765121f65259a1ad6f4320969dde697294ef9561a` |

迁移相关差异：`vite.config.ts` 的 `@/` 别名与 `readVersion()` 改为指向本目录（不再读取父目录），
`tsconfig.json` 的 `@/*` 映射为 `./*`；`app/**`、`lib/**` 复制进本目录后 `frontend/dist` 由
本目录源码重建。产物中产品名为 PicBox（`dist/index.html` 的 `<title>` 与页面品牌文字），
哈希因此与来源阶段不同，不能复用来源记录里的哈希。

### A2. Go 全量

```sh
go mod verify
go vet ./...
go test -race ./... -timeout 450s
CGO_ENABLED=0 go build -trimpath -o /tmp/picbox-standalone/picbox ./cmd/picbox
```

| 命令 | 结果 |
| --- | --- |
| `go mod verify` | `all modules verified` |
| `go vet ./...` | 通过，无输出 |
| `go test -race ./... -timeout 450s` | 通过：`picbox/cmd/picbox` 1.011s，`picbox/internal/server` 188.252s，实际耗时 3m13s |
| `go build`（CGO 关闭） | 通过，`file` 显示 `ELF 64-bit LSB executable, x86-64, statically linked` |

`/tmp/picbox-standalone/picbox --version`（19,088,696 字节，sha256
`08eaa7ff059f50562e9fba24073bfeae1169fa8e946c013ed2a5da75b553e0b9`）：

```text
picbox version=dev
picbox commit=unknown
picbox buildTime=unknown
picbox go=go1.26.8
picbox platform=linux/amd64
picbox note=本地开发构建：未通过 -ldflags 注入 version/commit/buildTime
```

产品名、数据文件名（`picbox.sqlite`）、Cookie 名（`picbox_token`）与配置前缀（`PICBOX_`）均已是
PicBox；`dev`/`note` 是直接 `go build` 的既有语义，发布构建由 `-ldflags` 注入版本。

### A3. 打包守卫与本地发布包

```sh
bash -n scripts/build-release.sh
bash -n scripts/test-build-release.sh
./scripts/test-build-release.sh
./scripts/build-release.sh --version 0.1.0-dev.1
```

- `test-build-release.sh`：`PASS: 21 个用例`（out-dir 越界与符号链接、指向 `data/`、
  `release/`/版本目录祖先符号链接、非法版本号、未知参数、前端产物缺失/不完整）。
- `build-release.sh --version 0.1.0-dev.1`：成功生成 Linux amd64/arm64 静态 ELF 与 `SHA256SUMS`；
  `sha256sum -c` 两项 `OK`；本机 amd64 产物 `--version` 输出版本 `0.1.0-dev.1`。
  构建身份 `commit=unknown`（验证当时尚未 commit），不是发布产物。

| 产物（`dist/release/0.1.0-dev.1/`，Git 忽略） | 字节 | sha256 |
| --- | --- | --- |
| `picbox_0.1.0-dev.1_linux_amd64.tar.gz` | 5,543,034 | `46e24b0d77338500cda22a7df8717b7af8283ebe32b28ea506ee3c9699958881` |
| `picbox_0.1.0-dev.1_linux_arm64.tar.gz` | 5,060,841 | `6b9d8673be8eceff66d425355cc5f2d82452e4307afe6c3b1ec3e9ca6eaf1f9a` |

包内文件为 `picbox`、`picbox.service`、`INSTALL.md`。ARM64 只在本机交叉编译并做 `file` 校验，
**未在 ARM64 实机运行**。

### A4. HTTP 回归（解包后的 amd64 发布二进制）

```sh
PICBOX_BIN=/tmp/picbox-smoke/extract/picbox_0.1.0-dev.1_linux_amd64/picbox node scripts/test-web-v2.cjs
```

结果：**177 条断言全部通过，0 条失败，退出码 0**。覆盖静态壳与 `/assets/*`（MIME、`no-store`/
`no-cache`、ETag、304）、CSP、`GET /api/config` 运行时上传上限、未知 API 的 JSON 404、
管理员生成激活码 → 用户凭码注册 → `change-password`（当前会话保持、其他旧会话 401、API Key 保留）、
批量操作与 `usage`/`tags`、单图 `meta` 可见性、导出 HEAD + 真实 ZIP（中央目录与 `manifest.json`）、
导出冷却 429、批量彻底删除的 `updatedIds`、配额/禁用/启用与五类审计过滤。

迁移引起的断言调整只有产品字符串与路径：导出文件名正则改为 `picbox-images-all-*.zip`，
环境变量改为 `PICBOX_BIN`，临时目录前缀改为 `picbox-`；**没有删除任何断言**。
该脚本不执行浏览器 JavaScript。

### A5. 隔离启动与静态检查（未替换任何现有服务）

用 A3 的 amd64 发布包解到 `/tmp`，全新数据目录、环回端口 `38612`：

| 检查 | 结果 |
| --- | --- |
| `GET /api/health` | `{"ok":true}` |
| `GET /` 的 `<title>` | `<title>PicBox</title>` |
| 页面引用的资源 | `/assets/favicon-B_xZ1S0S.svg`、`/assets/index-DqarhU-G.js`、`/assets/index-C7F7202u.css`，与 `dist` 一致 |
| `GET /api/config` | `{"ok":true,"uploadMaxBytes":5242880}` |
| `/dashboard`、`/admin`、`/images/abc` | 200（静态壳） |
| 数据目录 | 自建 `picbox.sqlite`、`auth.key`、`tmp/` |

结束后进程已停止、临时目录已删除。**未启动浏览器**：本次迁移不做浏览器交互验收，
也不改截图口径；界面交互证据只有来源历史（见 B 类）。

### A6. 源仓库与保留服务

- 源仓库 PicHome：`git status --porcelain` 输出 0 行，`HEAD` 仍为 `9a920c9`（迁移只读，未改动）。
- 保留的 PicHome 预览 `http://127.0.0.1:38417/` 仍返回 200，本任务未停止、未替换、未改端口。

### A7. 仓库卫生检查

| 检查 | 结果 |
| --- | --- |
| `bash -n`（发布脚本与守卫脚本） | 通过 |
| `git diff --check` | 无输出（无空白错误） |
| Markdown 相对链接 | 全部新增/修改链接目标存在（含 `LICENSE`、`frontend/README.md`、`deploy/picbox.service`、`.github/workflows/go.yml`） |
| `rg` 扫描运行/构建配置 | 无用户主目录等绝对源路径、无旧 `server-go/` 层级、无旧 `pichome` 品牌；PicHome 仅出现在来源说明与 `LICENSE` 版权行 |
| `git add --dry-run` 范围 | 只包含源码/文档/前端 `dist`/守卫脚本；`data/`、`dist/`、`node_modules/`、`.env`、`*.sqlite` 均被忽略 |

### A8. 增量验收：默认激活码 + 可选邮件（2026-11-17，同一天的增量）

需求是「默认激活码注册，部署者可自行开启邮件服务」。A1–A7 的数字是迁移当次的记录，
**未改写**；下面只记录这次增量改动实际跑出的结果。

改动要点：`GET /api/config` 增加非敏感布尔值 `mailEnabled`（= `SMTP.Configured() && BASEURL != ""`，
与邮件接口同一判断）；前端据此在未启用时把「忘记密码」「修改邮箱」换成「联系管理员」提示/禁用。
注册仍只走激活码，SMTP 不改变注册要求，也没有新增开关。

```sh
cd frontend && npx -y pnpm@10 verify
go vet ./...
go test -race ./... -timeout 450s
node scripts/test-web-v2.cjs
git diff --check
```

| 项目 | 结果 |
| --- | --- |
| `pnpm verify` | 通过：`tsc --noEmit` 无错误；**16 条 jsdom 测试全部通过**（新增 7 条邮件能力用例）；62 modules；`check-dist: ok` |
| `go vet ./...` | 通过，无输出 |
| `go test -race ./... -timeout 450s` | 通过：`picbox/cmd/picbox` 1.013s，`picbox/internal/server` 183.548s |
| `node scripts/test-web-v2.cjs` | **180 条断言全部通过**（迁移当次 177 条，本次新增 3 条） |
| `git diff --check` | 无输出 |

新增的断言与覆盖：

- Go `TestWebConfigReportsRuntimeUploadLimit`：字段集合由 2 个改为 3 个（`ok`、`uploadMaxBytes`、
  `mailEnabled`），并断言未配置 SMTP 时 `mailEnabled` 是布尔 `false`；
- Go `TestWebConfigReportsMailCapability`：未配置 SMTP / SMTP 完整但缺 `BASEURL` / 两者齐全
  三种情况的 `mailEnabled` 分别为 `false`、`false`、`true`，且响应里没有任何 SMTP 字段名或
  主机、账号、密码、发件人取值；
- HTTP 回归：`config.mailEnabled` 在未配置 SMTP 的临时服务上为 `false`、
  `/api/config` 的键集合严格为 `mailEnabled,ok,uploadMaxBytes`、打包产物里出现 `mailEnabled`；
- jsdom：`mailEnabled:false` 时登录页与 `/forgot-password` 显示「联系管理员」且不渲染提交表单、
  账号设置的「修改邮箱」禁用；`mailEnabled:true` 保持原入口；配置读取失败时保留原入口
  （不凭一次失败读取断言邮件不可用）；原注册/上传/批量/导出/改密断言全部保留。

`frontend/dist` 重建后的产物（本仓库实测哈希）：

| 文件 | sha256 |
| --- | --- |
| `dist/index.html` | `c6371854040231dff156965842d02109675a8a6ff248d1d933f858e291fb71ce` |
| `dist/assets/index-8HbvPnJr.js` | `4289bda871e2a9c802b925d3f223db986834ecee72a085c8196cb7b142c408bb` |
| `dist/assets/index-C7F7202u.css` | `0c4951182feb8329200b8db6fe588ccceda75bdf1e3fb49f2c2b4e18b4f8dd4e`（未变） |
| `dist/assets/favicon-B_xZ1S0S.svg` | `435f80ba178583ed32637c9765121f65259a1ad6f4320969dde697294ef9561a`（未变） |

本次**未**执行：真实 SMTP 投递（仍属 C 类未验证）、浏览器交互验收、发布打包与 Docker 资源测试。
未 commit / push / tag / 部署。

### A9. 开源前许可补充与提交验收（2026-11-17，本仓库独立实测）

本节是 A8 之后的验收，环境为 Linux amd64、Go 1.26.8、Node 24、已缓存的 pnpm 10.31.0。
新增根目录 `THIRD_PARTY_NOTICES.md` 与 `frontend/THIRD_PARTY_LICENSES.txt`，保留项目 MIT，
分别补齐 Go 目标依赖、标准库、SQLite/musl 等传递声明与前端运行时依赖的版权和许可正文。
更新依赖时需重新核对声明。前端构建显式保留许可注释，并由 `check-dist` 检查完整声明。

```sh
cd frontend && pnpm verify && pnpm build
cd ..
go mod verify
go vet ./...
go test -race ./... -timeout 450s
bash -n scripts/build-release.sh scripts/test-build-release.sh
./scripts/test-build-release.sh
./scripts/build-release.sh --version 0.1.0-dev.license.2
node scripts/test-web-v2.cjs
git diff --check
```

Go 命令使用 `/tmp` 下可写构建缓存；需要 `httptest` 或隔离 HTTP 服务监听本机临时端口的命令，
在允许该操作的环境下完成。首次沙箱运行的端口权限失败不计为通过。

| 项目 | 本仓库结果 |
| --- | --- |
| 前端 `pnpm verify` | TypeScript、16 条 jsdom 测试、Vite 构建及许可产物守卫全部通过 |
| 重建一致性 | 再次 `pnpm build` 后，四个 `dist` 文件 SHA256 均与前次相同 |
| 许可反例 | 在临时 fixture 中删除 JS 许可声明，`check-dist` 按预期拒绝；未改仓库产物 |
| Go 校验 | `go mod verify`、`go vet ./...` 通过；完整竞态测试通过（`cmd/picbox` 1.011s，`internal/server` 186.523s） |
| 打包守卫 | shell 语法检查通过；25 项用例通过（新增两份许可文件各自缺失、为空时无写入拒绝的四项） |
| 双架构打包 | Linux amd64/arm64 静态包及 `SHA256SUMS` 自检通过；逐包确认五个文件齐全，许可文件与仓库原文逐字节一致，二进制包含当前完整 JS 产物 |
| HTTP 回归 | 180 条断言全部通过；仍只代表 HTTP 契约与静态壳 |
| 文档与复核 | 新增/修改文档的相对文件链接目标存在；许可正文与原件定向复核通过；`git diff --check` 无输出 |

新产物相对 A8 的变化：`dist/index.html` 的 SHA256 为
`e7cc745cbe346da2bc27acc641f2e1bb3d75602414876344ebbd3407c2ab8447`，
`dist/assets/index-CEa-hwcB.js` 为
`2447703d3a6130b61074a7768d2a46ff8d1ede4e568d3191a8b358575bd519af`；CSS 与 favicon 未变。

本节打包发生在提交前，身份为 `commit=caaa27a-dirty`，包位于 Git 忽略的
`dist/release/0.1.0-dev.license.2/`，**不是正式发布产物**。没有运行真实浏览器交互、
真实 SMTP、ARM64 实机、Docker 资源或生产部署验收，也没有执行 GitHub CI、push、tag、发布或部署。
下方来源历史记录及 C 类未验证边界继续有效。

### A10. GitHub 版本 tag 自动打包发布（2026-11-17，本仓库本地验收）

本次仅修改 `.github/workflows/go.yml`、README 与本验收记录，复用现有构建与验证链。
推送 `v*` tag 时检查版本格式及其与 `frontend/package.json` 的一致性，通过既有完整验证后，
独立发布 job 才使用 `contents: write` 创建 GitHub Release；分支、PR、手动运行不发布。
上传步骤的 artifact ID 通过 job output 交给下载步骤，避免仅重跑发布 job 时找不到上一轮产物。

本地环境为 Linux amd64、Node 24、Python 3 + PyYAML 6.0.3；使用 actionlint 1.7.12。

| 项目 | 本仓库本地结果 |
| --- | --- |
| `actionlint .github/workflows/go.yml` | 通过，无错误 |
| workflow shell 语法 | 从 YAML 提取全部 `run` 脚本，逐项 `bash -n` 通过 |
| tag 校验 | 实际执行提取的校验脚本：稳定版、预发布、build metadata 版本通过；不匹配、非 `v`、残缺、空值、路径、引号、命令替换及超长输入被拒绝 |
| 发布 gate 与 artifact 连接 | 本地模拟分支、PR、手动选择 tag、push tag 四类事件；静态核对发布条件及 artifact ID 输出/输入引用 |
| 发布脚本 | 临时目录中准备资产和真实 SHA256 清单，以本地 `gh` stub 执行原脚本；稳定版、预发布及 build metadata 分类正确，已有 Release、缺少/额外资产、校验和错误时不调用创建操作 |
| 场景检查总计 | 上述本地 harness 共 49 项检查通过，退出码 0；不代表 GitHub runner 实测 |
| 打包守卫 | 两份打包脚本的 `bash -n` 通过，`./scripts/test-build-release.sh` 25 项用例通过 |
| 文档与复核 | 修改后的相对文件链接目标存在，`git diff --check` 通过，独立定向复核无阻塞问题 |

本次没有修改 Go、前端或打包脚本，未重复运行 A9 的前端、Go 全量竞态和真实双架构构建。
**未执行 GitHub Actions、artifact 服务传递或真实 Release 创建**；未 push、创建 tag、发布或部署。
实际 GitHub 端结果需在本工作流提交并推送后确认，不能将本地 stub 结果作为线上发布成功的证据。

### A11. 真实浏览器交互与界面修正（2026-11-18，本仓库独立实测）

本次在 Linux amd64、Node 24、pnpm 10 环境，使用 Orca 1.4.261 内置的真实 Chromium。
服务由当前工作树构建的 Go 二进制提供，独立环回端口与临时数据库，只使用合成账号和图片；
SMTP 环境全部清空，没有发送邮件，也没有接触原有服务或数据。初测为 `2c8cffd` 的前端，
下述修复后重新构建并切换同一隔离实例复验；最终构建仍是未提交工作树的开发产物，非正式发布包。

**方法与限制：** 浏览器实际执行 React、请求真实 Go API；通过页面 DOM click、表单填充、
文件选择及部分键盘事件操作。浏览器截图多次返回 `Screenshot timed out — the browser page did not draw a frame`，
部分坐标点击与键盘激活也未生效，需以 DOM 事件复核，不能据此宣称原生指针流程或完整键盘可访问性通过。
视觉辅助检查使用同一浏览器的 PDF 渲染导出转 PNG：临时复制当前屏幕匹配的 CSS 规则、隔离打印断点、
设置与视口一致的纸张后查看图片；布局数值在未注入辅助样式的页面上另行测量。
这种导出不是原生屏幕截图，不能替代桌面指针与截图验收。临时辅助样式没有进入源码或构建产物。

| 本仓库实际操作 | 结果与边界 |
| --- | --- |
| 首页 → 登录/注册 → 首账号注册 | 不填激活码即可注册，进入管理员工作台；后续账号注册本轮未测 |
| 多图上传与标签 | 浏览器选择两张合成 PNG（均为 640×360，分别含中文与 ASCII 文件名），添加中文标签，两张均显示上传成功并出现在列表 |
| 图片预览、详情 | 预览弹窗打开，Esc 可关闭；详情页显示文件名、尺寸、标签，真实图片已解码（naturalWidth=640、naturalHeight=360） |
| 搜索无结果 | 显示无匹配状态，清除筛选后两张图片重新显示 |
| 删除与恢复 | 一张图片经确认移入回收站，显示成功提示；恢复后回收站为空，工作台重新显示两张图片；未执行彻底删除 |
| 导出 | 校验通过后浏览器报告 ZIP 下载事件，页面提示已交给浏览器处理；未据此确认磁盘下载完成，ZIP 内容另由 HTTP 回归验证 |
| 账号设置、管理页 | 未启用邮件时修改邮箱禁用并显示原因；管理页总览与激活码展示正常，390px 下表格在容器内横向滚动，页面自身无横向溢出；未生成激活码、未改密 |
| 错误密码与真实连接失败 | 错误密码显示明确提示；停止本次隔离服务后提交登录，显示「网络异常，请检查网络连接后重试。」，提交按钮和模式切换恢复可用；服务恢复后在同一表单重试成功，重新进入工作台 |

发现并修复：注册页说明与首账号免激活码规则矛盾；未启用邮件的长提示挤在密码标签同行；
登录网络失败没有可见反馈；提交期间可切换登录/注册；静态 `#root` 未衔接弹性布局导致页脚悬在半屏；
暗色登录渐变被更晚定义的浅色规则覆盖。修复没有改变注册、登录、上传等接口契约。

最终布局复验（本仓库真实浏览器测量）：

| 视口与页面 | 结果 |
| --- | --- |
| 1440×1000，浅色登录 | 内容区高 943px，卡片在内容区垂直居中，页脚 top=943 / bottom=1000；修复前页脚 bottom=612；无横向溢出 |
| 390×844，暗色登录 | 页脚 top=787 / bottom=844，卡片居中，提示在密码框下方；渐变使用暗色 RGBA 值，无横向溢出 |
| 375×812，注册 | 说明一致，提交按钮在视口内，页脚 bottom=812，无横向溢出 |
| 320×568，注册 | 文档可自然滚动（高 637px），提交按钮在页脚前，没有固定高度裁切或横向溢出 |
| 390×844，首页长页 | 内容高约 2685px，页脚位于内容末尾，自然滚动，无横向溢出 |

最终验证命令与结果：

```sh
cd frontend && npx -y pnpm@10 verify
npx -y pnpm@10 exec vite build  # 二次构建后比对 dist 的 SHA256
cd ..
CGO_ENABLED=0 go build -trimpath -o /tmp/<隔离目录>/picbox.final ./cmd/picbox
PICBOX_BIN=/tmp/<隔离目录>/picbox.final node scripts/test-web-v2.cjs
git diff --check
```

`verify` 的 TypeScript、**22 条 jsdom 测试**、Vite 构建与 `check-dist` 全部通过，二次构建逐字节一致。
最终二进制的 HTTP 回归 **180 条断言通过、0 条失败**；隔离服务首页字节与当前 `dist/index.html` 一致。
新增六条行为测试覆盖说明、提示位置与辅助关联、启用邮件入口、网络失败重试、服务端错误重试、
提交期间禁用模式切换与失败后恢复。没有删除原断言。

最终前端四个文件的 SHA256：

| 文件 | SHA256 |
| --- | --- |
| `dist/index.html` | `412e97d039ca3779b73a12fb2acc13f4975e5933b989ca1b4b62ad6183f70344` |
| `dist/assets/index-CbVNjgDz.js` | `34c11d62e95929c81a1d1c0ce781ad231f96eab31e8d26e56c869595a9fbad9a` |
| `dist/assets/index-C-Ek-hUL.css` | `ae3948df596c80c41ba188ad7ebf86ea686dbeef67975f6480bc0030b8c6d9bd` |
| `dist/assets/favicon-B_xZ1S0S.svg` | `435f80ba178583ed32637c9765121f65259a1ad6f4320969dde697294ef9561a` |

本轮没有修改 Go 或发布脚本，没有重复 Go 竞态全量、双架构发布打包、SMTP、Docker 或生产验收；
没有 commit、push、tag、发布或部署。临时服务、合成数据、专用浏览器页与本任务创建的 worker 已清理。
桌面原生截图/指针通道、完整浏览器矩阵与其余 C 类边界仍待验证。

### A12. 开源前最终检查（2026-11-19，本仓库独立实测）

环境：Linux amd64、Go 1.26.8、Node 24.16.0、pnpm 10.34.9；基线提交
`1dab5425c21e8618d4bd4698ff973fa056418bc9`，检查前工作树干净。

```sh
cd frontend && npx -y pnpm@10 verify && cd ..
go vet ./...
go test -race ./... -timeout 450s
./scripts/test-build-release.sh
node scripts/test-web-v2.cjs
```

| 检查 | 结果 |
| --- | --- |
| `pnpm verify` | 通过：`tsc --noEmit` 无错误；**30 条 jsdom 测试**（`dashboard-behavior` 22 + `ui-fallbacks` 8，2 个文件）；64 modules；`check-dist: ok` |
| 产物新鲜度 | `verify` 重建后 `git status --porcelain --untracked-files=all` 与 `git diff --exit-code -- frontend/dist` 均为空，提交的 `dist/` 与当前源码逐字节一致 |
| `go vet ./...` | 通过，无输出 |
| `go test -race ./... -timeout 450s` | 通过：`picbox/cmd/picbox` 1.012s，`picbox/internal/server` 217.155s |
| `./scripts/test-build-release.sh` | 25 个用例 PASS（路径边界、版本号与前端产物前置校验） |
| `node scripts/test-web-v2.cjs` | **196 条断言全部通过、0 条失败**；仍只覆盖 HTTP 契约与静态壳 |
| 文档链接 | 所有已跟踪 `*.md` 的相对 `./` 链接目标都存在；`git diff --check` 无空白错误 |
| 密钥与数据扫描 | 跟踪文件内无本地绝对路径、私钥或真实凭据；`data/`、`dist/release/` 均被忽略，未进入任何可达提交 |

当前 `frontend/dist` 的 SHA256（与提交内容一致）：

| 文件 | SHA256 |
| --- | --- |
| `dist/index.html` | `42c6637ae8f4d34a010fac00dc08eb024329f34d8e855b5e87ccf76858dd14fd` |
| `dist/assets/index-Bdfv7YMZ.js` | `1b1b8a33d78e0ebd20b758ea3482d6ca296e188dcf5d02932c39e59c33dc9a03` |
| `dist/assets/index-BxdMwcV-.css` | `2cd3ff82e03be7427462ff4aff9cf2acd36d1b21177707910c8000305dc9d54d` |
| `dist/assets/favicon-B_xZ1S0S.svg` | `435f80ba178583ed32637c9765121f65259a1ad6f4320969dde697294ef9561a` |

**未覆盖：** 本次 UI 重构（画廊墙 / 接触印相表视图、圆角与按钮样式）只有 jsdom 与 HTTP/静态壳证据，
**没有**真实浏览器交互记录；A11 的浏览器证据对应 `2c8cffd` 的前端。没有运行 GitHub Actions、
没有 push、tag、发布或部署（远端仓库已配置为 `github.com/xyz0z0/PicBox`），也没有重跑
Docker 资源测量、真实 SMTP、ARM64 实机或生产部署。

### A13. 首版版本号与发布准备（2026-11-19，本仓库独立实测）

首版发布 tag 定为 `v0.0.1`：把 `frontend/package.json` 的 `version` 改为 `0.0.1`，同步
README/AGENTS 的版本号与示例命令；页脚版本在构建期从 `package.json` 注入，所以同时重建并提交
`frontend/dist`（JS 哈希文件名随之变化）。

```sh
cd frontend && npx -y pnpm@10 verify && cd ..
```

| 检查 | 结果 |
| --- | --- |
| `pnpm verify` | 通过：`tsc --noEmit`；30 条 jsdom 测试；64 modules；`check-dist: ok` |
| 页脚版本 | 重建产物内为 `0.0.1`，与 `frontend/package.json` 一致 |
| 产物一致性 | 重建后提交；`git status --porcelain --untracked-files=all -- frontend/dist` 为空 |
| tag 与版本号 | `v0.0.1` 去掉 `v` 后与 `package.json` 的 `0.0.1` 一致（真实校验在 CI 的 tag 步骤） |

重建后的 `frontend/dist` SHA256（取代 A12 的同名文件）：

| 文件 | SHA256 |
| --- | --- |
| `dist/index.html` | `6e39c8bf06a67fe761b428237da56e3d575d3db9ded713f2d3f4b66dd4b3c4d2` |
| `dist/assets/index-Bs47hdl7.js` | `0c53d04f9cba1313afb86b960f6840c5b3ef6cc86a98540c735b850b4c0d42a8` |
| `dist/assets/index-BxdMwcV-.css` | `2cd3ff82e03be7427462ff4aff9cf2acd36d1b21177707910c8000305dc9d54d` |
| `dist/assets/favicon-B_xZ1S0S.svg` | `435f80ba178583ed32637c9765121f65259a1ad6f4320969dde697294ef9561a` |

**未覆盖：** 本轮未改 Go 代码，未重跑 Go 测试、打包脚本或 HTTP 回归；发布结果以 Actions 与
Releases 页面为准，本文件不预先记录线上结果。

**发布结果（同日补记，已核对成功）：** `main` 推送（run 44451631725）与 `v0.0.1` tag
（run 44451633633）的 Go CI 均成功；`publish-release` 创建了
[`v0.0.1` Release](https://github.com/xyz0z0/PicBox/releases/tag/v0.0.1)，资产为
`picbox_0.0.1_linux_amd64.tar.gz`（5,572,009 字节）、`picbox_0.0.1_linux_arm64.tar.gz`
（5,087,231 字节）与 `SHA256SUMS`（196 字节）。未在真实 VPS 安装验收这些包。

## B. 来源历史证据（PicHome `9a920c9`，**不是本仓库实测**）

以下是 PicHome 在拆分前最后一次记录的结果。它们对源构建成立，本仓库未重新测量；
其中 `testdata/*.json` 是当时资源测试的原始记录，原样带入作为可回查证据。

- **前端产物**：当时 `frontend` 的 `pnpm verify` 通过（7 条 jsdom 测试，之后补到 9 条），
  `dist` 哈希与本仓库不同（品牌与目录变化）。
- **Go 全量**：`go test -race ./...` 通过（`internal/server` 196s / 188s 两轮），
  `go vet`、`go mod verify` 通过；打包守卫 21 个用例通过。
- **发布包**：`0.1.0-dev.3` 的 amd64/arm64 包分别 5,543,005 / 5,059,703 字节，
  `SHA256SUMS` 自检通过；ARM64 仅交叉编译。
- **HTTP 回归**：同一脚本 177 条断言通过（当时导出文件名为 `pichome-images-*`）。
- **Docker 资源测量**（256MiB / 512MiB、1 CPU、no swap、无 `GOMEMLIMIT`）：
  当时的镜像 `pichome-go:resource-test`（manifest list 摘要
  `sha256:a7c12eec789c997f103b02b7db162f65e047a5af1ec7337d98182e08cd24cb12`），
  启动至健康 321ms / 293ms，cgroup `memory.peak` 130.69MiB / 130.02MiB，
  4000×3000 上传 443ms / 468ms，4 并发上传 `1×200 + 3×429`，无 OOM。
  原始记录见 [`testdata/resource-results-ui-go-2026-11-17.json`](./testdata/resource-results-ui-go-2026-11-17.json)。
  **本仓库未运行 Docker 资源测试**，镜像名现已改为 `picbox:resource-test`，旧镜像 ID 不能代表新产物。
- **浏览器交互（源项目阶段）**：当时在隔离预览（`127.0.0.1:38417`）完成桌面/375px、明暗主题、
  首页/登录注册/工作台/管理端 5 tab/详情页、上传与账号弹窗、搜索/标签/可见性/软删除/恢复/彻底删除、
  改密与重登、激活码与审计的页面内 DOM 交互，且无 CSP violation。
  **方法限制**：当时 Orca 桌面窗口处于隐藏/锁屏会话，`orca screenshot` / 指针注入不可用；
  截图由同一 URL 的无头 Chrome 渲染、点击为页面内 DOM click。**Orca 桌面指针点击与自带截图从未取得证据**，
  需在可见桌面会话补一次。该记录属于来源阶段，不构成本仓库的界面验收。

更早两轮「简化 vanilla 预览页」的资源记录（镜像 `sha256:d3fdc4e...`、`sha256:fa828f65...`）对应
已删除的界面版本，拆分时**未带入**本仓库，仍留在 PicHome 源仓库；它们不能用来描述当前 UI 版本。

## C. 仍未被覆盖的部分

- 本仓库已完成 A11 所列真实浏览器 DOM/部分键盘交互、375px/390px 与暗色布局检查；
  **原生截图、完整指针流程与完整键盘可访问性**仍无验收证据。激活码生成、后续账号注册、改密等本轮未执行。
- 画廊墙 / 接触印相表视图与后续样式重构（`42a309d`、`1dab542`）没有真实浏览器验收，
  只有 A12 的 jsdom 与 HTTP/静态壳证据。
- 真实 512MB VPS / 生产部署 / 反向代理 / 持续高负载 / 断电持久性。
- 真实证书的 SMTP TLS 握手（只有来源阶段的 fake SMTP 明文与「要求 STARTTLS 时失败」用例）。
- ARM64 实机、Windows/macOS 文件系统。
- 旧数据兼容：Prisma 数据库、旧 `uploads/`、旧 schema 1 数据均不导入、不兼容，没有就地升级。
- 许可证：沿用来源 MIT 许可（`LICENSE`，版权人保留原文）；**未**新增本仓库版权人或变更许可证条款。
- 未部署（只发布二进制包）；发布与 CI 结果见 A13 补记与 Releases 页面。
  远端仓库为 `github.com/xyz0z0/PicBox`。**验证当时尚未 commit**；随后补的初始提交只做仓库初始化，
  不改变上述验证结果，也不把 `commit=unknown` 的产物变成正式发布。
