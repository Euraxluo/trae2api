// Package server 实现 `serve` 子命令：启动 Trae2Api 网关。
//
// 用法（注意：本包不再单独编译成可执行文件，它是单一二进制 trae2api 的子命令）：
//
//	trae2api                            # 不带子命令 = serve，用 ./config.json
//	trae2api serve -config D:/c.json    # 指定配置
//	trae2api serve -listen :9000        # 覆盖监听地址
//	trae2api serve -gui                 # 额外开一个桌面窗口（Windows 优先内嵌 WebView2）
package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"trae2api/internal/config"
	"trae2api/internal/desktop"
	"trae2api/internal/server"
)

// Run 执行 serve 子命令，返回退出码（由总入口 os.Exit）。
//
// 为什么用 FlagSet 而不是包级 flag.*：单一可执行文件里，总入口可能已经解析过
// 自己的参数；用包级 flag 会与它共用一套 flag 集合，产生难以排查的冲突。
// 每个子命令有自己的 FlagSet 是更干净的边界。
func Run(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.json", "配置文件路径")
	listen := fs.String("listen", "", "覆盖监听地址（如 127.0.0.1:7870）")
	gui := fs.Bool("gui", false, "额外打开一个桌面窗口显示控制台（Windows 下优先内嵌 WebView2）")
	guiBrowser := fs.Bool("gui-browser", false, "桌面窗口强制使用浏览器 App 模式（跳过 WebView2）")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	log.SetFlags(log.LstdFlags)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		return 1
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	srv, err := server.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		return 1
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		// 对话可能很长（推理模型 + 长回复），读超时给足；写超时靠客户端断开控制。
		ReadTimeout: 120 * time.Second,
		IdleTimeout: 120 * time.Second,
	}

	// 优雅停机：收到信号后停止接新请求，等待在途请求收尾。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Printf("收到停机信号，正在优雅退出…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	// 后台任务：额度刷新 + 每日签到（阻塞直到 ctx 取消）
	go srv.RunBackground(ctx)

	// 定期落盘池状态（冷却/熔断/计数跨重启保留）
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = srv.Pool().Save()
				return
			case <-t.C:
				if err := srv.Pool().Save(); err != nil {
					log.Printf("WARN: 落盘池状态失败: %v", err)
				}
			}
		}
	}()

	keyHint := "未设置（无需鉴权）"
	if cfg.APIKey != "" {
		keyHint = "已设置"
	}
	log.Printf("========================================================")
	log.Printf("  Trae2Api %s", server.Version)
	log.Printf("  Base URL : http://%s/v1", cfg.Listen)
	log.Printf("  控制台   : http://%s/", cfg.Listen)
	log.Printf("  API Key  : %s", keyHint)
	log.Printf("  模型前缀 : trae:<模型名>  (Trae CN, chat_v3)")
	log.Printf("             solo:<模型名>  (TRAE SOLO CN, solo_work_lite)")
	if cfg.Schedule.CheckinEnabled {
		log.Printf("  自动签到 : 已启用，每日 %v 点", cfg.Schedule.CheckinHours)
	} else {
		log.Printf("  自动签到 : 已关闭")
	}
	log.Printf("  配置     : %s", absOrSelf(*cfgPath))
	log.Printf("========================================================")

	// 桌面窗口：在**网关已经开始监听之后**才打开，否则窗口会先加载一个
	// 还没起来的服务，用户看到的是"无法连接"。
	//
	// 用一个就绪探测来等：窗口打开时服务必须已经能响应，否则体验很差。
	if *gui {
		// stopOnClose 只在**内嵌 WebView2** 模式置真。
		//
		// 用原子变量：OnReady 与 OnClose 由不同的 goroutine 调用
		//（WebView2 的 COM 回调线程 vs 消息循环线程）。
		var stopOnClose atomic.Bool
		go func() {
			// ⚠️ 两个 URL 必须分开：
			//   baseURL    给就绪探测用（waitReady 会拼 "healthz"）
			//   consoleURL 给窗口打开用（指向管理中枢 /console）
			// 混用会拼出 "http://host:port/consolehealthz" —— 永远探测失败。
			baseURL := localConsoleURL(cfg.Listen)
			consoleURL := baseURL + "console"
			if !waitReady(baseURL, 10*time.Second) {
				log.Printf("WARN: 网关未在 10s 内就绪，仍尝试打开桌面窗口")
			}
			mode, note := desktop.Available()
			if note != "" {
				log.Printf("INFO: 桌面窗口：%s（将使用 %s）", note, mode)
			}
			err := desktop.Run(desktop.Options{
				URL:              consoleURL,
				Title:            "Trae2Api 控制台",
				PreferBrowserApp: *guiBrowser,
				OnReady: func(m desktop.Mode) {
					log.Printf("INFO: 桌面窗口已打开（模式 %s）", m)
					// 只有内嵌窗口才"关窗即停机"：浏览器 App 模式是独立进程，
					// 用户可能只是关掉窗口改用浏览器标签页，停服务是误伤。
					stopOnClose.Store(m == desktop.ModeWebView2)
				},
				OnClose: func() {
					// **关窗即优雅停机**（对齐参考实现）。
					//
					// 为什么不是"关窗后继续跑"：用户双击 exe 打开窗口时，
					// 心里的模型是"这是个桌面应用"—— 关掉窗口就该退出。
					// 继续跑会留下一个**看不见的后台进程**占着端口，
					// 下次双击会因端口占用而失败，而用户完全不知道为什么。
					//
					// 只在**内嵌 WebView2** 模式这么做：浏览器 App 模式是独立进程，
					// 用户可能只是关掉窗口去用浏览器标签页，此时停服务是误伤。
					if !stopOnClose.Load() {
						log.Printf("INFO: 桌面窗口已关闭（浏览器 App 模式，网关继续运行；Ctrl+C 退出）")
						return
					}
					log.Printf("INFO: 桌面窗口已关闭，正在优雅停机…")
					stop()
				},
			})
			if err != nil {
				log.Printf("WARN: 桌面窗口退出: %v", err)
			}
		}()
	}

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "监听 %s 失败: %v\n", cfg.Listen, err)
		return 1
	}
	log.Printf("已退出。")
	return 0
}

// localConsoleURL 把监听地址转成浏览器可访问的控制台地址。
//
// 监听 0.0.0.0 或 :7870 时不能原样拼进 URL —— 0.0.0.0 不是可访问的地址。
func localConsoleURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		// 形如 ":7870"
		if strings.HasPrefix(listen, ":") {
			return "http://127.0.0.1" + listen + "/"
		}
		return "http://" + listen + "/"
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// waitReady 轮询等待网关可响应（窗口不该在服务起来之前打开）。
func waitReady(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url + "healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func absOrSelf(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
