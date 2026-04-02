## Deepl Proxy Worker

### 前后端分离
- 后端 Worker 业务逻辑在 `src/index.js`。
- 管理台静态页面在 `webui/`（`index.html` / `app.js` / `style.css`）。
- 通过 `wrangler.toml` 的 `[assets]` 绑定由 Cloudflare 提供静态资源。

### 部署方式（可自由选）
#### 方式 A：Cloudflare Git 集成（推荐）
1. Workers & Pages -> Create -> Workers -> Import a repository。
2. 选择仓库后，Deploy command 使用 `npx wrangler deploy`。
3. 设置环境变量：`GATEWAY_TOKEN`、`ADMIN_TOKEN`、`ALLOW_ORIGIN` 等。
4. 后续仓库更新由 Cloudflare 自动检测并部署。

#### 方式 B：本地/NPM 手动部署
```bash
npm i -g wrangler
wrangler deploy
```

### 功能说明
- 支持 deepl-pro.com 与官方 DeepL 中转（`site_type`）。
- deepl-pro key 在 quota/limit 场景下永久禁用（不自动解封）。
- official key 可不支持 usage（仅 official key 时 `/v2/usage` 返回 501）。
- WebUI 支持 key 增删改查。
