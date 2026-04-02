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

### 与你现有 `deepl_keys` 表结构兼容性
你给的结构：
```sql
CREATE TABLE deepl_keys (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  auth_key TEXT NOT NULL,
  endpoint TEXT NOT NULL DEFAULT 'https://api.deepl-pro.com',
  status TEXT NOT NULL DEFAULT 'active',
  disable_type TEXT,
  disabled_until INTEGER,
  last_error_code TEXT,
  last_error_message TEXT,
  last_used_at INTEGER,
  last_checked_at INTEGER,
  character_count INTEGER,
  character_limit INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
```
是**直接兼容**的。当前版本不再使用 `site_type` 字段，而是由 `endpoint` 自动推断 provider（`api.deepl.com` 视为 official，其它按 deepl-pro 处理）。

注意：当前版本已移除针对历史表结构的自动 `ALTER TABLE` 兼容逻辑，建议按上面的结构初始化数据库后再部署。

### 手动建表 SQL（D1）
如果你希望手动初始化数据库，可执行：
```sql
CREATE TABLE IF NOT EXISTS deepl_keys (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  auth_key TEXT NOT NULL,
  endpoint TEXT NOT NULL DEFAULT 'https://api.deepl-pro.com',
  status TEXT NOT NULL DEFAULT 'active',
  disable_type TEXT,
  disabled_until INTEGER,
  last_error_code TEXT,
  last_error_message TEXT,
  last_used_at INTEGER,
  last_checked_at INTEGER,
  character_count INTEGER,
  character_limit INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_deepl_keys_status_id ON deepl_keys(status, id);

CREATE TABLE IF NOT EXISTS deepl_translation_cache (
  cache_key TEXT PRIMARY KEY,
  body TEXT NOT NULL,
  headers_json TEXT,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_translation_cache_expires_at ON deepl_translation_cache(expires_at);
```

### 前后端分离结构
- 后端 Worker：`src/index.js`
- 管理台静态资源：`webui/index.html`, `webui/app.js`, `webui/style.css`

### 翻译结果缓存（两级缓存：Cloudflare Cache + D1）
- 变量 `CACHE_TTL_SECONDS` 用于控制翻译结果缓存秒数（`>0` 开启，`<=0` 关闭）。
- 命中条件：相同的翻译请求参数（`text`、`target_lang` 等）会命中同一个缓存键。
- 一级缓存：先查 Cloudflare Cache；命中后直接返回，不再请求上游 DeepL API。
- 二级缓存：若 Cloudflare Cache 未命中，则查询 D1 表 `deepl_translation_cache`；命中后会回填 Cloudflare Cache 并返回。
- 响应头 `X-Cache-Status`：
  - `HIT`：命中缓存；
  - `HIT-DB`：一级未命中，命中二级 D1 缓存；
  - `MISS`：两级都未命中，已请求上游并回填缓存。
- 缓存字段中不保存 `X-Upstream-Key-Name`（避免缓存泄露上游 key 名称）。
- 如需临时跳过缓存，可在请求头加 `x-no-cache: 1`。

### 部署方式
- Cloudflare Git 集成：仓库更新自动部署
- 本地部署：
```bash
npm i -g wrangler
wrangler deploy
```
