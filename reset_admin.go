package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"deepl-proxy/config"
	"deepl-proxy/database"
	"deepl-proxy/service"
)

// resetAdminCommand 实现 `deepl-proxy reset-admin`：不启动服务，直接改库里的管理员令牌。
//
// 使用场景是把自己锁在门外（忘记令牌、会话密钥丢了、或者初始化时手滑）。
// 改完同时轮换会话密钥，所有已登录会话立即失效。
func resetAdminCommand(args []string) error {
	fs := flag.NewFlagSet("deepl-proxy reset-admin", flag.ContinueOnError)
	var (
		token     string
		dbPath    string
		fromStdin bool
	)
	fs.StringVar(&dbPath, "db-path", "", "SQLite 数据库路径（不传时与启动逻辑一样自动查找）")
	fs.StringVar(&token, "token", "", "直接指定新令牌（≥8 位）；不传则随机生成")
	fs.BoolVar(&fromStdin, "stdin", false, "从标准输入读取新令牌（避免出现在命令历史/进程列表里）")
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintf(out, "用法: deepl-proxy reset-admin [选项]\n\n")
		fmt.Fprintf(out, "不启动 HTTP 服务，直接把数据库里的管理员令牌改成新值，并轮换会话密钥。\n\n选项:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("无法识别的参数: %s", strings.Join(fs.Args(), " "))
	}
	if token != "" && fromStdin {
		return errors.New("-token 与 -stdin 只能选一个")
	}

	cfgArgs := []string{}
	if dbPath != "" {
		cfgArgs = append(cfgArgs, "-db-path", dbPath)
	}
	cfg, err := config.LoadFrom(cfgArgs)
	if err != nil {
		return err
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	writeDB, readDB, err := database.Init(ctx, cfg.Database.Path)
	if err != nil {
		return err
	}
	defer func() { _ = readDB.Close() }()
	defer func() { _ = writeDB.Close() }()

	settings, err := service.NewSettings(ctx, writeDB, cfg)
	if err != nil {
		return err
	}

	generated := false
	if fromStdin {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err != nil {
			return fmt.Errorf("读取标准输入失败: %w", err)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			return errors.New("标准输入里没有令牌")
		}
	}
	if token == "" {
		token, err = service.GenerateAdminToken()
		if err != nil {
			return err
		}
		generated = true
	}

	if _, err := settings.ResetAdminToken(ctx, token); err != nil {
		return err
	}

	fmt.Printf("数据库: %s（来源：%s）\n", cfg.Database.Path, cfg.Database.Source)
	if generated {
		fmt.Printf("新的管理员令牌: %s\n", token)
		fmt.Println("请立即保存；该令牌只在这里显示一次。")
	} else {
		fmt.Println("管理员令牌已按指定值重置。")
	}
	if settings.SetupRequired() {
		fmt.Println("提示: 网关令牌仍为空，请打开管理后台完成初始化，否则翻译接口不可用。")
	}
	fmt.Println("会话密钥已轮换，之前登录的会话全部失效。")
	return nil
}
