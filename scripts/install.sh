#!/usr/bin/env bash
# PicBox Linux 一键安装 / 升级脚本：
#   自动识别系统架构（amd64 / arm64），从 GitHub Releases 下载发布包与 SHA256SUMS，
#   校验完整性与 --version 后，配置 picbox 系统用户、/var/lib/picbox 与 systemd 服务。
#
# 安全约定：
#   - 版本号按 semver 白名单校验；
#   - 所有写入路径先经 realpath -m 解析物理路径，拒绝指向数据目录或根目录；
#   - 默认仅执行首次安装，若检测到已有安装则拒绝覆盖，需显式传入 --upgrade；
#   - 升级模式只原子替换二进制，不覆盖 /var/lib/picbox 与 /etc/picbox.env。
set -euo pipefail

readonly REPO="xyz0z0/PicBox"
readonly RELEASE_BASE="https://github.com/${REPO}/releases/download"
readonly LATEST_API="https://api.github.com/repos/${REPO}/releases/latest"

readonly DEFAULT_BIN_PATH="/usr/local/bin/picbox"
readonly DEFAULT_SERVICE_PATH="/etc/systemd/system/picbox.service"
readonly DEFAULT_DATA_DIR="/var/lib/picbox"
readonly SERVICE_USER="picbox"

VERSION=""
UPGRADE=0
UPDATE_SERVICE=0
DRY_RUN=0
BIN_PATH="$DEFAULT_BIN_PATH"
SERVICE_PATH="$DEFAULT_SERVICE_PATH"
DATA_DIR="$DEFAULT_DATA_DIR"
WORK_DIR=""

usage() {
  cat <<'EOF'
用法: install.sh [选项]

选项:
  --version <版本号>   指定安装版本（如 0.1.0 或 v0.1.0）；省略时自动获取最新 Release
  --upgrade            升级已有安装（原子替换二进制并重启服务，保留数据目录与 /etc/picbox.env）
  --update-service     配合 --upgrade 使用，同时用发布包内的模板覆盖 systemd unit
  --dry-run            仅校验参数、架构与路径，打印将执行的操作，不下载也不写入系统文件
  -h, --help           显示本帮助

示例:
  curl -fsSL https://raw.githubusercontent.com/xyz0z0/PicBox/main/scripts/install.sh | sudo bash
  curl -fsSL https://raw.githubusercontent.com/xyz0z0/PicBox/main/scripts/install.sh | sudo bash -s -- --version 0.1.0
  curl -fsSL https://raw.githubusercontent.com/xyz0z0/PicBox/main/scripts/install.sh | sudo bash -s -- --upgrade
EOF
}

die() {
  printf '错误: %s\n' "$*" >&2
  exit 1
}

info() {
  printf '[picbox-install] %s\n' "$*"
}

cleanup() {
  if [[ -n "$WORK_DIR" && -d "$WORK_DIR" ]]; then
    rm -rf -- "$WORK_DIR"
  fi
}
trap cleanup EXIT

physical_path() {
  realpath -m -- "$1" 2>/dev/null || die "无法解析路径: $1"
}

validate_version() {
  local raw="$1" stripped
  stripped="${raw#v}"
  if [[ -z "$stripped" || "${#stripped}" -gt 64 ]]; then
    die "非法版本号: '$raw'"
  fi
  if ! [[ "$stripped" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$ ]]; then
    die "非法版本号 '$raw'；仅支持 semver 格式（例如 0.1.0 或 v0.1.0）"
  fi
  printf '%s' "$stripped"
}

detect_arch() {
  local os arch
  os="$(uname -s)"
  [[ "$os" == "Linux" ]] || die "当前仅支持 Linux（检测到: $os）"
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) printf 'amd64' ;;
    aarch64|arm64) printf 'arm64' ;;
    *) die "不支持的 CPU 架构: $arch（仅支持 amd64 / arm64）" ;;
  esac
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "缺少必需命令: $1"
}

fetch_latest_version() {
  local json tag
  json="$(curl -fsSL -H 'Accept: application/vnd.github+json' "$LATEST_API")" || die "无法从 GitHub 获取最新版本信息"
  tag="$(printf '%s\n' "$json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
  [[ -n "$tag" ]] || die "未能解析最新 Release 的 tag_name"
  validate_version "$tag"
}

validate_target_paths() {
  local real_bin real_svc real_data
  real_bin="$(physical_path "$BIN_PATH")"
  real_svc="$(physical_path "$SERVICE_PATH")"
  real_data="$(physical_path "$DATA_DIR")"

  for p in "$real_bin" "$real_svc" "$real_data"; do
    [[ "$p" != "/" ]] || die "目标路径不能为根目录: $p"
  done

  case "$real_bin" in
    "$real_data"|"$real_data"/*) die "二进制路径不能位于数据目录内: $real_bin" ;;
  esac
  case "$real_svc" in
    "$real_data"|"$real_data"/*) die "systemd unit 路径不能位于数据目录内: $real_svc" ;;
  esac

  BIN_PATH="$real_bin"
  SERVICE_PATH="$real_svc"
  DATA_DIR="$real_data"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)
      [[ $# -ge 2 ]] || die "--version 需要参数"
      VERSION="$(validate_version "$2")"
      shift 2
      ;;
    --upgrade)
      UPGRADE=1
      shift
      ;;
    --update-service)
      UPDATE_SERVICE=1
      shift
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "未知参数: $1（使用 --help 查看用法）"
      ;;
  esac
done

require_cmd curl
require_cmd tar
require_cmd sha256sum
require_cmd realpath

ARCH="$(detect_arch)"
validate_target_paths

if [[ -z "$VERSION" ]]; then
  if [[ "$DRY_RUN" -eq 1 ]]; then
    VERSION="latest(待查询)"
  else
    info "正在查询 GitHub 最新版本..."
    VERSION="$(fetch_latest_version)"
  fi
fi

if [[ "$DRY_RUN" -eq 1 ]]; then
  info "DRY-RUN 模式：不下载、不修改系统"
  info "版本: $VERSION | 架构: linux/$ARCH | 模式: $([[ "$UPGRADE" -eq 1 ]] && echo '升级' || echo '首次安装')"
  info "目标二进制: $BIN_PATH"
  info "目标数据目录: $DATA_DIR"
  info "目标 systemd unit: $SERVICE_PATH"
  exit 0
fi

[[ "$(id -u)" -eq 0 ]] || die "请使用 root 权限运行（例如 sudo bash）"
require_cmd systemctl

if [[ "$UPGRADE" -eq 0 ]]; then
  if id "$SERVICE_USER" >/dev/null 2>&1 || [[ -e "$DATA_DIR" ]] || [[ -e "$BIN_PATH" ]] || [[ -e "$SERVICE_PATH" ]]; then
    die "检测到已有 PicBox 安装痕迹（用户/数据目录/二进制/unit）。若要升级请改用 --upgrade"
  fi
else
  [[ -e "$BIN_PATH" ]] || die "未检测到已安装的二进制 $BIN_PATH，请先不带 --upgrade 执行首次安装"
fi

WORK_DIR="$(mktemp -d)"
ARCHIVE="picbox_${VERSION}_linux_${ARCH}.tar.gz"
ARCHIVE_URL="${RELEASE_BASE}/v${VERSION}/${ARCHIVE}"
SUMS_URL="${RELEASE_BASE}/v${VERSION}/SHA256SUMS"

info "正在下载 ${ARCHIVE} 与 SHA256SUMS..."
curl -fsSL -o "${WORK_DIR}/${ARCHIVE}" "$ARCHIVE_URL" || die "下载安装包失败: $ARCHIVE_URL"
curl -fsSL -o "${WORK_DIR}/SHA256SUMS" "$SUMS_URL" || die "下载校验清单失败: $SUMS_URL"

info "正在校验 SHA256..."
(
  cd "$WORK_DIR"
  grep -F "  ${ARCHIVE}" SHA256SUMS > SHA256SUMS.target || die "SHA256SUMS 中未找到 ${ARCHIVE}"
  sha256sum -c SHA256SUMS.target
)

tar -xzf "${WORK_DIR}/${ARCHIVE}" -C "$WORK_DIR"
PKG_DIR="${WORK_DIR}/picbox_${VERSION}_linux_${ARCH}"
[[ -x "${PKG_DIR}/picbox" ]] || die "解包后未找到可执行文件 ${PKG_DIR}/picbox"
[[ -f "${PKG_DIR}/picbox.service" ]] || die "解包后未找到 ${PKG_DIR}/picbox.service"

VER_OUT="$("${PKG_DIR}/picbox" --version)"
info "包内二进制版本: ${VER_OUT}"
if [[ "$VER_OUT" == *"note="* || "$VER_OUT" == *"-dirty"* ]]; then
  die "拒绝部署非正式构建产物: ${VER_OUT}"
fi

if [[ "$UPGRADE" -eq 0 ]]; then
  info "创建系统用户 ${SERVICE_USER} 与数据目录 ${DATA_DIR}..."
  useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin "$SERVICE_USER"
  install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$DATA_DIR"
fi

BIN_DIR="$(dirname "$BIN_PATH")"
STAGING_BIN="${BIN_DIR}/.picbox.new.$$"
install -d -m 0755 "$BIN_DIR"
install -m 0555 "${PKG_DIR}/picbox" "$STAGING_BIN"
"${STAGING_BIN}" --version >/dev/null

if [[ "$UPGRADE" -eq 1 ]] && systemctl is-active --quiet picbox; then
  info "正在停止运行中的 picbox 服务..."
  systemctl stop picbox
fi

mv -f "$STAGING_BIN" "$BIN_PATH"

if [[ "$UPGRADE" -eq 0 || "$UPDATE_SERVICE" -eq 1 || ! -e "$SERVICE_PATH" ]]; then
  info "写入 systemd unit: ${SERVICE_PATH}"
  install -m 0644 "${PKG_DIR}/picbox.service" "$SERVICE_PATH"
fi

systemctl daemon-reload
if [[ "$UPGRADE" -eq 0 ]]; then
  systemctl enable --now picbox
else
  systemctl restart picbox
fi

sleep 1
systemctl is-active --quiet picbox || die "picbox 服务启动失败，请执行 journalctl -u picbox -n 50 查看日志"

info "PicBox v${VERSION} (${ARCH}) $([[ "$UPGRADE" -eq 1 ]] && echo '升级' || echo '安装')完成！"
info "默认监听: http://127.0.0.1:3001（首次访问请创建管理员账号）"
info "环境变量配置: /etc/picbox.env（可选）；服务管理: systemctl status picbox"
