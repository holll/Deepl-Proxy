package main

import (
	"io/fs"
	"strings"
	"testing"
)

// 前端静态资源是 go:embed 进二进制的，改动不需要重新发布资源目录，
// 但也意味着没有构建期检查。这里用少量断言锁住 2026-11-18 审查里修掉的关键点，
// 防止后续编辑把「CSP / 语义表格 / 无内联事件 / 无 contenteditable」改回去。
func webuiSub(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(webuiEmbed, "webui")
	if err != nil {
		t.Fatalf("webui embed 子树无效: %v", err)
	}
	return sub
}

func readWebuiFile(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(webuiSub(t), name)
	if err != nil {
		t.Fatalf("读取内嵌资源 %s 失败: %v", name, err)
	}
	return string(data)
}

func TestWebuiAssetsEmbedded(t *testing.T) {
	for _, name := range []string{"index.html", "style.css", "app.js", "favicon.svg"} {
		if _, err := fs.Stat(webuiSub(t), name); err != nil {
			t.Errorf("webui/%s 未被打包: %v", name, err)
		}
	}
}

func TestWebuiSecurityBaseline(t *testing.T) {
	html := readWebuiFile(t, "index.html")
	js := readWebuiFile(t, "app.js")

	if !strings.Contains(html, `http-equiv="Content-Security-Policy"`) {
		t.Error("index.html 缺少 CSP meta")
	}
	for _, attr := range []string{"onclick=", "onchange=", "onload=", "onerror="} {
		if strings.Contains(html, attr) {
			t.Errorf("index.html 出现内联事件处理器 %q，会破坏 CSP", attr)
		}
	}
	if strings.Contains(js, "contenteditable") {
		t.Error("app.js 不应再依赖 contenteditable 行内编辑")
	}
	if strings.Contains(js, ".innerHTML = (data.keys") {
		t.Error("app.js 仍存在整表替换式渲染")
	}
	if !strings.Contains(js, "dialog.showModal") && !strings.Contains(js, "showModal()") {
		t.Error("app.js 不再使用原生 dialog.showModal()")
	}
}

func TestWebuiAccessibilityBaseline(t *testing.T) {
	html := readWebuiFile(t, "index.html")
	css := readWebuiFile(t, "style.css")

	if got := strings.Count(html, `scope="col"`); got != 12 {
		t.Errorf("表头 scope=col 数量 = %d，期望 12（Key 表 8 列 + 记录表 4 列）", got)
	}
	if strings.Contains(html, `<th scope="col">Key</th>`) {
		t.Error("Key 列应由操作区的查看按钮替代")
	}
	if got := strings.Count(html, "<caption"); got != 2 {
		t.Errorf("caption 数量 = %d，期望 2", got)
	}
	if strings.Contains(html, "<h3") {
		t.Error("index.html 不应出现 h3（h1→h2 层级）")
	}
	if !strings.Contains(html, "<noscript>") {
		t.Error("index.html 缺少 noscript 兜底")
	}
	if !strings.Contains(html, `id="fQuota"`) {
		t.Error("新增表单缺少配额周期选择（quota_period）")
	}
	if !strings.Contains(html, `<option value="auto" selected>自动识别`) {
		t.Error("Endpoint 默认应为“自动识别（按 Key 后缀）”")
	}
	if !strings.Contains(html, "<details") {
		t.Error("翻译记录面板应默认收起（details），避免喧宾夺主")
	}
	if !strings.Contains(css, "[hidden]") {
		t.Error("style.css 需要 [hidden] 强制 display:none，否则 .login-screen 会盖住页面")
	}
	if !strings.Contains(css, "prefers-reduced-motion") {
		t.Error("style.css 缺少 prefers-reduced-motion 降级")
	}
	if !strings.Contains(css, "@media (max-width: 720px)") {
		t.Error("style.css 缺少移动端断点")
	}
}

// TestWebuiSettingsBaseline 验证后台设置入口及其范围。
func TestWebuiSettingsBaseline(t *testing.T) {
	html := readWebuiFile(t, "index.html")
	js := readWebuiFile(t, "app.js")

	for _, id := range []string{
		`id="setupForm"`, `id="settingsDialog"`, `id="settingsForm"`,
		`id="sAdminToken"`, `id="sGatewayToken"`,
		`id="sTTLDays"`, `id="wFormat"`, `id="wURL"`, `id="wNickname"`, `id="webhookTestBtn"`,
	} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html 缺少设置项 %s", id)
		}
	}
	for _, api := range []string{"/admin/setup", "/admin/settings", "/admin/webhook/test", "/admin/settings/rotate-secret"} {
		if !strings.Contains(js, api) {
			t.Errorf("app.js 未调用 %s", api)
		}
	}
	for _, removed := range []string{"sPort", "sAllowOrigin", "sDatabasePath", "<legend>服务</legend>"} {
		if strings.Contains(html, removed) || strings.Contains(js, removed) {
			t.Errorf("前端仍包含已移除的服务设置 %s", removed)
		}
	}
	if strings.Contains(html, "config.yaml") {
		t.Error("前端不应再提示修改 config.yaml")
	}
}
