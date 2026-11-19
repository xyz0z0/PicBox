# 贡献指南

感谢你对 PicBox 的关注与贡献。提交代码或文档前，请先阅读本指南以及仓库根目录的 [`CODE-STANDARDS.md`](./CODE-STANDARDS.md)。

## 本地开发环境

PicBox 采用 Go 单二进制 + 内嵌静态 React 前端架构：

- **Go**：用于构建后端服务、运行单元测试与打包（具体版本以 `go.mod` 与 CI 为准）。
- **Node.js & pnpm 10**：仅在修改前端源码时需要。请在 `frontend/` 目录下使用 `pnpm@10`：

```sh
cd frontend
npx -y pnpm@10 install --frozen-lockfile
```

> 仅修改 Go 代码时，直接复用仓库中已提交的 `frontend/dist/`，无需安装 Node 或运行前端构建。

## 强制规范（摘自 `CODE-STANDARDS.md`）

1. **内嵌前端产物必须与源码逐字节一致**
   任何对 `frontend/app/**`、`frontend/lib/**`、`frontend/src/**` 或 `frontend/package.json` 的改动，都必须在 `frontend/` 内运行 `pnpm verify` 并**同时提交重建后的 `frontend/dist/`**（包含哈希文件名变化）。CI 会检查 `frontend/dist` 与源码是否同步。
2. **写入/清理/打包类脚本必须先解析物理路径再动手**
   任何会写入、覆盖或删除文件的脚本，在执行动作前必须用 `realpath -m` 解析输出目录与最终产物路径（含已存在的祖先符号链接），确认位于允许的根目录内且不指向数据目录；拒绝时不得产生任何文件，版本号等外部输入须做白名单校验。
3. **验收记录必须区分「本仓库独立验收」与「来源历史证据」**
   修改 [`VALIDATION.md`](./VALIDATION.md) 时，不得将来源历史证据改写为本仓库实测结果，未在本仓库执行的验证须明确标注。

## 提交前自检命令

请根据改动范围在提交前运行对应检查（均在仓库根目录执行，前端命令在 `frontend/` 执行）：

```sh
# 1. 前端（修改 frontend/{app,lib,src} 或 package.json 时必跑）
cd frontend && npx -y pnpm@10 verify    # tsc + jsdom 测试 + vite build + check-dist

# 2. Go 代码检查与全量测试（注意不要用 -short 跳过测试）
go vet ./...
go test -race ./... -timeout 450s

# 3. 打包脚本守卫（修改 scripts/ 或发布逻辑时必跑）
./scripts/test-build-release.sh

# 4. HTTP 契约与静态壳回归（修改路由、接口或前端静态壳时必跑）
node scripts/test-web-v2.cjs
```

仅修改 `*.md` 纯文档时，请至少运行 `git diff --check` 并核对新增或修改的相对链接目标存在。

## Pull Request 要求

- **范围聚焦**：每个 PR 尽量聚焦单一问题或功能，避免混入无关格式化或数据文件（不要提交 `data/`、`*.sqlite*`、`auth.key`、`.env`、`node_modules/`、`dist/release/` 等）。
- **说明清晰**：按 `.github/PULL_REQUEST_TEMPLATE.md` 填写改动说明、关联 Issue 与实际执行过的验证项。
- **通过检查**：确保上述对应自检命令与 GitHub Actions CI 全部通过，且未通过删减断言或跳过测试来使检查变绿。
- **不破坏现有契约**：涉及数据库 schema 变更时，须按 `internal/server/schema.sql` 与 `schemaVersion` 规范演进，保持只读校验顺序。
