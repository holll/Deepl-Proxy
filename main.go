package main

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
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
	cfg := config.Load()

	log.Printf("DeepL Proxy %s (commit %s, built %s)", Version, Commit, BuildTime)

	// 初始化数据库
	writeDB, readDB := database.Init(cfg.Database.Path)
	defer writeDB.Close()
	defer readDB.Close()

	// 初始化服务
	kr := service.NewKeyring(writeDB, readDB)
	cs := service.NewCacheService(writeDB, readDB, cfg)

	// 启动时查询所有 key 的用量
	go kr.RefreshAllUsage()

	// 定时缓存清理
	go func() {
		ticker := time.NewTicker(time.Duration(cfg.Cache.CleanupInterval) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			cs.CleanExpired()
		}
	}()

	// 初始化 Gin
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.SetTrustedProxies([]string{"127.0.0.1"})
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
	handler.SetupRoutes(r, kr, cs, cfg)

	// 启动 HTTP server
	addr := ":" + cfg.Server.Port
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
	}
	log.Printf("DeepL Proxy starting on %s", addr)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	// 优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down ...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
