package config

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// chdirTemp 把工作目录切到临时目录：Load 读的是 CWD 下的 config.yaml，
// 不切目录会读到开发者本机的真实配置。
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	return dir
}

func clearWebhookEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"WEBHOOK_URL", "WEBHOOK_FORMAT", "WEBHOOK_ENABLED"} {
		t.Setenv(key, "")
	}
}

func TestLoadWebhookDefaults(t *testing.T) {
	chdirTemp(t)
	clearWebhookEnv(t)

	cfg := Load()
	w := cfg.Webhook
	if w.Format != "generic" || w.TitlePrefix != "DeepL Proxy" {
		t.Fatalf("format/title = %q/%q", w.Format, w.TitlePrefix)
	}
	if w.TimeoutSeconds != 8 || w.MinIntervalSeconds != 600 {
		t.Fatalf("timeouts = %d/%d", w.TimeoutSeconds, w.MinIntervalSeconds)
	}
	if !w.NotifyKeyDisabled || !w.NotifyNoKeys || w.NotifyTemporary {
		t.Fatalf("notify flags = %+v", w)
	}
	if w.Enabled || w.Configured() {
		t.Fatal("未配置时不应启用通知")
	}
}

func TestImportLegacyWebhook(t *testing.T) {
	dir := chdirTemp(t)
	clearWebhookEnv(t)
	yaml := `webhook:
  enabled: true
  format: "meow"
  nickname: "meow-test-user"
  notify_temporary: true
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, ok, err := ImportLegacy(filepath.Join(dir, "config.yaml"))
	if err != nil || !ok {
		t.Fatalf("ImportLegacy: ok=%v err=%v", ok, err)
	}
	if cfg.Webhook.Format != "meow" || cfg.Webhook.Nickname != "meow-test-user" {
		t.Fatalf("webhook = %+v", cfg.Webhook)
	}
	if !cfg.Webhook.NotifyTemporary {
		t.Fatal("notify_temporary 未生效")
	}
	// yaml 未写的字段必须保留默认值（yaml.Unmarshal 不会清零整个结构体）。
	if cfg.Webhook.MinIntervalSeconds != 600 || cfg.Webhook.TitlePrefix != "DeepL Proxy" {
		t.Fatalf("默认值被覆盖: %+v", cfg.Webhook)
	}
	if !cfg.Webhook.Configured() {
		t.Fatal("meow 格式只要昵称非空就算已配置")
	}
}

func TestLoadWebhookEnvOverrides(t *testing.T) {
	dir := chdirTemp(t)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("webhook:\n  enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEBHOOK_ENABLED", "true")
	t.Setenv("WEBHOOK_FORMAT", "TEXT")
	t.Setenv("WEBHOOK_URL", "http://127.0.0.1:9000/hook")

	cfg := Load()
	if !cfg.Webhook.Enabled {
		t.Fatal("WEBHOOK_ENABLED 未生效")
	}
	if cfg.Webhook.Format != "text" {
		t.Fatalf("格式应归一化为小写，得到 %q", cfg.Webhook.Format)
	}
	if cfg.Webhook.URL != "http://127.0.0.1:9000/hook" {
		t.Fatalf("url = %q", cfg.Webhook.URL)
	}
	if !cfg.Webhook.Configured() {
		t.Fatal("http 地址应视为已配置")
	}
}

func TestWebhookConfiguredRequiresURLForGeneric(t *testing.T) {
	enabled := WebhookConfig{Enabled: true, Format: "generic"}
	if enabled.Configured() {
		t.Fatal("generic 格式没有 URL 时不应算已配置")
	}
	if (WebhookConfig{Enabled: false, URL: "http://x"}).Configured() {
		t.Fatal("未启用时 Configured 必须为 false")
	}
}

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		port string
		db   string
	}{
		{"empty", nil, "", ""},
		{"single dash", []string{"-port", "9090", "-db-path", "/tmp/a.db"}, "9090", "/tmp/a.db"},
		{"double dash", []string{"--port=9091", "--db-path=/tmp/b.db"}, "9091", "/tmp/b.db"},
	}
	for _, tc := range cases {
		f, err := ParseArgs(tc.args)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if f.Port != tc.port || f.DBPath != tc.db {
			t.Fatalf("%s: got %+v", tc.name, f)
		}
	}

	if _, err := ParseArgs([]string{"-port"}); err == nil {
		t.Fatal("缺值时应当报错")
	}
	if _, err := ParseArgs([]string{"extra"}); err == nil {
		t.Fatal("位置参数应当报错")
	}
	if _, err := ParseArgs([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h 应返回 flag.ErrHelp，得到 %v", err)
	}
}

func TestResolveDBPath(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "cwd")
	exeDir := filepath.Join(dir, "bin")
	for _, d := range []string{cwd, exeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// 显式路径优先，且不做存在性检查。
	got, src := ResolveDBPath("/tmp/x.db", cwd, exeDir)
	if got != "/tmp/x.db" || src != "命令行" {
		t.Fatalf("explicit: %q %q", got, src)
	}

	// 都找不到 → 当前目录/data/deepl-proxy.db（默认）。
	got, src = ResolveDBPath("", cwd, exeDir)
	if got != filepath.Join(cwd, "data", "deepl-proxy.db") || src != "默认" {
		t.Fatalf("default: %q %q", got, src)
	}

	// 程序目录优先于当前目录？不是：当前目录优先。
	exeDB := filepath.Join(exeDir, "data", "deepl-proxy.db")
	if err := os.MkdirAll(filepath.Dir(exeDB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exeDB, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, src = ResolveDBPath("", cwd, exeDir)
	if got != exeDB || src != "程序目录" {
		t.Fatalf("exe dir: %q %q", got, src)
	}

	// 当前目录有库时优先用它。
	cwdDB := filepath.Join(cwd, "deepl-proxy.db")
	if err := os.WriteFile(cwdDB, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, src = ResolveDBPath("", cwd, exeDir)
	if got != cwdDB || src != "当前目录" {
		t.Fatalf("cwd: %q %q", got, src)
	}

	// data/ 下的库优先于同目录裸文件名。
	cwdDataDB := filepath.Join(cwd, "data", "deepl-proxy.db")
	if err := os.MkdirAll(filepath.Dir(cwdDataDB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cwdDataDB, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, src = ResolveDBPath("", cwd, exeDir)
	if got != cwdDataDB || src != "当前目录" {
		t.Fatalf("cwd data: %q %q", got, src)
	}

	// 目录（同名）不算命中。
	dirAsDB := filepath.Join(dir, "isdir")
	if err := os.MkdirAll(filepath.Join(dirAsDB, "deepl-proxy.db"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, _ = ResolveDBPath("", dirAsDB, "")
	if got != filepath.Join(dirAsDB, "data", "deepl-proxy.db") {
		t.Fatalf("dir should not match: %q", got)
	}
}

func TestLoadFromPrecedence(t *testing.T) {
	chdirTemp(t)
	t.Setenv("PORT", "7000")
	t.Setenv("DB_PATH", "")

	cfg, err := LoadFrom(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != "7000" {
		t.Fatalf("env port = %q", cfg.Server.Port)
	}
	if cfg.Database.Source != "默认" {
		t.Fatalf("source = %q", cfg.Database.Source)
	}

	cfg, err = LoadFrom([]string{"-port", "7100", "-db-path", "/tmp/flag.db"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != "7100" || cfg.Database.Path != "/tmp/flag.db" || cfg.Database.Source != "命令行" {
		t.Fatalf("flag wins: %+v", cfg.Database)
	}

	t.Setenv("DB_PATH", "/tmp/env.db")
	cfg, err = LoadFrom(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Path != "/tmp/env.db" || cfg.Database.Source != "环境变量" {
		t.Fatalf("env db: %+v", cfg.Database)
	}
}
