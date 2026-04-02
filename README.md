## Deepl Proxy Worker

### 为什么部署后变量和 D1 绑定会“消失”？
Cloudflare 部署时会把 `wrangler.toml` 当作**期望状态**。
- 如果你只在 Dashboard 手动加了绑定/变量，但 `wrangler.toml` 没声明，部署后就可能被覆盖掉。
- 你之前配置里没有 `[[d1_databases]]`，所以部署时 `DB` 绑定会被移除，进而出现 `Cannot read properties of undefined (reading 'prepare')`。

### 本仓库已做的修复
1. 增加 `keep_vars = true`，保留 Dashboard 里手动配置的 Variables/Secrets。
2. 在 `wrangler.toml` 中显式声明 `[[d1_databases]]`，绑定名固定为 `DB`。

### 你需要做的事
1. 打开 `wrangler.toml`，把：
   - `database_name`
   - `database_id`
   改成你自己的 D1 信息。
2. 重新部署（Cloudflare Git 或 `wrangler deploy`）。
3. 确认 Dashboard -> Settings 里能看到：
   - Variables/Secrets（含 `ADMIN_TOKEN`, `GATEWAY_TOKEN` 等）
   - D1 binding：`DB`

### 前后端分离结构
- 后端 Worker：`src/index.js`
- 管理台静态资源：`webui/index.html`, `webui/app.js`, `webui/style.css`

### 部署方式
- Cloudflare Git 集成：仓库更新自动部署
- 本地部署：
```bash
npm i -g wrangler
wrangler deploy
```
