package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"picbox/internal/server"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if err := run(os.Args[1:], os.Stdout); err != nil {
		log.Printf("%v", err)
		os.Exit(1)
	}
}

// run 先解析命令行参数：--version 只打印版本信息就返回，不读取配置、
// 不创建数据目录、不监听端口；否则按环境变量启动 HTTP 服务。
func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("picbox", flag.ContinueOnError)
	flags.SetOutput(stdout)
	showVersion := flags.Bool("version", false, "打印 version/commit/buildTime 后退出，不读取配置、不创建数据目录")
	flags.Usage = func() {
		fmt.Fprintf(stdout, "用法: %s [--version]\n\n不带参数时按环境变量启动 HTTP 服务。\n", flags.Name())
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// -h/--help 已打印用法，不属于错误。
			return nil
		}
		return fmt.Errorf("参数错误: %w", err)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("参数错误: 不支持的位置参数 %q（只接受 --version）", flags.Arg(0))
	}
	if *showVersion {
		return writeVersion(stdout)
	}

	cfg, err := server.LoadConfig()
	if err != nil {
		return fmt.Errorf("配置错误: %w", err)
	}

	app, err := server.New(cfg)
	if err != nil {
		return fmt.Errorf("启动失败: %w", err)
	}
	defer func() {
		if err := app.Close(); err != nil {
			log.Printf("关闭数据库失败: %v", err)
		}
	}()

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		// 原图/缩略图是流式响应，不设置 WriteTimeout。
		IdleTimeout:    90 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	listenErr := make(chan error, 1)
	go func() {
		// 启动日志只打印监听地址与数据目录，不打印 JWT 密钥或 Cookie。
		log.Printf("picbox 启动: 监听 %s，数据目录 %s", cfg.Addr, cfg.DataDir)
		listenErr <- httpServer.ListenAndServe()
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-listenErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP 服务异常: %w", err)
		}
		return nil
	case <-ctx.Done():
		log.Printf("收到退出信号，开始优雅退出")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			log.Printf("优雅退出失败，仍在清理: %v", shutdownErr)
			_ = httpServer.Close()
		}
		if err := <-listenErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP 服务退出异常: %w", err)
		}
		log.Printf("已退出")
		return nil
	}
}
