package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setVersionVars 临时替换 ldflags 注入变量，测试结束后恢复。
func setVersionVars(t *testing.T, ver, sha, built string) {
	t.Helper()
	originalVersion, originalCommit, originalBuildTime := version, commit, buildTime
	t.Cleanup(func() {
		version, commit, buildTime = originalVersion, originalCommit, originalBuildTime
	})
	version, commit, buildTime = ver, sha, built
}

// --version 必须打印注入的字段，且完全不创建数据目录（这是打包脚本与 CI 的冒烟断言）。
func TestVersionFlagReportsInjectedFieldsWithoutDataDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	t.Setenv("PICBOX_DATA_DIR", dataDir)
	setVersionVars(t, "1.2.3", "abc1234", "2026-11-17T00:00:00Z")

	var out bytes.Buffer
	if err := run([]string{"--version"}, &out); err != nil {
		t.Fatalf("--version 不应失败: %v", err)
	}

	got := out.String()
	for _, want := range []string{"version=1.2.3", "commit=abc1234", "buildTime=2026-11-17T00:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Fatalf("--version 输出缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "note=") {
		t.Fatalf("已注入版本信息时不应输出开发构建提示:\n%s", got)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--version 不应创建数据目录 %s: %v", dataDir, err)
	}
}

// 直接 go build 的二进制必须能一眼看出是开发构建。
func TestVersionFlagMarksDevBuild(t *testing.T) {
	setVersionVars(t, "dev", "unknown", "unknown")

	var out bytes.Buffer
	if err := run([]string{"-version"}, &out); err != nil {
		t.Fatalf("-version 不应失败: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "version=dev") || !strings.Contains(got, "note=") {
		t.Fatalf("开发构建应输出 dev 版本与说明:\n%s", got)
	}
}

// 未知参数必须报错退出，而不是被静默忽略后启动服务；-h 打印用法后正常退出。
func TestRunRejectsUnknownArguments(t *testing.T) {
	if err := run([]string{"--nope"}, &bytes.Buffer{}); err == nil {
		t.Fatal("未知参数应当返回错误")
	}
	if err := run([]string{"extra"}, &bytes.Buffer{}); err == nil {
		t.Fatal("多余的位置参数应当返回错误")
	}

	var help bytes.Buffer
	if err := run([]string{"-h"}, &help); err != nil {
		t.Fatalf("-h 不应失败: %v", err)
	}
	if !strings.Contains(help.String(), "--version") {
		t.Fatalf("用法说明应提到 --version:\n%s", help.String())
	}
}
