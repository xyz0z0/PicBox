#!/usr/bin/env bash
# 构建 PicBox Go 原生发布包：
#   Linux amd64 / arm64 静态二进制 + 对应 systemd 模板 + 简明中文安装说明 + SHA256SUMS
#
# 前置：静态前端已构建并提交（cd frontend && pnpm install --frozen-lockfile && pnpm verify）。
# dist 是二进制的嵌入内容，脚本会在任何写入之前校验入口与引用资源存在；生产运行机仍不需要 Node。
#
# 只写 <out-dir>/release/<version>/（默认 dist），不修改源码与数据目录，
# 不调用 sudo / systemctl，不发布、不上传、不打 tag。
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SERVER_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd -P)"
readonly DEFAULT_OUT_DIR="$SERVER_DIR/dist"
readonly SERVICE_TEMPLATE="$SERVER_DIR/deploy/picbox.service"
readonly INSTALL_TEMPLATE="$SERVER_DIR/deploy/INSTALL.md"
readonly LICENSE_FILE="$SERVER_DIR/LICENSE"
readonly THIRD_PARTY_NOTICES_FILE="$SERVER_DIR/THIRD_PARTY_NOTICES.md"
readonly DEFAULT_VERSION="0.0.0-dev"
readonly TARGETS=("amd64" "arm64")

VERSION=""
VERSION_PROVIDED=0
OUT_DIR="$DEFAULT_OUT_DIR"

usage() {
  cat <<'EOF'
用法: build-release.sh [--version <版本号>] [--out-dir <目录>]

选项:
  --version <版本号>  发布版本，形如 1.2.3 / v1.2.3 / 1.2.3-rc.1。
                      默认 0.0.0-dev（占位版本，正式发布必须显式指定）。
  --out-dir <目录>    输出根目录，必须（物理路径，含符号链接解析）位于仓库根目录内，
                      且不能是 data/。默认 dist。
  -h, --help          显示本帮助。

产物（每个架构一个目录 + 一个 tarball，最后是校验清单）:
  <out-dir>/release/<version>/picbox_<version>_linux_amd64.tar.gz
  <out-dir>/release/<version>/picbox_<version>_linux_arm64.tar.gz
  <out-dir>/release/<version>/SHA256SUMS

本脚本不会停止或安装任何服务，也不会执行 sudo、systemctl、curl|sh 或远程脚本；
安装步骤由管理员按包内 INSTALL.md 手工执行。
EOF
}

die() {
  printf '错误: %s\n' "$*" >&2
  exit 1
}

info() {
  printf '[build-release] %s\n' "$*"
}

# 物理路径：用 realpath -m 展开“已存在的祖先（含自身）”里的全部符号链接，
# 不存在的尾部按词法拼合。只做词法规整，dist/link -> /tmp 这类符号链接
# 在字符串层面看起来仍在仓库内，实际会写到外部或 data/
physical_path() {
  realpath -m -- "$1" 2>/dev/null || die "无法解析路径: $1"
}

# 拼接产物路径，并强制校验解析后的物理路径仍在 base 目录内
# （防止版本号等输入逃逸目标目录，也防止目录祖先是指向外的符号链接）。
safe_join() {
  local base="$1" child="$2" joined
  joined="$(physical_path "$base/$child")"
  case "$joined" in
    "$base"/*) ;;
    *)
      printf '错误: 产物路径逃逸目标目录: %s（基准 %s）\n' "$joined" "$base" >&2
      exit 1
      ;;
  esac
  printf '%s' "$joined"
}

# 版本号只允许 semver 形式；可选的 v 前缀会被去掉，避免进入目录名后带出多余字符。
normalize_version() {
  local raw="$1" stripped
  [ -n "$raw" ] || die "版本号不能为空"
  [ "${#raw}" -le 64 ] || die "版本号过长（最多 64 字符）: ${#raw} 字符"
  stripped="${raw#v}"
  if ! [[ "$stripped" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$ ]]; then
    die "非法版本号: '$raw'（只接受 1.2.3 / 1.2.3-rc.1 / 1.2.3+build.5 形式，不允许斜杠、空格、引号或路径片段）"
  fi
  printf '%s' "$stripped"
}

host_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) printf 'amd64' ;;
    aarch64 | arm64) printf 'arm64' ;;
    *) uname -m ;;
  esac
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || die "--version 需要参数"
      VERSION="$2"
      VERSION_PROVIDED=1
      shift 2
      ;;
    --version=*)
      VERSION="${1#--version=}"
      VERSION_PROVIDED=1
      shift
      ;;
    --out-dir)
      [ "$#" -ge 2 ] || die "--out-dir 需要参数"
      OUT_DIR="$2"
      shift 2
      ;;
    --out-dir=*)
      OUT_DIR="${1#--out-dir=}"
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      die "未知参数: $1"
      ;;
  esac
done

if [ "$VERSION_PROVIDED" -eq 0 ]; then
  VERSION="$DEFAULT_VERSION"
  info "未指定 --version，使用占位版本 $DEFAULT_VERSION（正式发布请显式指定）"
fi
VERSION="$(normalize_version "$VERSION")"

# ---- 前置检查：任何一条不满足都在写文件之前退出 ----
command -v go >/dev/null 2>&1 || die "未找到 go 工具链（构建机需要 Go，运行机不需要）"
command -v gzip >/dev/null 2>&1 || die "未找到 gzip"
command -v sha256sum >/dev/null 2>&1 || die "未找到 sha256sum"
command -v tar >/dev/null 2>&1 || die "未找到 tar"
command -v realpath >/dev/null 2>&1 || die "未找到 realpath（coreutils，用于解析符号链接后的物理路径）"
if ! tar --version 2>/dev/null | head -n 1 | grep -qi 'gnu tar'; then
  die "需要 GNU tar（可复现打包用的 --sort/--owner/--mtime 参数）"
fi
[ -f "$SERVER_DIR/go.mod" ] || die "缺少 $SERVER_DIR/go.mod"
[ -d "$SERVER_DIR/cmd/picbox" ] || die "缺少 $SERVER_DIR/cmd/picbox"
[ -f "$SERVICE_TEMPLATE" ] || die "缺少服务模板 $SERVICE_TEMPLATE"
[ -f "$INSTALL_TEMPLATE" ] || die "缺少安装说明 $INSTALL_TEMPLATE"
[ -f "$LICENSE_FILE" ] && [ -s "$LICENSE_FILE" ] || die "缺少或为空的许可证文件 $LICENSE_FILE"
[ -f "$THIRD_PARTY_NOTICES_FILE" ] && [ -s "$THIRD_PARTY_NOTICES_FILE" ] || die "缺少或为空的第三方声明文件 $THIRD_PARTY_NOTICES_FILE"

OUT_ABS="$(physical_path "$OUT_DIR")"
SERVER_REAL="$(physical_path "$SERVER_DIR")"
DATA_REAL="$(physical_path "$SERVER_DIR/data")"
case "$OUT_ABS/" in
  "$SERVER_REAL"/*) ;;
  *) die "输出目录必须位于 $SERVER_REAL 内（当前物理路径: $OUT_ABS；指向仓库外的符号链接同样会被拒绝）" ;;
esac
case "$OUT_ABS/" in
  "$DATA_REAL"/*) die "拒绝写入数据目录: $OUT_ABS" ;;
esac

RELEASE_DIR="$(physical_path "$OUT_ABS/release/$VERSION")"
case "$RELEASE_DIR" in
  "$OUT_ABS"/release/*) ;;
  *) die "发布目录逃逸输出根目录: $RELEASE_DIR（release/ 或版本目录若是指向外的符号链接会被拒绝）" ;;
esac

if [ -e "$RELEASE_DIR" ] && [ -n "$(ls -A "$RELEASE_DIR" 2>/dev/null || true)" ]; then
  die "目标目录已存在且非空: $RELEASE_DIR（脚本不覆盖已有产物，请先确认后手动移走）"
fi

# ---- 静态前端产物：它是 Go 二进制的一部分（frontend/embed.go），必须先于编译检查 ----
# 生产运行机不需要 Node，但构建机必须先把 frontend/dist 构建并提交：
#   cd frontend && pnpm install --frozen-lockfile && pnpm verify
# 否则 go build 会把空或不存在的目录当成嵌入源而失败，或者带着上一次的陈旧 UI。
FRONTEND_DIST="$SERVER_DIR/frontend/dist"
FRONTEND_INDEX="$FRONTEND_DIST/index.html"
[ -f "$FRONTEND_INDEX" ] || die "缺少前端入口 $FRONTEND_INDEX（先运行: cd frontend && pnpm install --frozen-lockfile && pnpm verify）"
[ -d "$FRONTEND_DIST/assets" ] || die "缺少前端资源目录 $FRONTEND_DIST/assets（先运行: cd frontend && pnpm verify）"

# 入口里引用的每个 /assets/ 文件都必须真实存在，避免空目录或只提交了 index.html 时
# 打包出一个打开就 404 的发布版本。grep 无匹配时不要被 set -e 当成脚本错误。
FRONTEND_ASSETS="$(grep -oE '/assets/[A-Za-z0-9._-]+\.(js|css|svg)' "$FRONTEND_INDEX" | LC_ALL=C sort -u || true)"
[ -n "$FRONTEND_ASSETS" ] || die "前端入口 $FRONTEND_INDEX 没有引用任何 /assets/ 资源（产物不完整，先运行 pnpm verify）"
while IFS= read -r asset_url; do
  [ -f "$FRONTEND_DIST/${asset_url#/}" ] || die "前端入口引用的资源缺失: $asset_url（先运行: cd frontend && pnpm verify）"
done <<<"$FRONTEND_ASSETS"
info "前端产物: $FRONTEND_INDEX（$(printf '%s\n' "$FRONTEND_ASSETS" | wc -l | tr -d ' ') 个入口资源）"

# 纯 HEAD short hash 会把“未提交/未跟踪的 PicBox 源码”当成同一份代码，
# 因此工作区只要有任何改动（含未跟踪文件）就在 commit 后追加 -dirty。
short_commit() {
  local sha
  if ! sha="$(git -C "$SERVER_DIR" rev-parse --short HEAD 2>/dev/null)"; then
    printf 'unknown'
    return 0
  fi
  if [ -n "$(git -C "$SERVER_DIR" status --porcelain --untracked-files=normal -- . 2>/dev/null)" ]; then
    printf '%s-dirty' "$sha"
    return 0
  fi
  printf '%s' "$sha"
}

COMMIT="$(short_commit)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
GO_VERSION="$(go version 2>/dev/null || true)"

staging_dirs=()
cleanup_staging() {
  local dir
  for dir in "${staging_dirs[@]:-}"; do
    [ -n "$dir" ] && rm -rf "$dir"
  done
}
trap cleanup_staging EXIT

info "版本: $VERSION"
info "提交: $COMMIT"
case "$COMMIT" in
  *-dirty) info "警告: 源码含未提交或未跟踪改动，commit 已标记 -dirty，此产物不得作为正式发布版本" ;;
esac
info "构建时间(UTC): $BUILD_TIME"
info "发布目录: $RELEASE_DIR"
info "构建工具链: $GO_VERSION"

mkdir -p "$RELEASE_DIR"

ldflags="-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.buildTime=$BUILD_TIME"

build_target() {
  local arch="$1"
  local name="picbox_${VERSION}_linux_${arch}"
  local stage tarball description
  stage="$(safe_join "$RELEASE_DIR" "$name")"
  tarball="$(safe_join "$RELEASE_DIR" "$name.tar.gz")"

  [ -e "$stage" ] && die "临时目录已存在: $stage"
  [ -e "$tarball" ] && die "产物已存在: $tarball"

  mkdir -p "$stage"
  staging_dirs+=("$stage")

  info "编译 linux/$arch ..."
  (
    cd "$SERVER_DIR"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
      go build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$stage/picbox" ./cmd/picbox
  )
  chmod 0755 "$stage/picbox"
  cp "$SERVICE_TEMPLATE" "$stage/picbox.service"
  cp "$INSTALL_TEMPLATE" "$stage/INSTALL.md"
  cp "$LICENSE_FILE" "$stage/LICENSE"
  cp "$THIRD_PARTY_NOTICES_FILE" "$stage/THIRD_PARTY_NOTICES.md"
  chmod 0644 "$stage/picbox.service" "$stage/INSTALL.md" "$stage/LICENSE" "$stage/THIRD_PARTY_NOTICES.md"

  # 静态链接自检：运行机不需要 libc、Node、SQLite 服务或图片处理库。
  if command -v file >/dev/null 2>&1; then
    description="$(file -b "$stage/picbox")"
    case "$description" in
      *ELF*) ;;
      *) die "产物不是 ELF 可执行文件（linux/$arch）: $description" ;;
    esac
    case "$description" in
      *'statically linked'* | *'static-pie linked'*) ;;
      *) die "产物不是静态链接（linux/$arch）: $description" ;;
    esac
    info "file: $description"
  else
    info "未找到 file 命令，跳过静态链接检查（需要人工确认）"
  fi

  # 本机架构的二进制实际跑一次 --version，确认 ldflags 注入生效且不需要数据目录。
  if [ "$arch" = "$(host_arch)" ]; then
    local printed
    printed="$("$stage/picbox" --version)"
    case "$printed" in
      *"version=$VERSION"*) ;;
      *) die "本机 $arch 二进制的 --version 未包含 version=$VERSION: $printed" ;;
    esac
    info "本机 $arch --version 校验通过"
  fi

  # 可复现打包：固定顺序、mtime、属主，gzip 不写时间戳。
  tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner \
    -C "$RELEASE_DIR" -cf - "$name" | gzip -n -9 >"$tarball"

  rm -rf "$stage"
  info "已生成 $tarball"
}

for arch in "${TARGETS[@]}"; do
  build_target "$arch"
done

(
  cd "$RELEASE_DIR"
  LC_ALL=C sha256sum -- *.tar.gz >SHA256SUMS
  sha256sum -c SHA256SUMS
)

info "完成。产物目录: $RELEASE_DIR"
info "校验清单: $RELEASE_DIR/SHA256SUMS"
ls -l "$RELEASE_DIR"
