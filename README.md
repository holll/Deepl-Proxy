# Deepl Proxy Worker

一个基于 **Cloudflare Workers + D1 + Cache API** 的 DeepL 代理服务，提供：
- 多 Key 轮询与自动熔断；
- DeepL 兼容翻译接口；
- 管理后台（Web UI）；
- 两级缓存（Cloudflare Cache + D1）；
- 简单会话登录与管理 API。

---

## 1. 软件功能

### 1.1 DeepL 兼容 API
- `POST /v2/translate`（兼容 `/translate`）
  - 转发到上游 DeepL / deepl-pro 站点；
  - 支持常见参数（`text`、`target_lang`、`source_lang`、`formality` 等）。
- `GET /v2/usage`
  - 对非 official 站点 key 提供用量查询聚合。

### 1.2 多 Key 管理与自动容错
- Key 信息存储在 D1 的 `deepl_keys` 表；
- 自动区分 provider（`api.deepl.com` 视为 official，其他按 deepl-pro）；
- 根据错误类型自动临时禁用 / 永久禁用 key；
- 可通过管理 API 新增、修改、删除 key。

### 1.3 管理后台（Web UI）
- 静态页面路径：`/webui/`；
- 支持管理员登录、查看与维护 key、刷新 usage 快照。

### 1.4 两级缓存（重点）
- 一级：Cloudflare Cache API；
- 二级：D1 表 `deepl_translation_cache`；
- 流程：
  1. 先查 Cloudflare Cache；
  2. 未命中则查 D1；
  3. D1 命中后回填 Cloudflare Cache；
  4. 两级都未命中才请求上游。
- 响应头 `X-Cache-Status`：
  - `HIT`：命中 Cloudflare 缓存；
  - `HIT-DB`：命中 D1 二级缓存；
  - `MISS`：两级未命中，已请求上游并写回缓存。
- 缓存数据不会保存 `X-Upstream-Key-Name`，避免缓存暴露上游 key 标识。
- 过期缓存清理由 **Cron Trigger** 执行（不在查询路径中做清理），降低实时请求时延。

---

## 2. 项目结构

- 后端 Worker：`src/index.js`
- 管理台页面：
  - `webui/index.html`
  - `webui/app.js`
  - `webui/style.css`
- Worker 配置：`wrangler.toml`

---

## 3. 部署教程

下面给出从 0 到可用的完整部署步骤。

### 3.1 前置条件
1. Cloudflare 账号；
2. 已开通 Workers 与 D1；
3. 本地安装 Node.js 与 Wrangler。

安装 Wrangler：
```bash
npm i -g wrangler
```

登录 Cloudflare：
```bash
wrangler login
```

### 3.2 配置 `wrangler.toml`
请确认 `wrangler.toml` 中已配置：
- Worker 基本信息（`name`、`main` 等）；
- D1 绑定 `DB`（`[[d1_databases]]`）；
- 如使用 Dashboard 变量，建议保留 `keep_vars = true`。
- 如需自动清理过期 D1 缓存，配置 Cron 触发器（示例）：

```toml
[triggers]
crons = ["*/10 * * * *"]
```

> 重要：`DB` 绑定名必须与代码一致（`env.DB`）。

### 3.3 创建 D1 数据库与数据表

#### 方式 A：让服务自动建表（推荐）
Worker 启动时会执行 `CREATE TABLE IF NOT EXISTS`，一般不需要手动建表。

#### 方式 B：手动执行 SQL
可在 D1 控制台或 `wrangler d1 execute` 执行：

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

### 3.4 配置环境变量 / Secrets
至少建议配置：
- `ADMIN_TOKEN`：管理后台登录令牌；
- `GATEWAY_TOKEN`：翻译接口调用令牌；
- `ADMIN_COOKIE_SECRET`：管理员会话签名密钥；
- `ALLOW_ORIGIN`：允许跨域来源（可选）；
- `CACHE_TTL_SECONDS`：缓存秒数（默认 86400）。
- `CACHE_CLEANUP_BATCH_SIZE`：每轮 Cron 清理条数（默认 500）。
- `CACHE_CLEANUP_MAX_ROUNDS`：单次 Cron 最多清理轮数（默认 20）。

可用命令示例：
```bash
wrangler secret put ADMIN_TOKEN
wrangler secret put GATEWAY_TOKEN
wrangler secret put ADMIN_COOKIE_SECRET
```

### 3.5 部署
```bash
wrangler deploy
```

部署成功后，记下你的 Worker 域名，例如：
`https://your-worker.your-subdomain.workers.dev`

---

## 4. 使用说明

### 4.1 管理后台
- 打开：`https://<worker-domain>/webui/`
- 登录后可进行 key 管理与 usage 刷新。

### 4.2 翻译 API 调用
```bash
curl -X POST "https://<worker-domain>/v2/translate" \
  -H "Authorization: Bearer <GATEWAY_TOKEN>" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data "text=Hello world&target_lang=ZH"
```

### 4.3 跳过缓存
请求头加入：
```http
x-no-cache: 1
```

---

## 5. 常见问题

### 5.1 为什么部署后绑定或变量不见了？
Cloudflare 会以 `wrangler.toml` 为期望状态。若配置未写入文件，部署时可能被覆盖。

### 5.2 提示 `DB binding is not configured` 怎么办？
说明 `[[d1_databases]]` 未正确声明或绑定名不是 `DB`。

### 5.3 `/v2/usage` 返回不支持？
official 站点 key（`api.deepl.com`）不支持当前 usage 聚合逻辑，仅非 official key 可用。

---

## 6. 本地开发（可选）

```bash
wrangler dev
```

本地调试时同样需要配置 D1 绑定与必要变量。
