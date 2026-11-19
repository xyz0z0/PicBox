# Repository Guidelines

PicBox 是自托管的图片服务：Go 单二进制提供 HTTP 与 API，界面是嵌入二进制的静态 React 包。
本文件写给在本仓库里改代码的人（含 agent）。改动前先读 [`CODE-STANDARDS.md`](./CODE-STANDARDS.md)。

## 委派与分工

- 开始实质工作前，先根据用户要验收的结果识别主任务。范围和验收标准明确时，优先把主任务交给子 agent；只派搭建、准备样本或清理，仍由主 agent 完成核心任务，未达到委派要求。零碎操作可直接处理。
- 依赖同一浏览器标签页、登录状态或临时实例的连续流程，交给同一个子 agent 从头做到尾。互不依赖的任务可按容量并行。
- 按当前会话规则确认派发路线。子 agent 能否使用所需工具或权限不明确时，先让它做最小探测。确认受限后，主 agent 只接手受限步骤，交付时说明具体原因。
- 主 agent 负责划定范围、关键决策和抽查证据；证据足够时不重复完整验证。交付时简要说明子 agent 执行了什么、主 agent 复核了什么。

## 目录与所有权

| 路径 | 说明 |
| --- | --- |
| `cmd/picbox/` | 入口：`--version`、配置加载、HTTP 服务生命周期与优雅退出 |
| `internal/server/` | 路由、认证、图片、管理端、SQLite schema/open 校验、邮件、审计、限流 |
| `frontend/app/`、`frontend/lib/` | 从 PicHome 复制过来的纯浏览器组件与函数（原样复用，不要重写成简化界面） |
| `frontend/src/` | 静态入口、页面装配（`main.tsx`、`src/pages/*`）、shims、jsdom 行为测试 |
| `frontend/dist/` | Vite 构建产物，**必须提交**（Go 二进制嵌入它） |
| `deploy/` | systemd 模板与安装/升级/回滚说明 |
| `scripts/` | 发布打包、打包守卫、HTTP 回归、Docker 资源验收 |
| `testdata/` | 资源验收原始记录（来源阶段，见 `VALIDATION.md`） |

Go module 名为 `picbox`。命令、二进制、systemd unit、配置前缀统一为 `picbox` / `PICBOX_`；
通用变量（`BASEURL`、`SMTP_*`、`UPLOAD_MAX_BYTES`、`TRUSTED_PROXIES` 等）保持既有语义。

## 命令

```sh
# 前端（pnpm 10，与 CI 一致）
cd frontend && npx -y pnpm@10 install --frozen-lockfile
npx -y pnpm@10 verify          # tsc + 30 条 jsdom 测试 + vite build + check-dist

# Go
go mod verify
go vet ./...
go test -race ./... -timeout 450s

# 打包与回归（都在仓库根执行）
./scripts/test-build-release.sh
./scripts/build-release.sh --version 0.0.1-dev.1
node scripts/test-web-v2.cjs                       # 自己临时构建并启动隔离服务
PICBOX_BIN=/path/to/picbox node scripts/test-web-v2.cjs
```

`go test -race ./...` 需要约 3 分钟（真实 SQLite 与图片处理），不要用 `-short` 或缩小范围冒充全量。

## 验证边界

| 改动 | 必须做 |
| --- | --- |
| 纯文档（只改 `*.md`） | `git diff --check`；核对新增/修改的相对链接目标存在 |
| Go 代码 | `go vet ./...`、`go test -race ./... -timeout 450s`，涉及发布脚本时再跑 `./scripts/test-build-release.sh` |
| 前端源码（`frontend/{app,lib,src}`、`package.json`） | `pnpm verify`，提交重建的 `frontend/dist/`（逐字节一致） |
| 打包/清理/写入类脚本 | `bash -n <script>` + 对应守卫脚本（`test-build-release.sh`） |
| HTTP 契约或静态壳 | `node scripts/test-web-v2.cjs`（196 条断言） |
| 安装/升级文档 | 命令与路径与 `scripts/build-release.sh`、`deploy/picbox.service` 实际产物一致 |

前端 `dist/` 新鲜度由 CI 的 git 状态比对守卫，脚本写入边界由 `test-build-release.sh` 守卫；
不要绕过它们，也不要删除断言来让测试变绿。

## 数据、密钥与产物

- 不要提交：`data/`、`*.sqlite*`、`auth.key`、上传的图片、真实 `.env`、`node_modules/`、
  `dist/release/`（发布包）、`resource-results*.json`。
- `.env.example` 必须保持可公开（不含真实密钥）；新增配置项同时更新 `.env.example` 与 README 表格。
- 数据库 schema 变更必须走 `internal/server/schema.sql` 并同步 `schemaVersion`，
  且保持「非空数据库先只读校验标记」的顺序；不要用 `CREATE IF NOT EXISTS` 接管外部库。

## 提交

- commit 信息用中文；正文写清跑了哪些命令、结果如何、**没有尝试**什么，并引用适用的
  `CODE-STANDARDS.md` 规则（没有则写「无相关规则」）。
- 先回交、集中复核通过后再 commit；**未经明确指示不得 push、发布或部署**。
- 不要把 `/tmp` 报告、凭据、私有审计或测试数据带进仓库。

## 规则来源

本仓库的规则只收录可自动化或可回查的条目。PicHome（源提交 `9a920c9`）的 `LESSONS.md`
记录了拆分前的历史教训，其中与本仓库相关的两条已提升为 [`CODE-STANDARDS.md`](./CODE-STANDARDS.md)；
其余条目绑定源仓库的提交与脚本，未迁入以避免引用本仓库不存在的路径。
