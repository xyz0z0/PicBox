## 改动说明

<!-- 简述本次 PR 做了什么、为什么这样改 -->

## 关联 Issue

<!-- 例如：Closes #123 或 Fixes #123（无则留空或写「无」） -->

## 验证清单

请仅勾选你在提交前**实际执行并通过**的检查项；不适用当前改动的项请留空或注明原因：

- [ ] Go 静态检查与全量测试：`go vet ./...` 与 `go test -race ./... -timeout 450s`
- [ ] 前端构建与产物同步（改动 `frontend/{app,lib,src}` 或 `package.json` 时必选）：已在 `frontend/` 运行 `pnpm verify` 并提交重建的 `frontend/dist/`
- [ ] 脚本检查（改动 `scripts/` 或打包逻辑时必选）：已运行 `./scripts/test-build-release.sh`（以及 `bash -n`）
- [ ] HTTP 契约与静态壳回归（改动路由、接口或前端静态壳时必选）：已运行 `node scripts/test-web-v2.cjs`
- [ ] 仅文档改动：已运行 `git diff --check` 并核对新增/修改的相对链接有效
