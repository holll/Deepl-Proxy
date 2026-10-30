# Deepl Proxy

一个基于 **Go + SQLite** 的 DeepL 代理服务，提供：
- 多 Key 轮询与自动熔断；
- DeepL 兼容翻译接口；
- **DeepLX 支持**（自动转换响应格式）；
- 管理后台（Web UI，嵌入二进制）；
- SQLite 翻译缓存（配合外层 CDN 使用）；
- 简单会话登录与管理 API。

---

## 1. 软件功能

### 1.1 DeepL 兼容 API
- `POST /v2/translate`（兼容 `/translate`）
  - 转发到上游 DeepL / deepl-pro / DeepLX 站点；
  - 支持 JSON 和 form-urlencoded 两种请求格式；
  - 支持常见参数（`text`、`target_lang`、`source_lang`、`formality` 等）。
- `GET /v2/usage`
  - 对非 official 站点 key 提供用量查询聚合（DeepLX 不支持）。

### 1.2 多 Key 管理与自动容错
- Key 信息存储在 SQLite 的 `deepl_keys` 表，服务启动时自动建表；
- 支持三种 provider：`deepl`（DeepL 官方/deepl-pro）、`deeplx`（DeepLX 自建服务）；
- 根据错误类型自动熔断：
  - `temporary` — 5 分钟禁用（429 / 5xx）；
  - `monthly` — 到北京时间下月 1 日（官方站点月配额耗尽）；
  - `permanent` — 永久标记 `dead`（401 / 403 / deepl-pro 配额耗尽）；
  - `network_error` — 1 分钟禁用（连接失败）。
- 可通过管理 API 新增、修改、删除 key。

### 1.3 DeepLX 支持
- Key 的 `provider` 设为 `deeplx` 时，翻译请求自动以 JSON 格式发往 DeepLX 的 `/translate` 端点；
- DeepLX 响应自动转换为 DeepL 兼容格式返回客户端；
- DeepLX 无需 auth_key，endpoint 填写服务地址即可（如 `http://127.0.0.1:1188`）。

### 1.4 管理后台（Web UI）
- 路径：`/webui/`（静态文件已嵌入二进制，无需额外部署）；
- 支持管理员登录、查看与维护 key、切换 provider、刷新 usage 快照。

### 1.5 翻译缓存
- 后端使用 SQLite 存储翻译结果；
- 响应自动附加 `Cache-Control: public, max-age=N`，外层 CDN 可直接缓存；
- 响应头 `X-Cache-Status`：`HIT`（SQLite 命中）/ `MISS`（上游翻译）；
- 缓存数据不保存 `X-Upstream-Key-Name`，避免暴露上游 key 标识；
- 过期缓存由后台 goroutine 定时清理，不影响实时请求；
- **击穿保护**：同一份文本的并发未命中会被合并成一次上游调用，其余请求等待并复用结果；等待方仍可被自己的客户端取消（立即返回 499，不陪跑）；
- **升级兼容**：缓存键格式变更过（新增 provider 维度），新版启动后会先按新键查，未命中的再按旧键查一次，命中就地迁移到新键。因此升级不会导致既有缓存失效，旧行会按自己的过期时间自然回收，回滚到旧版本也仍能命中。旧行全部过期后可把 `cache.legacy_key_disabled` 设为 `true` 关掉这次额外查询。

---

## 2. 项目结构

```
├── main.go                  # 入口：配置加载、数据库初始化、路由注册、启动
├── main_test.go             # 端到端测试：真实 HTTP 路由 + 本地假上游
├── go.mod / go.sum          # Go module 依赖
├── config.yaml              # 配置文件（YAML）
├── config/
│   └── config.go            # 配置结构体与加载逻辑
├── models/
│   └── models.go            # 数据类型定义
├── database/
│   ├── database.go          # SQLite 初始化、建表迁移、CRUD
│   └── database_test.go     # 缓存 CRUD / CAS 续期 / 过期清理
├── provider/
│   ├── provider.go          # Provider 接口定义 + 错误分类
│   ├── deepl.go             # DeepL 上游（form-urlencoded）
│   ├── deeplx.go            # DeepLX 上游（JSON，响应转 DeepL 兼容）
│   └── provider_test.go     # 响应体上限 / context 取消 / header 隔离
├── service/
│   ├── keyring.go           # Key 轮询、熔断、用量采样
│   ├── cache.go             # 缓存服务 + CDN 友好响应头
│   ├── legacykey.go         # v1 缓存键复刻（升级兼容读取）
│   ├── flight.go            # 并发请求合并（防缓存击穿）
│   ├── cachekey_test.go     # 缓存键构造与 provider 可服务性判断
│   ├── legacykey_test.go    # v1 键与历史版本逐字节对齐
│   └── flight_test.go       # 合并行为 / ctx 取消 / panic 释放等待方
├── handler/
│   ├── middleware.go        # CORS / Gateway Auth / Admin Auth (HMAC)
│   ├── translate.go         # 翻译 API + 路由注册
│   ├── admin.go             # 管理后台 API
│   └── middleware_test.go   # 请求 ID 清洗 / 鉴权中间件
└── webui/                   # 管理后台前端（嵌入二进制）
    ├── index.html
    ├── app.js
    └── style.css
```

---

## 3. 快速开始

### 3.1 前置条件
- Go 1.27+
- SQLite 无需额外安装（使用纯 Go 驱动 `modernc.org/sqlite`）

### 3.2 编译

```bash
# 如在国内，设置 Go 代理
set GOPROXY=https://goproxy.cn,direct

go build -o deepl-proxy .
```

### 3.3 测试

```bash
go test ./...
```

测试覆盖六块：缓存键构造（provider 维度、参数完整性、文本不被归一化）、v1 旧键兼容（与历史实现逐字节对齐、就地迁移、条数校验）、并发合并（击穿保护、等待方 ctx 取消、不合并 `x-no-cache`）、SQLite 缓存层（CAS 续期、过期清理、`created_at` 保留）、上游 provider（响应体上限、context 取消、错误响应头隔离）、以及通过真实 HTTP 路由的端到端行为（provider 切换不串缓存、`ttl_days=0` 不读旧行、请求头健壮性）。端到端用例使用本地假上游，不需要真实的 DeepL Key，也不会产生调用费用。

### 3.4 配置

编辑 `config.yaml` 或通过环境变量覆盖：

**config.yaml：**
```yaml
server:
  port: "8080"
  allow_origin: "*"

auth:
  admin_token: "your-admin-token"
  gateway_token: "your-gateway-token"
  admin_cookie_secret: "random-secret-string"
  session_ttl: 43200        # 管理员会话有效期（秒），默认 12 小时

database:
  path: "./data/deepl-proxy.db"

cache:
  ttl_days: 1               # 翻译缓存 TTL（天），0 表示不缓存
  cleanup_interval: 600     # 过期缓存清理间隔（秒）
  cleanup_batch_size: 500
  cleanup_max_rounds: 20
```

**环境变量**（优先级高于 config.yaml）：

| 变量 | 对应配置 | 必填 |
|---|---|---|
| `GATEWAY_TOKEN` | `auth.gateway_token` | 是 |
| `ADMIN_TOKEN` | `auth.admin_token` | 是 |
| `ADMIN_COOKIE_SECRET` | `auth.admin_cookie_secret` | 推荐 |
| `ALLOW_ORIGIN` | `server.allow_origin` | 否 |
| `PORT` | `server.port` | 否 |

### 3.5 运行

```bash
deepl-proxy.exe
# 输出: DeepL Proxy starting on :8080
```

数据库文件 `data/deepl-proxy.db` 和所需表结构自动创建，无需手动建表。

### 3.6 外层 CDN 配置建议

Go 服务返回的翻译响应自带 `Cache-Control: public, max-age=86400`。在外层 CDN（如 Cloudflare、Nginx）上配置：
- 允许缓存 POST 请求的响应；
- 缓存键包含请求体哈希（可根据 CDN 类型配置 Cache Key）；
- 对 `/webui/*` 静态文件设置较长的浏览器缓存时间。

---

## 4. 使用说明

### 4.1 管理后台
- 打开：`http://<host>:8080/webui/`
- 输入 `ADMIN_TOKEN` 登录；
- 新增 Key 时可选择 Provider（`deepl` / `deeplx`）。

### 4.2 翻译 API 调用

JSON 格式：
```bash
curl -X POST "http://localhost:8080/v2/translate" \
  -H "Authorization: Bearer <GATEWAY_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"text": ["Hello world"], "target_lang": "ZH"}'
```

form-urlencoded 格式：
```bash
curl -X POST "http://localhost:8080/v2/translate" \
  -H "Authorization: DeepL-Auth-Key <GATEWAY_TOKEN>" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data "text=Hello world&target_lang=ZH"
```

响应示例：
```json
{
  "translations": [
    {
      "detected_source_language": "EN",
      "text": "你好世界"
    }
  ]
}
```

### 4.3 跳过缓存
请求头加入：
```http
x-no-cache: 1
```

### 4.4 响应头说明

| 头 | 值 | 说明 |
|---|---|---|
| `X-Cache-Status` | `HIT` / `MISS` | 是否命中 SQLite 缓存 |
| `Cache-Control` | `public, max-age=86400` | CDN 可缓存此响应 |
| `X-Upstream-Key-Name` | key 名称 | 实际使用的上游 key（仅 MISS 时出现） |
| `X-Key-Site-Type` | `official` / `deepl_pro` / `deeplx` | 上游站点类型 |

---

## 5. Provider 说明

| Provider | 上游地址示例 | Auth Key | Usage API | 说明 |
|---|---|---|---|---|
| `deepl` | `https://api.deepl.com` | 必填 | 支持（非 official） | DeepL 官方 API 或 deepl-pro |
| `deepl` | `https://api.deepl-pro.com` | 必填 | 支持 | deepl-pro 第三方服务 |
| `deeplx` | `http://127.0.0.1:1188` | 可选 | 不支持 | 自建 DeepLX 服务 |

---

## 6. 常见问题

### 6.1 编译报错 "GOCACHE is not defined"
设置 Go 缓存目录：
```bash
mkdir .gocache
set GOCACHE=./.gocache
```

### 6.2 `/v2/usage` 返回不支持？
- official 站点 key（`api.deepl.com`）和 deeplx 均不支持用量查询。

### 6.3 DeepLX 翻译返回的不是 DeepL 格式？
服务端已自动转换，客户端始终收到 `{"translations": [...]}` 格式。

### 6.4 如何重置数据库？
删除 `data/deepl-proxy.db`，重启服务会自动重建。

### 6.5 升级后缓存会失效吗？
不会。缓存键升级为带 provider 维度的新格式后，读取路径保留了旧键兼容：新键未命中时按旧键再查一次，命中就把它迁移写入新键。代价是迁移期内每个旧条目会多一次索引查询，并且第一次命中后会多一次写入。

两类请求不会走旧键兼容：带了 `custom_instructions` / `tag_handling_version` / `show_billed_characters` 的请求（旧版本会静默丢弃这些参数，旧行不能代表它们的语义），以及译文条数与本次请求不符的旧行（旧键不含 provider 维度，`deeplx` 产出的行只有一条译文，回放给多 text 请求会是结构错误的响应）。
