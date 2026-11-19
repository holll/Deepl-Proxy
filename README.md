# Deepl Proxy

一个基于 **Go + SQLite** 的 DeepL 代理服务，提供：
- 多 Key 轮询与自动熔断；
- DeepL 兼容翻译接口；
- **DeepLX 支持**（自动转换响应格式）；
- 管理后台（Web UI，嵌入二进制）；
- SQLite 翻译缓存（配合外层 CDN 使用）；
- Key 失效 / 配额耗尽 / 无可用 Key 的 Webhook 通知（通用 JSON、MeoW、纯文本、GET 模板）；
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
- 支持两种 provider：`deepl`（DeepL 官方 / deepl-pro）、`deeplx`（DeepLX 自建服务）；
- **Key 后缀自动识别站点**：新增时 Endpoint 选“自动识别”，以 `:fx` 结尾的 key 走 `api-free.deepl.com`，以 `:dp` 结尾的 key 走 `api.deepl-pro.com`，其余走 `api.deepl.com`；显式选择 Endpoint 时以选择为准；
- 根据错误类型自动熔断：
  - `temporary` — 429 按上游 `Retry-After` 冷却（秒数或 HTTP 日期）；响应头缺失或无效，以及 5xx，仍冷却 5 分钟；
  - `quota` — 配额耗尽，按配额周期停到重置时刻：每年 100 万的 key 停到次年 1 月 1 日，每月 50 万的 key 停到北京时间下月 1 日；
  - `permanent` — 永久标记 `dead`（401 / 403 / deepl-pro 配额耗尽）；
  - `network_error` — 1 分钟禁用（连接失败）。
- 配额周期不是独立配置负担：默认 `auto` 按用量上限推断（≥100 万视为每年，否则每月），也可以在新增/编辑时强制指定 `yearly` / `monthly`；
- 可通过管理 API 新增、修改、删除、测试 key。

### 1.3 DeepLX 支持
- Key 的 `provider` 设为 `deeplx` 时，翻译请求自动以 JSON 格式发往 DeepLX 的 `/translate` 端点；
- DeepLX 响应自动转换为 DeepL 兼容格式返回客户端；
- DeepLX 无需 auth_key，endpoint 填写服务地址即可（如 `http://127.0.0.1:1188`）。

### 1.4 管理后台（Web UI）
- 路径：`/webui/`（静态文件已嵌入二进制，无需额外部署）；
- 以 **Key 管理** 为主：站点/Key 提示/状态/用量进度/重置时间一屏可见，支持连通性测试、启用停用、删除、行内改名称与 Endpoint；
- **翻译记录默认收起**，只显示“最新记录：几分钟前”用于确认服务在跑；展开后才加载分页列表。

### 1.5 通知（Webhook）
- 触发事件：`key_disabled`（Key 被永久熔断）、`quota_exhausted`（配额耗尽）、`key_temporarily_disabled`（临时故障，默认不发）、`no_keys`（没有任何可用 Key）；
- 默认 `generic`：POST JSON `{service,event,level,title,message,data,timestamp}`，可直接接 n8n、Bark 网关、自建机器人等；
- `meow`：兼容 [MeoW](https://www.chuckfang.com/MeoW/api_doc.html) 的 `POST /{昵称}/{标题}`，配置昵称即可，不绑定该服务；
- `text` / `get`：纯文本 POST 或 URL 模板 GET（`{event}` `{title}` `{msg}` `{level}` `{data}` 占位符）；
- 同一 Key 的同类通知默认 10 分钟内只发一次，队列满时丢弃并记日志，绝不阻塞翻译请求。

### 1.6 翻译缓存
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
├── config/
│   └── config.go            # 内置默认值 + 启动环境变量 + 旧配置一次性迁移
├── models/
│   └── models.go            # 数据类型定义
├── database/
│   ├── database.go          # SQLite 初始化、建表迁移、CRUD
│   └── database_test.go     # 缓存 CRUD / CAS 续期 / 过期清理
├── provider/
│   ├── provider.go          # Provider 接口定义 + 错误分类
│   ├── deepl.go             # DeepL 上游（form-urlencoded）
│   ├── deeplx.go            # DeepLX 上游（JSON，响应转 DeepL 兼容）
│   ├── keydetect.go         # Key 后缀 → 站点自动识别（:fx / :dp）
│   └── provider_test.go     # 响应体上限 / context 取消 / header 隔离
├── service/
│   ├── keyring.go           # Key 轮询、熔断、连通性测试、用量采样
│   ├── quota.go             # 配额周期推断与重置时刻（每年/每月）
│   ├── settings.go          # 运行期设置（数据库为唯一来源，热更新通知）
│   ├── notify.go            # Webhook 通知（节流、重试、多种格式）
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
│   ├── admin_settings.go    # 设置读写 / 首次初始化 / 会话密钥轮换
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

**不再需要 `config.yaml`。** 管理后台的「设置」对话框维护认证、缓存和 Webhook 配置，并保存在数据库的 `app_settings` 表里。缓存天数、清理参数和 v1 旧键兼容读取开关需要重启进程，认证和 Webhook 设置保存后生效。监听端口、CORS 和数据库路径不再由管理后台或 `/admin/settings` 接口修改或展示；以前迁入库的服务配置不受影响。

首次启动（数据库里还没有管理员令牌）时会进入初始化页：设置管理员令牌（≥8 位）与可选的网关令牌后直接进入控制台。

只有下面两项需要在启动前确定（可用命令行参数覆盖，见 3.5；它们决定进程如何启动/打开哪个库）：

| 变量 | 说明 | 默认 |
|---|---|---|
| `PORT` | 监听端口 | `8080` |
| `DB_PATH` | SQLite 数据库路径 | `./data/deepl-proxy.db` |

以下环境变量仅作为**首次迁移**的初始值（数据库里已有设置时不再生效）：`ADMIN_TOKEN`、`GATEWAY_TOKEN`、`ADMIN_COOKIE_SECRET`、`ALLOW_ORIGIN`、`WEBHOOK_ENABLED`、`WEBHOOK_URL`、`WEBHOOK_FORMAT`。

设置页包含三组：

- **认证与会话**：管理员令牌（留空表示不修改）、网关令牌、会话有效期、轮换会话密钥（会让已登录会话全部失效）；
- **缓存**：缓存天数、清理间隔、单批条数、最大轮数、停用 v1 旧键兼容读取；
- **通知（Webhook）**：开关、格式（generic / meow / text / get）、URL、MeoW 昵称、标题前缀、超时、同事件最小间隔、三类事件开关，并可在**保存前**直接「发送测试通知」。

> 从旧版本升级：先备份原数据库和 `config.yaml`。若旧配置自定义了 `database.path`，首次启动新版前请用 `-db-path` 或 `DB_PATH` 指向原数据库；数据库路径在配置迁移前决定。确认启动日志显示的是原库路径。当前目录仍有 `config.yaml` 时，首次启动会尝试将旧设置一次性迁入数据库；确认迁移成功后再移走旧文件。

### 3.5 运行

```bash
./deepl-proxy                                   # 默认 :8080，自动找库
./deepl-proxy -port 9000                        # 只改端口
./deepl-proxy -port 9000 -db-path /var/lib/deepl/proxy.db
./deepl-proxy --help                            # 用法与全部参数
```

参数优先级：**命令行 > 环境变量（`PORT` / `DB_PATH`）> 内置默认值**。

不传 `-db-path` 且没有 `DB_PATH` 时，按下面的顺序找库（只认已存在的文件）：

1. 当前目录 `data/deepl-proxy.db`
2. 当前目录 `deepl-proxy.db`
3. 程序所在目录 `data/deepl-proxy.db`
4. 程序所在目录 `deepl-proxy.db`
5. 都没有 → 在当前目录 `data/deepl-proxy.db` 新建

启动日志会打印最终路径和来源，例如：

```
数据库: /opt/deepl-proxy/data/deepl-proxy.db（来源：程序目录）
DeepL Proxy starting on :8080
```

这样把二进制放到任意目录、双击/直接运行时，会优先复用同目录已有的库，而不是在工作目录里再造一个空库。数据库文件和表结构自动创建，无需手动建表。

#### 忘记后台密码

不需要改库、也不需要能登录，直接在本机执行：

```bash
./deepl-proxy reset-admin                          # 生成随机令牌并打印（只显示一次）
./deepl-proxy reset-admin -token 'my-new-token'    # 指定新令牌（≥8 位）
echo 'my-new-token' | ./deepl-proxy reset-admin -stdin   # 从标准输入读，避免进历史/进程列表
./deepl-proxy reset-admin -db-path /opt/deepl/proxy.db    # 库不在自动查找路径时显式指定
```

它会写入数据库并**同时轮换会话密钥**，所以旧令牌立即失效、所有已登录会话被踢下线；网关令牌与其它设置不受影响。

### 3.6 外层 CDN 配置建议

Go 服务返回的翻译响应自带 `Cache-Control: public, max-age=86400`。在外层 CDN（如 Cloudflare、Nginx）上配置：
- 允许缓存 POST 请求的响应；
- 缓存键包含请求体哈希（可根据 CDN 类型配置 Cache Key）；
- 对 `/webui/*` 静态文件设置较长的浏览器缓存时间。

---

## 4. 使用说明

### 4.1 管理后台
- 打开：`http://<host>:8080/webui/`
- 首次启动按提示设置管理员令牌；之后用该令牌登录（令牌存在数据库里，可在「设置」中修改）；
- 新增 Key 时 Endpoint 默认“自动识别（按 Key 后缀）”，只填 Name 与 Auth Key 即可；`deeplx` 需要显式给 Endpoint；
- Key 列表可直接测试连通性（发一条 `ping`，不消耗配额快照、不写缓存）、启用/停用、删除，并可看到站点、Key 提示、用量进度与配额周期；
- 翻译记录默认收起：折叠标题栏显示“最新记录：N 分钟前”，展开后才加载分页列表并支持逐条删除；不提供关键词搜索（`GET /admin/cache` 忽略旧客户端的 `search` 参数，`total` 为全部条目数，包括尚未清理的过期条目）。

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

| Provider | Key 后缀 | 自动识别的上游 | Usage API | 说明 |
|---|---|---|---|---|
| `deepl` | 其它 | `https://api.deepl.com` | 支持 | DeepL 官方付费/开发者计划 |
| `deepl` | `:fx` | `https://api-free.deepl.com` | 支持 | 官方免费计划 |
| `deepl` | `:dp` | `https://api.deepl-pro.com` | 支持 | deepl-pro 第三方服务 |
| `deeplx` | 不适用 | 显式填写的 Endpoint | 不支持 | 自建 DeepLX 服务 |

免费计划的配额口径有两种：新 key 为每年 100 万字符，旧 key 为每月 50 万字符。服务按用量上限自动区分（≥100 万按年重置，否则按月重置），也可以在 Key 上手写 `yearly` / `monthly` 覆盖。

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

### 6.5 配额耗尽后 key 什么时候恢复？
按配额周期自动恢复：每年 100 万的 key 在次年 1 月 1 日 00:00（北京时间）恢复，每月 50 万的 key 在下月 1 日 00:00 恢复，服务启动时会自动把到期的 key 重新置为可用。管理后台的“截至”列会显示剩余时间。

### 6.6 换了 Endpoint 或配额周期，缓存会失效吗？
不会。缓存键只包含文本、语言、formality、glossary、context、model_type、provider class 等影响译文内容的字段，Endpoint（`api.deepl.com` / `api-free.deepl.com` / `api.deepl-pro.com`）与配额周期都不参与键的构造，修改它们不会改变已有行的可读性。

### 6.7 升级后缓存会失效吗？
不会。缓存键升级为带 provider 维度的新格式后，读取路径保留了旧键兼容：新键未命中时按旧键再查一次，命中就把它迁移写入新键。代价是迁移期内每个旧条目会多一次索引查询，并且第一次命中后会多一次写入。

两类请求不会走旧键兼容：带了 `custom_instructions` / `tag_handling_version` / `show_billed_characters` 的请求（旧版本会静默丢弃这些参数，旧行不能代表它们的语义），以及译文条数与本次请求不符的旧行（旧键不含 provider 维度，`deeplx` 产出的行只有一条译文，回放给多 text 请求会是结构错误的响应）。
