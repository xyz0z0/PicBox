# CODE-STANDARDS 强制规范

本文件收录必须无条件满足、且能自动检查的规则。每条都指明守卫位置；违反时必须在 review 中点名。

---

### 内嵌前端产物必须与源码逐字节一致

**要求：** 任何对 `frontend/{app,lib,src}` 或 `frontend/package.json` 的改动，都必须在
`frontend/` 内用 pnpm 10 重新 `pnpm verify` 并提交重建的 `frontend/dist/`（含哈希文件名变化）。
Go 二进制嵌入的是 `dist/`，不是源码；产物过期时编译照样成功，但线上 UI 是旧版本。

**自动化：**
- `frontend/scripts/check-dist.mjs`（`pnpm verify` 的最后一步）断言单外链 module script、
  扁平 `/assets/*`、无 `process.env` / `next/*` / `PrismaClient` / Node 内建模块。
- `.github/workflows/go.yml` 的 `Verify committed frontend bundle is current` 用
  `git status --porcelain --untracked-files=all` + `git diff --exit-code` 检查 `frontend/dist`。
- `scripts/build-release.sh` 在任何写入前拒绝缺失或不完整的 `frontend/dist`。

---

### 写入/清理/打包类脚本必须先解析物理路径再动手

**要求：** 任何会写入、覆盖或删除文件的脚本，在动作之前用 `realpath -m` 解析输出目录与最终产物
路径（已存在的祖先符号链接同样要解析），确认仍在允许的根目录内且不是数据目录。拒绝时不得产生
任何文件；版本号等外部输入必须按白名单校验。

**自动化：** `scripts/test-build-release.sh` 在 `mktemp -d` 沙箱里覆盖 out-dir 越界、指向 `data/`、
`release/`/版本目录祖先符号链接、非法版本号与前端产物缺失等分支，并断言拒绝后沙箱无任何变化。
CI 在打包前运行它。

---

### 验收记录必须区分「本仓库独立验收」与「来源历史证据」

**要求：** `VALIDATION.md` 中每个数字、每条结论都要标明属于哪一类：
在本仓库实际跑出的命令与结果，或从源项目（PicHome，提交 `9a920c9` / `3b60b0b`）带入的历史证据。
历史证据不得改写成新仓库的实测结果，也不得把旧构建的数字当成本仓库产物的指标。
本仓库未执行的验证（浏览器交互、Docker 资源测量、ARM64 实机、真实 SMTP）必须明确写为未验证。

**自动化：** 无（文档规则）；review 时逐项核对 `VALIDATION.md` 的章节归属与命令可复现性。
