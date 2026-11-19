#!/usr/bin/env bash
# build-release.sh 的轻量回归：路径边界（符号链接/逃逸）、版本号与前端产物前置校验。
#
# 设计约定：
# - 全部用例都在 `mktemp -d` 的沙箱里跑：沙箱内复制一份 build-release.sh 与最小目录结构，
#   因此 `SERVER_DIR` 指向沙箱，不会读写仓库里的 data/、dist/ 或任何其他目录。
# - 拒绝类用例覆盖“构建前就被拒绝”的路径（版本号非法、out-dir/release/version 祖先越界、
#   前端产物缺失/不完整）；它们走不到 `go build`，不产生产物。
# - 最后一条“完整前端 fixture 越过前置校验”的用例会进入 `go build`：沙箱里没有 Go 源码，
#   所以它在打印编译信息后立刻失败。这条用例只证明前端前置校验不是永久阻塞，允许沙箱写入。
# - 每个拒绝用例都要求：非零退出 + 命中预期错误信息 + 沙箱内容没有任何变化。
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly BUILD_SCRIPT="$SCRIPT_DIR/build-release.sh"
readonly SANDBOX="$(mktemp -d "${TMPDIR:-/tmp}/picbox-build-release-guards.XXXXXX")"
trap 'rm -rf "$SANDBOX"' EXIT

failures=0
case_count=0

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}

pass() {
  printf 'ok   %s\n' "$*"
}

snapshot() {
  (cd "$SANDBOX" && find . -mindepth 1 | LC_ALL=C sort)
}

[ -f "$BUILD_SCRIPT" ] || {
  printf '错误: 未找到 %s\n' "$BUILD_SCRIPT" >&2
  exit 1
}

# ---- 沙箱布局：一个假的 PicBox 仓库根，加上各种指向外部的符号链接 ----
server="$SANDBOX/picbox"
outside="$SANDBOX/outside"
mkdir -p "$server/scripts" "$server/cmd/picbox" "$server/deploy" "$server/data" "$outside"
cp "$BUILD_SCRIPT" "$server/scripts/build-release.sh"
chmod +x "$server/scripts/build-release.sh"
printf 'module picbox\n\ngo 1.26.0\n' >"$server/go.mod"
printf '# 沙箱占位：build-release.sh 只检查存在性\n' >"$server/deploy/picbox.service"
printf '# 沙箱占位：build-release.sh 只检查存在性\n' >"$server/deploy/INSTALL.md"
printf 'PicBox license fixture\n' >"$server/LICENSE"
printf '# Third party notices fixture\n' >"$server/THIRD_PARTY_NOTICES.md"

ln -s "$outside" "$server/out-link"                     # out-dir 指向沙箱外
ln -s "data" "$server/data-link"                        # out-dir 指向 data
mkdir -p "$server/root-release-link"
ln -s "$outside" "$server/root-release-link/release"    # release/ 祖先指向沙箱外
mkdir -p "$server/root-version-link/release"
ln -s "$outside" "$server/root-version-link/release/1.2.3"   # 版本目录祖先指向沙箱外
mkdir -p "$server/root-release-data-link"
ln -s "$server/data" "$server/root-release-data-link/release" # release/ 祖先指向 data

initial="$(snapshot)"

# run_case <名称> <预期错误片段> [build-release.sh 参数...]
run_case() {
  local name="$1" expected="$2"
  shift 2
  case_count=$((case_count + 1))

  local output status current
  set +e
  output="$("$server/scripts/build-release.sh" "$@" 2>&1)"
  status=$?
  set -e

  if [ "$status" -eq 0 ]; then
    fail "$name: 期望非零退出，实际返回 0"
    printf '%s\n' "$output" >&2
    return
  fi
  if ! grep -qF "$expected" <<<"$output"; then
    fail "$name: 输出未包含预期信息 \"$expected\""
    printf '%s\n' "$output" >&2
    return
  fi
  current="$(snapshot)"
  if [ "$current" != "$initial" ]; then
    fail "$name: 拒绝之后沙箱内容发生变化（不应有任何写入）"
    diff <(printf '%s\n' "$initial") <(printf '%s\n' "$current") >&2 || true
    return
  fi
  pass "$name"
}

# ---- 路径边界：已存在的祖先符号链接不得绕过仓库根与 data 边界 ----
run_case "out-dir 符号链接指向沙箱外" "输出目录必须位于" --version 1.2.3 --out-dir "$server/out-link"
run_case "out-dir 符号链接指向 data" "拒绝写入数据目录" --version 1.2.3 --out-dir "$server/data-link"
run_case "out-dir 直接写 data" "拒绝写入数据目录" --version 1.2.3 --out-dir "$server/data"
run_case "out-dir 绝对路径在沙箱外" "输出目录必须位于" --version 1.2.3 --out-dir "$outside"
run_case "release/ 祖先符号链接指向沙箱外" "发布目录逃逸输出根目录" --version 1.2.3 --out-dir "$server/root-release-link"
run_case "版本目录祖先符号链接指向沙箱外" "发布目录逃逸输出根目录" --version 1.2.3 --out-dir "$server/root-version-link"
run_case "release/ 祖先符号链接指向 data" "发布目录逃逸输出根目录" --version 1.2.3 --out-dir "$server/root-release-data-link"

# ---- 版本号：非 semver 形式必须拒绝（构建前退出） ----
for bad in "1.2" "../evil" "1.2.3/../../etc" "1.2.3;id" "1.2.3 " "v"; do
  run_case "非法版本号: '${bad}'" "非法版本号" --version "$bad"
done
run_case "空版本号: --version ''" "版本号不能为空" --version ""
run_case "空版本号: --version=" "版本号不能为空" --version=
run_case "未知参数" "未知参数" --nope

# ---- 许可文件：缺失或为空都必须在任何写入前拒绝 ----
mv "$server/LICENSE" "$server/LICENSE.saved"
initial="$(snapshot)"
run_case "缺少根许可证文件" "缺少或为空的许可证文件" --version 1.2.3 --out-dir "$server"
mv "$server/LICENSE.saved" "$server/LICENSE"
initial="$(snapshot)"
: >"$server/LICENSE"
initial="$(snapshot)"
run_case "根许可证文件为空" "缺少或为空的许可证文件" --version 1.2.3 --out-dir "$server"
printf 'PicBox license fixture\n' >"$server/LICENSE"
initial="$(snapshot)"
mv "$server/THIRD_PARTY_NOTICES.md" "$server/THIRD_PARTY_NOTICES.saved"
initial="$(snapshot)"
run_case "缺少第三方声明文件" "缺少或为空的第三方声明文件" --version 1.2.3 --out-dir "$server"
mv "$server/THIRD_PARTY_NOTICES.saved" "$server/THIRD_PARTY_NOTICES.md"
initial="$(snapshot)"
: >"$server/THIRD_PARTY_NOTICES.md"
initial="$(snapshot)"
run_case "第三方声明文件为空" "缺少或为空的第三方声明文件" --version 1.2.3 --out-dir "$server"
printf '# Third party notices fixture\n' >"$server/THIRD_PARTY_NOTICES.md"
initial="$(snapshot)"

# ---- 前端产物：Go 二进制嵌入 frontend/dist，缺失或不完整必须在编译前拒绝 ----
# 此时沙箱还没有 frontend/dist，前面的守卫已全部通过。
run_case "缺少前端入口 index.html" "缺少前端入口" --version 1.2.3 --out-dir "$server"

mkdir -p "$server/frontend/dist"
printf '<!doctype html><html><body></body></html>\n' >"$server/frontend/dist/index.html"
initial="$(snapshot)"
run_case "缺少前端资源目录 assets" "缺少前端资源目录" --version 1.2.3 --out-dir "$server"

mkdir -p "$server/frontend/dist/assets"
initial="$(snapshot)"
run_case "前端入口没有引用任何 assets" "没有引用任何 /assets/ 资源" --version 1.2.3 --out-dir "$server"

printf '<!doctype html><html><head><script type="module" src="/assets/missing.js"></script></head></html>\n' \
  >"$server/frontend/dist/index.html"
initial="$(snapshot)"
run_case "前端入口引用的资源缺失" "前端入口引用的资源缺失: /assets/missing.js" --version 1.2.3 --out-dir "$server"

# ---- 完整前端 fixture：应越过前置校验并开始编译（沙箱无 Go 源码，编译随即失败） ----
mkdir -p "$server/frontend/dist/assets"
printf '/* fixture */\n' >"$server/frontend/dist/assets/app.css"
printf '/* fixture */\n' >"$server/frontend/dist/assets/app.js"
printf '<!doctype html><html><head><link rel="stylesheet" href="/assets/app.css">' \
  '<script type="module" src="/assets/app.js"></script></head></html>\n' \
  >"$server/frontend/dist/index.html"

case_count=$((case_count + 1))
set +e
output="$("$server/scripts/build-release.sh" --version 1.2.3 --out-dir "$server" 2>&1)"
status=$?
set -e
if [ "$status" -eq 0 ]; then
  fail "完整前端 fixture: 期望 go build 失败（沙箱无 Go 源码），实际返回 0"
  printf '%s\n' "$output" >&2
elif ! grep -qF "编译 linux/amd64" <<<"$output"; then
  fail "完整前端 fixture: 未越过前端校验进入编译"
  printf '%s\n' "$output" >&2
elif grep -qE "缺少前端入口|前端入口没有引用|前端入口引用的资源缺失|缺少前端资源目录" <<<"$output"; then
  fail "完整前端 fixture: 仍被前端前置校验拦下"
  printf '%s\n' "$output" >&2
else
  pass "完整前端 fixture: 越过前端校验并进入编译"
fi

echo
if [ "$failures" -ne 0 ]; then
  printf 'FAIL: %d/%d 个用例未通过\n' "$failures" "$case_count" >&2
  exit 1
fi
printf 'PASS: %d 个用例（路径边界、版本号与前端产物前置校验）\n' "$case_count"
