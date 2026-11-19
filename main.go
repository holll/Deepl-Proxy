package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"deepl-proxy/config"
	"deepl-proxy/database"
	"deepl-proxy/handler"
	"deepl-proxy/service"

	"github.com/gin-gonic/gin"
)

//go:embed webui/*
var webuiEmbed embed.FS

// 构建时通过 -ldflags "-X main.Version=... -X main.Commit=... -X main.BuildTime=..." 注入。
var (
	Version   = "dev"
	Commit    = "dev"
	BuildTime = "dev"
)

func main() {
	// 子命令：不启动服务，直接重置数据库里的管理员令牌。
	if len(os.Args) > 1 && os.Args[1] == "reset-admin" {
		if err := resetAdminCommand(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			log.Printf("reset-admin: %v", err)
			os.Exit(1)
		}
		return
	}

	// 把真正的逻辑放进 run() 返回 error：这样数据库关闭等 defer 一定会执行。
	// 旧实现在 ListenAndServe 的 goroutine 里直接 log.Fatalf，进程被强制退出，
	// 所有 defer（含 DB 关闭）全部跳过。
	if err := run(); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadFrom(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // -h/--help 已经把用法打印出来了
		}
		return err
	}

	log.Printf("DeepL Proxy %s (commit %s, built %s)", Version, Commit, BuildTime)
	log.Printf("数据库: %s（来源：%s）", cfg.Database.Path, cfg.Database.Source)

	// 应用级 ctx：收到信号或 server 出错时统一取消所有后台 goroutine，
	// 让它们有机会退出而不是变成进程级孤儿。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 初始化数据库
	writeDB, readDB, err := database.Init(ctx, cfg.Database.Path)
	if err != nil {
		return err
	}
	defer func() {
		if err := writeDB.Close(); err != nil {
			log.Printf("close write db: %v", err)
		}
		if err := readDB.Close(); err != nil {
			log.Printf("close read db: %v", err)
		}
	}()

	// 运行期设置：数据库为唯一来源（首次启动会把旧的 config.yaml 迁进来）。
	settings, err := service.NewSettings(ctx, writeDB, cfg)
	if err != nil {
		return err
	}

	// 初始化服务
	kr := service.NewKeyring(ctx, writeDB, readDB)
	defer kr.Close()
	// 缓存参数在构造时快照；后台修改缓存设置后提示“重启生效”。
	cs := service.NewCacheService(writeDB, readDB, settings.Get())

	// webhook 通知（key 失效 / 配额用尽 / 无可用 key）。
	// 未配置时是空对象，所有调用都是 no-op，不会给主链路带来分支成本。
	notifier := service.NewWebhookManager(ctx, settings)
	kr.SetNotifier(notifier.Notifier())
	defer notifier.Close()

	var wg sync.WaitGroup

	// 启动时查询所有 key 的用量
	wg.Go(func() { kr.RefreshAllUsage(ctx) })

	// 定时恢复已到期的 Key，并在关闭数据库前等待任务退出。
	wg.Go(func() { kr.RunKeyRecovery(ctx) })

	// 定时缓存清理
	wg.Go(func() { runCacheCleanup(ctx, cs, settings) })

	// 初始化 Gin
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	if err := r.SetTrustedProxies([]string{"127.0.0.1"}); err != nil {
		log.Printf("trusted proxies warning: %v", err)
	}
	r.Use(gin.Recovery())
	r.Use(handler.RequestLogger())

	// 静态文件 webui（embed）
	webuiFS, err := fs.Sub(webuiEmbed, "webui")
	if err != nil {
		log.Printf("webui embed warning: %v", err)
	} else {
		r.StaticFS("/webui", http.FS(webuiFS))
	}

	// 路由注册
	handler.SetupRoutes(r, kr, cs, settings, settings, notifier)

	// 启动 HTTP server
	addr := ":" + settings.Get().Server.Port
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
		// 没有这些超时，一个慢连接就能永久占住一个 goroutine 和一份内存；
		// 在 1.8GB 内存的机器上这是很实际的 OOM 路径。
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	srvErr := make(chan error, 1)
	wg.Go(func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	})
	log.Printf("DeepL Proxy starting on %s", addr)

	var runErr error
	select {
	case <-ctx.Done():
		log.Println("Shutting down ...")
	case err := <-srvErr:
		runErr = err
		log.Printf("listen error: %v", err)
	}

	stop() // Cancel lifecycle work also when ListenAndServe fails.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown error: %v", err)
	}

	wg.Wait()
	return runErr
}

// runCacheCleanup 按配置间隔清理过期缓存，ctx 取消即退出。
//
// 间隔在启动时读取一次：后台修改清理参数后需要重启才会应用，
// 响应里通过 restart_required_for 明确告知。
func runCacheCleanup(ctx context.Context, cs *service.CacheService, settings *service.Settings) {
	interval := time.Duration(settings.Get().Cache.CleanupInterval) * time.Second
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cs.CleanExpired(ctx)
		}
	}
}
