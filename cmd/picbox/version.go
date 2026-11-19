package main

import (
	"fmt"
	"io"
	"runtime"
	"strings"
)

// 发布构建通过
// -ldflags "-s -w -X main.version=... -X main.commit=... -X main.buildTime=..."
// 注入这三个变量（见 scripts/build-release.sh）；直接 go build / go run
// 时保留下面的开发默认值，避免空字符串被误认为"正式版本"。
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

// writeVersion 把版本信息写到 w。
// -version/--version 必须在读取配置、创建数据目录、监听端口之前完成并退出，
// 因此这里只使用 runtime 信息，不做任何文件或网络操作。
func writeVersion(w io.Writer) error {
	fields := []struct {
		name  string
		value string
	}{
		{"version", version},
		{"commit", commit},
		{"buildTime", buildTime},
		{"go", runtime.Version()},
		{"platform", runtime.GOOS + "/" + runtime.GOARCH},
	}

	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, "picbox %s=%s\n", f.name, valueOr(f.value, "unknown"))
	}
	if isDevBuild() {
		b.WriteString("picbox note=本地开发构建：未通过 -ldflags 注入 version/commit/buildTime\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// isDevBuild 只在完全未注入版本信息时返回 true。
func isDevBuild() bool {
	return version == "dev" && commit == "unknown" && buildTime == "unknown"
}
