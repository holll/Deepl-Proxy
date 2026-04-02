export default {
  async fetch(request, env) {
    try {
      await ensureSchema(env);
      const url = new URL(request.url);

      if (request.method === "OPTIONS") return handleOptions(request, env);

      // Frontend static assets (Cloudflare assets binding)
      if (url.pathname === "/") {
        return Response.redirect(new URL("/webui/", request.url).toString(), 302);
      }
      if (url.pathname.startsWith("/webui")) {
        return serveWebUI(request, env);
      }

      // Admin auth/session
      if (url.pathname === "/admin/login" && request.method === "POST") return handleAdminLogin(request, env);
      if (url.pathname === "/admin/logout" && request.method === "POST") return handleAdminLogout(request, env);
      if (url.pathname === "/admin/session" && request.method === "GET") return handleAdminSession(request, env);

      // Admin key CRUD
      if (url.pathname === "/admin/keys" && request.method === "GET") return handleAdminKeys(request, env);
      if (url.pathname === "/admin/keys" && request.method === "POST") return handleAdminKeyCreate(request, env);
      if (url.pathname.startsWith("/admin/keys/") && request.method === "PUT") return handleAdminKeyUpdate(request, env, url.pathname.split("/").pop());
      if (url.pathname.startsWith("/admin/keys/") && request.method === "DELETE") return handleAdminKeyDelete(request, env, url.pathname.split("/").pop());

      // DeepL compatible API
      if ((url.pathname === "/v2/translate" || url.pathname === "/translate") && request.method === "POST") return handleTranslateCompat(request, env);
      if (url.pathname === "/v2/usage" && request.method === "GET") return handleUsageCompat(request, env);

      if (url.pathname === "/health") return withCors(json({ ok: true, now: new Date().toISOString() }), request, env);
      return withCors(json({ error: "Not found" }, 404), request, env);
    } catch (err) {
      return withCors(json({ error: safeErrorMessage(err) }, 500), request, env);
    }
  },
};

async function serveWebUI(request, env) {
  if (!env.ASSETS || typeof env.ASSETS.fetch !== "function") {
    return withCors(json({ error: "ASSETS binding is not configured" }, 500), request, env);
  }
  const response = await env.ASSETS.fetch(request);
  return withCors(response, request, env);
}

async function handleTranslateCompat(request, env) {
  const authError = verifyGatewayAuthCompat(request, env);
  if (authError) return withCors(authError, request, env);

  const clientReq = await parseIncomingTranslateRequest(request);
  if (!clientReq.ok) return withCors(json({ message: clientReq.error }, clientReq.status || 400), request, env);

  const activeKeys = await getActiveKeys(env);
  if (!activeKeys.length) return withCors(json({ message: "No available keys" }, 503), request, env);

  let lastFailure = null;
  for (const key of activeKeys) {
    try {
      const resp = await fetch(`${key.endpoint}/v2/translate`, {
        method: "POST",
        headers: {
          Authorization: `DeepL-Auth-Key ${key.auth_key}`,
          "Content-Type": "application/x-www-form-urlencoded",
        },
        body: clientReq.form.toString(),
      });

      const text = await resp.text();
      if (resp.ok || resp.status === 400) {
        const passthrough = new Response(text, {
          status: resp.status,
          headers: {
            "Content-Type": resp.headers.get("Content-Type") || "application/json; charset=utf-8",
            "X-Upstream-Key-Name": key.name || "",
            "X-Key-Site-Type": key.site_type || "deepl_pro",
          },
        });
        return withCors(passthrough, request, env);
      }

      const errorType = classifyDeepLError(resp.status, text, key.site_type);
      await applyDisableByType(env, key.id, errorType, String(resp.status), text);
      lastFailure = { key: key.name, status: resp.status, body: truncate(text, 160) };
    } catch (err) {
      await disableKeyTemporary(env, key.id, "network_error", safeErrorMessage(err), 60000);
      lastFailure = { key: key.name, error: safeErrorMessage(err) };
    }
  }

  return withCors(json({ message: "All keys unavailable", lastFailure }, 502), request, env);
}

async function handleUsageCompat(request, env) {
  const authError = verifyGatewayAuthCompat(request, env);
  if (authError) return withCors(authError, request, env);

  const keys = await getActiveKeys(env);
  if (!keys.length) return withCors(json({ message: "No available keys" }, 503), request, env);

  const nonOfficial = keys.filter((k) => (k.site_type || "deepl_pro") !== "official");
  if (!nonOfficial.length) {
    return withCors(json({ message: "Usage API not supported for official site keys" }, 501), request, env);
  }

  for (const key of nonOfficial) {
    const usage = await fetchUsage(key);
    if (usage.ok && usage.data) return withCors(json(usage.data, 200), request, env);
  }
  return withCors(json({ message: "Usage query failed" }, 502), request, env);
}

async function handleAdminKeys(request, env) {
  const authError = await verifyAdminAuth(request, env);
  if (authError) return withCors(authError, request, env);
  const rs = await env.DB.prepare("SELECT id,name,endpoint,site_type,status,disable_type,character_count,character_limit,created_at,updated_at FROM deepl_keys ORDER BY id ASC").all();
  return withCors(json({ keys: rs.results || [] }), request, env);
}

async function handleAdminKeyCreate(request, env) {
  const authError = await verifyAdminAuth(request, env);
  if (authError) return withCors(authError, request, env);

  const body = await request.json();
  if (!body?.name || !body?.auth_key || !body?.endpoint) {
    return withCors(json({ error: "name/auth_key/endpoint required" }, 400), request, env);
  }

  const now = Date.now();
  await env.DB
    .prepare("INSERT INTO deepl_keys (name,auth_key,endpoint,site_type,status,created_at,updated_at) VALUES (?,?,?,?, 'active',?,?)")
    .bind(
      String(body.name),
      String(body.auth_key),
      String(body.endpoint).replace(/\/$/, ""),
      body.site_type === "official" ? "official" : "deepl_pro",
      now,
      now
    )
    .run();
  return withCors(json({ ok: true }, 201), request, env);
}

async function handleAdminKeyUpdate(request, env, id) {
  const authError = await verifyAdminAuth(request, env);
  if (authError) return withCors(authError, request, env);
  const body = await request.json();
  const now = Date.now();

  await env.DB
    .prepare("UPDATE deepl_keys SET name=COALESCE(?,name),auth_key=COALESCE(?,auth_key),endpoint=COALESCE(?,endpoint),site_type=COALESCE(?,site_type),status=COALESCE(?,status),updated_at=? WHERE id=?")
    .bind(
      body.name ?? null,
      body.auth_key ?? null,
      body.endpoint ? String(body.endpoint).replace(/\/$/, "") : null,
      body.site_type ?? null,
      body.status ?? null,
      now,
      id
    )
    .run();
  return withCors(json({ ok: true }), request, env);
}

async function handleAdminKeyDelete(request, env, id) {
  const authError = await verifyAdminAuth(request, env);
  if (authError) return withCors(authError, request, env);
  await env.DB.prepare("DELETE FROM deepl_keys WHERE id=?").bind(id).run();
  return withCors(json({ ok: true }), request, env);
}

function verifyGatewayAuthCompat(request, env) {
  if (!env.GATEWAY_TOKEN) return json({ message: "GATEWAY_TOKEN is not configured" }, 500);
  const auth = request.headers.get("Authorization") || "";
  return auth === `Bearer ${env.GATEWAY_TOKEN}` || auth === `DeepL-Auth-Key ${env.GATEWAY_TOKEN}`
    ? null
    : json({ message: "Authorization failed" }, 401);
}

async function verifyAdminAuth(request, env) {
  if (!env.ADMIN_TOKEN) return json({ error: "ADMIN_TOKEN is not configured" }, 500);
  const auth = request.headers.get("Authorization") || "";
  return auth === `Bearer ${env.ADMIN_TOKEN}` ? null : json({ error: "Unauthorized" }, 401);
}

async function handleAdminLogin(request, env) {
  const body = await request.json().catch(() => ({}));
  if (body.admin_token !== env.ADMIN_TOKEN) return withCors(json({ error: "Unauthorized" }, 401), request, env);
  return withCors(json({ ok: true, authenticated: true }, 200), request, env);
}

async function handleAdminLogout(request, env) {
  return withCors(json({ ok: true, authenticated: false }), request, env);
}

async function handleAdminSession(request, env) {
  return withCors(json({ authenticated: false }), request, env);
}

async function parseIncomingTranslateRequest(request) {
  const ct = (request.headers.get("Content-Type") || "").toLowerCase();
  if (ct.includes("application/json")) {
    const body = await request.json().catch(() => null);
    if (!body?.text || !body?.target_lang) return { ok: false, status: 400, error: "text and target_lang are required" };

    const form = new URLSearchParams();
    const texts = Array.isArray(body.text) ? body.text : [body.text];
    for (const text of texts) form.append("text", text);
    form.append("target_lang", String(body.target_lang).toUpperCase());
    return { ok: true, form };
  }

  const raw = await request.text();
  const form = new URLSearchParams(raw);
  if (!form.getAll("text").length || !form.get("target_lang")) {
    return { ok: false, status: 400, error: "text and target_lang are required" };
  }
  return { ok: true, form };
}

function classifyDeepLError(status, text, siteType) {
  const content = String(text || "").toLowerCase();
  if (status === 456 || content.includes("quota") || content.includes("limit")) {
    return siteType === "deepl_pro" ? "permanent" : "monthly";
  }
  if ([429, 500, 502, 503, 504].includes(status)) return "temporary";
  if ([401, 403].includes(status)) return "permanent";
  return "none";
}

async function applyDisableByType(env, keyId, type, errorCode, errorMessage) {
  if (type === "monthly") return disableKeyMonthly(env, keyId, errorCode, errorMessage);
  if (type === "temporary") return disableKeyTemporary(env, keyId, errorCode, errorMessage, 5 * 60 * 1000);
  if (type === "permanent") return disableKeyPermanent(env, keyId, errorCode, errorMessage);
  return null;
}

async function disableKeyMonthly(env, keyId, errorCode, errorMessage) {
  const now = Date.now();
  await env.DB
    .prepare("UPDATE deepl_keys SET status='disabled',disable_type='monthly',disabled_until=?,last_error_code=?,last_error_message=?,updated_at=? WHERE id=?")
    .bind(getStartOfNextMonthBeijing(new Date(now)), truncate(errorCode, 100), truncate(errorMessage, 500), now, keyId)
    .run();
}

async function disableKeyTemporary(env, keyId, errorCode, errorMessage, ms) {
  const now = Date.now();
  await env.DB
    .prepare("UPDATE deepl_keys SET status='disabled',disable_type='temporary',disabled_until=?,last_error_code=?,last_error_message=?,updated_at=? WHERE id=?")
    .bind(now + ms, truncate(errorCode, 100), truncate(errorMessage, 500), now, keyId)
    .run();
}

async function disableKeyPermanent(env, keyId, errorCode, errorMessage) {
  const now = Date.now();
  await env.DB
    .prepare("UPDATE deepl_keys SET status='dead',disable_type='permanent',disabled_until=NULL,last_error_code=?,last_error_message=?,updated_at=? WHERE id=?")
    .bind(truncate(errorCode, 100), truncate(errorMessage, 500), now, keyId)
    .run();
}

async function fetchUsage(key) {
  const resp = await fetch(`${key.endpoint}/v2/usage`, { headers: { Authorization: `DeepL-Auth-Key ${key.auth_key}` } });
  const text = await resp.text();
  return { ok: resp.ok, data: JSON.parse(text || "null"), text };
}

async function getActiveKeys(env) {
  const rs = await env.DB.prepare("SELECT * FROM deepl_keys WHERE status='active' ORDER BY id ASC").all();
  return rs.results || [];
}

async function ensureSchema(env) {
  await env.DB
    .prepare("CREATE TABLE IF NOT EXISTS deepl_keys (id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT,auth_key TEXT NOT NULL,endpoint TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'active',disable_type TEXT,disabled_until INTEGER,last_error_code TEXT,last_error_message TEXT,character_count INTEGER,character_limit INTEGER,created_at INTEGER,updated_at INTEGER)")
    .run();
  await env.DB.prepare("ALTER TABLE deepl_keys ADD COLUMN site_type TEXT DEFAULT 'deepl_pro'").run().catch(() => {});
}

function handleOptions(request, env) {
  const headers = new Headers();
  applyCorsHeaders(headers, request, env);
  headers.set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS");
  headers.set("Access-Control-Allow-Headers", "Authorization,Content-Type,x-no-cache");
  return new Response(null, { status: 204, headers });
}

function withCors(response, request, env) {
  const headers = new Headers(response.headers);
  applyCorsHeaders(headers, request, env);
  return new Response(response.body, { status: response.status, headers });
}

function applyCorsHeaders(headers, request, env) {
  const allowOrigin = env.ALLOW_ORIGIN || "*";
  const reqOrigin = request.headers.get("Origin");
  if (allowOrigin === "*") headers.set("Access-Control-Allow-Origin", "*");
  else if (reqOrigin && reqOrigin === allowOrigin) {
    headers.set("Access-Control-Allow-Origin", reqOrigin);
    headers.set("Access-Control-Allow-Credentials", "true");
  }
}

function getStartOfNextMonthBeijing(now = new Date()) {
  const beijingOffsetMs = 8 * 60 * 60 * 1000;
  const beijingNow = new Date(now.getTime() + beijingOffsetMs);
  return Date.UTC(beijingNow.getUTCFullYear(), beijingNow.getUTCMonth() + 1, 1, 0, 0, 0, 0) - beijingOffsetMs;
}

function truncate(text, max = 500) {
  const value = String(text || "");
  return value.length > max ? value.slice(0, max) : value;
}

function safeErrorMessage(err) {
  return err instanceof Error ? err.message : String(err || "Unknown error");
}

function json(data, status = 200) {
  return new Response(JSON.stringify(data, null, 2), {
    status,
    headers: { "Content-Type": "application/json; charset=utf-8" },
  });
}
