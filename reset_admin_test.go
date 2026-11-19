package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"deepl-proxy/config"
	"deepl-proxy/database"
	"deepl-proxy/service"
)

// seedSettings 建一个已初始化的库，返回路径。
func seedSettings(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cli.db")
	writeDB, readDB, err := database.Init(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	base := config.Defaults()
	st, err := service.NewSettings(context.Background(), writeDB, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Setup(context.Background(), "old-admin-token", "keep-gateway-token", "*"); err != nil {
		t.Fatal(err)
	}
	_ = readDB.Close()
	_ = writeDB.Close()
	return dbPath
}

func readSettings(t *testing.T, dbPath string) *config.Config {
	t.Helper()
	writeDB, readDB, err := database.Init(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readDB.Close() }()
	defer func() { _ = writeDB.Close() }()
	st, err := service.NewSettings(context.Background(), writeDB, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	return st.Get()
}

func TestResetAdminCommandToken(t *testing.T) {
	dbPath := seedSettings(t)

	if err := resetAdminCommand([]string{"-db-path", dbPath, "-token", "cli-admin-token"}); err != nil {
		t.Fatal(err)
	}
	cfg := readSettings(t, dbPath)
	if cfg.Auth.AdminToken != "cli-admin-token" {
		t.Fatalf("token = %q", cfg.Auth.AdminToken)
	}
	if cfg.Auth.GatewayToken != "keep-gateway-token" {
		t.Fatalf("gateway 不应被改动: %q", cfg.Auth.GatewayToken)
	}
}

func TestResetAdminCommandStdin(t *testing.T) {
	dbPath := seedSettings(t)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	go func() {
		_, _ = w.WriteString("stdin-admin-token\n")
		_ = w.Close()
	}()

	if err := resetAdminCommand([]string{"-db-path", dbPath, "-stdin"}); err != nil {
		t.Fatal(err)
	}
	if cfg := readSettings(t, dbPath); cfg.Auth.AdminToken != "stdin-admin-token" {
		t.Fatalf("token = %q", cfg.Auth.AdminToken)
	}
}

func TestResetAdminCommandRejectsShortToken(t *testing.T) {
	dbPath := seedSettings(t)
	if err := resetAdminCommand([]string{"-db-path", dbPath, "-token", "short"}); err == nil {
		t.Fatal("短令牌应当报错")
	}
	if cfg := readSettings(t, dbPath); cfg.Auth.AdminToken != "old-admin-token" {
		t.Fatalf("失败时不应改动令牌: %q", cfg.Auth.AdminToken)
	}
}

func TestResetAdminCommandMixedInput(t *testing.T) {
	dbPath := seedSettings(t)
	if err := resetAdminCommand([]string{"-db-path", dbPath, "-token", "abc-token-1", "-stdin"}); err == nil {
		t.Fatal("-token 与 -stdin 同时使用应当报错")
	}
}
