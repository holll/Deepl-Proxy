export default {
  async fetch(request, env, ctx) {
    try {
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

      // 以下路由依赖 D1
      if (!env.DB || typeof env.DB.prepare !== "function") {
        return withCors(json({ error: "DB binding is not configured. Please bind D1 as env.DB." }, 500), request, env);
      }
      await ensureSchema(env);

      // Admin key CRUD
      if (url.pathname === "/admin/keys" && request.method === "GET") return handleAdminKeys(request, env);
      if (url.pathname === "/admin/keys" && request.method === "POST") return handleAdminKeyCreate(request, env);
      if (url.pathname.startsWith("/admin/keys/") && request.method === "PUT") return handleAdminKeyUpdate(request, env, url.pathname.split("/").pop());
      if (url.pathname.startsWith("/admin/keys/") && request.method === "DELETE") return handleAdminKeyDelete(request, env, url.pathname.split("/").pop());
      if (url.pathname === "/admin/usage-refresh" && (request.method === "POST" || request.method === "GET")) return handleUsageRefresh(request, env);

      // DeepL compatible API
      if ((url.pathname === "/v2/translate" || url.pathname === "/translate") && request.method === "POST") return handleTranslateCompat(request, env, ctx);
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

  const originalUrl = new URL(request.url);
  const assetPath = originalUrl.pathname === "/webui" ? "/" : originalUrl.pathname.replace(/^\/webui/, "") || "/";
  const assetUrl = new URL(request.url);
  assetUrl.pathname = assetPath;

  const assetRequest = new Request(assetUrl.toString(), request);
  const response = await env.ASSETS.fetch(assetRequest);
  return withCors(response, request, env);
}

async function handleTranslateCompat(request, env, ctx) {
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
            "X-Key-Site-Type": detectProviderByEndpoint(key.endpoint),
          },
        });
        if (resp.ok && shouldSampleUsageRefresh(env)) {
          ctx.waitUntil(refreshUsageSnapshotSafely(env, key));
        }
        return withCors(passthrough, request, env);
      }

      const errorType = classifyDeepLError(resp.status, text, detectProviderByEndpoint(key.endpoint));
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

  const nonOfficial = keys.filter((k) => detectProviderByEndpoint(k.endpoint) !== "official");
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
  const rs = await env.DB.prepare("SELECT id,name,endpoint,status,disable_type,disabled_until,last_error_code,last_error_message,last_used_at,last_checked_at,character_count,character_limit,created_at,updated_at FROM deepl_keys ORDER BY id ASC").all();
  const keys = (rs.results || []).map((row) => ({
    ...row,
    disabled_until_iso: row.disabled_until ? new Date(row.disabled_until).toISOString() : null,
    last_used_at_iso: row.last_used_at ? new Date(row.last_used_at).toISOString() : null,
    last_checked_at_iso: row.last_checked_at ? new Date(row.last_checked_at).toISOString() : null,
    created_at_iso: row.created_at ? new Date(row.created_at).toISOString() : null,
    updated_at_iso: row.updated_at ? new Date(row.updated_at).toISOString() : null,
  }));
  return withCors(json({ keys }), request, env);
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
    .prepare("INSERT INTO deepl_keys (name,auth_key,endpoint,status,created_at,updated_at) VALUES (?,?,?, 'active',?,?)")
    .bind(
      String(body.name),
      String(body.auth_key),
      String(body.endpoint).replace(/\/$/, ""),
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
    .prepare("UPDATE deepl_keys SET name=COALESCE(?,name),auth_key=COALESCE(?,auth_key),endpoint=COALESCE(?,endpoint),status=COALESCE(?,status),updated_at=? WHERE id=?")
    .bind(
      body.name ?? null,
      body.auth_key ?? null,
      body.endpoint ? String(body.endpoint).replace(/\/$/, "") : null,
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

async function handleUsageRefresh(request, env) {
  const authError = await verifyAdminAuth(request, env);
  if (authError) return withCors(authError, request, env);
  const keys = await getActiveKeys(env);
  const results = [];
  for (const key of keys) {
    try {
      if (detectProviderByEndpoint(key.endpoint) === "official") {
        results.push({ key: key.name, ok: false, skipped: "official_usage_not_supported" });
        continue;
      }
      const usage = await fetchUsage(key);
      if (usage.ok && usage.data) {
        await updateUsageSnapshot(env, key.id, usage.data);
        results.push({ key: key.name, ok: true, data: usage.data });
      } else {
        results.push({ key: key.name, ok: false, status: usage.status, body: truncate(usage.text, 200) });
      }
    } catch (err) {
      results.push({ key: key.name, ok: false, error: safeErrorMessage(err) });
    }
  }
  return withCors(json({ results }), request, env);
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
  const ok = await isAdminAuthorized(request, env);
  return ok ? null : json({ error: "Unauthorized" }, 401);
}

async function handleAdminLogin(request, env) {
  const body = await request.json().catch(() => ({}));
  if (String(body?.admin_token || "") !== String(env.ADMIN_TOKEN || "")) {
    return withCors(json({ error: "Unauthorized" }, 401), request, env);
  }
  const headers = new Headers({ "Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store" });
  headers.append("Set-Cookie", await createAdminSessionCookie(env));
  return withCors(new Response(JSON.stringify({ ok: true, authenticated: true }), { status: 200, headers }), request, env);
}

async function handleAdminLogout(request, env) {
  const headers = new Headers({ "Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store" });
  headers.append("Set-Cookie", clearAdminSessionCookie());
  return withCors(new Response(JSON.stringify({ ok: true, authenticated: false }), { status: 200, headers }), request, env);
}

async function handleAdminSession(request, env) {
  const authenticated = await isAdminAuthorized(request, env);
  return withCors(json({ authenticated }), request, env);
}

async function isAdminAuthorized(request, env) {
  const auth = request.headers.get("Authorization") || "";
  if (auth === `Bearer ${env.ADMIN_TOKEN}`) return true;
  return verifyAdminSessionCookie(request, env);
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
  let data = null;
  try { data = JSON.parse(text || "null"); } catch {}
  return { ok: resp.ok, status: resp.status, data, text };
}

async function getActiveKeys(env) {
  const rs = await env.DB.prepare("SELECT * FROM deepl_keys WHERE status='active' ORDER BY id ASC").all();
  return rs.results || [];
}

async function updateUsageSnapshot(env, keyId, data) {
  const now = Date.now();
  await env.DB.prepare("UPDATE deepl_keys SET character_count=?, character_limit=?, last_checked_at=?, updated_at=? WHERE id=?")
    .bind(numberOrNull(data?.character_count), numberOrNull(data?.character_limit), now, now, keyId)
    .run();
}

function shouldSampleUsageRefresh(env) {
  const raw = Number(env.USAGE_REFRESH_SAMPLE_RATE ?? 0.05);
  const rate = Number.isFinite(raw) ? Math.max(0, Math.min(1, raw)) : 0.05;
  return Math.random() < rate;
}

async function refreshUsageSnapshotSafely(env, key) {
  try {
    if (detectProviderByEndpoint(key.endpoint) === "official") return;
    const usage = await fetchUsage(key);
    if (usage.ok && usage.data) await updateUsageSnapshot(env, key.id, usage.data);
  } catch {}
}

async function ensureSchema(env) {
  await env.DB
    .prepare("CREATE TABLE IF NOT EXISTS deepl_keys (id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT,auth_key TEXT NOT NULL,endpoint TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'active',disable_type TEXT,disabled_until INTEGER,last_error_code TEXT,last_error_message TEXT,character_count INTEGER,character_limit INTEGER,created_at INTEGER,updated_at INTEGER)")
    .run();
  const alters = [
    "ALTER TABLE deepl_keys ADD COLUMN last_used_at INTEGER",
    "ALTER TABLE deepl_keys ADD COLUMN last_checked_at INTEGER",
    "ALTER TABLE deepl_keys ADD COLUMN disabled_until INTEGER",
    "ALTER TABLE deepl_keys ADD COLUMN disable_type TEXT",
    "ALTER TABLE deepl_keys ADD COLUMN last_error_code TEXT",
    "ALTER TABLE deepl_keys ADD COLUMN last_error_message TEXT",
    "ALTER TABLE deepl_keys ADD COLUMN character_count INTEGER",
    "ALTER TABLE deepl_keys ADD COLUMN character_limit INTEGER",
    "ALTER TABLE deepl_keys ADD COLUMN created_at INTEGER",
    "ALTER TABLE deepl_keys ADD COLUMN updated_at INTEGER"
  ];
  for (const sql of alters) {
    await env.DB.prepare(sql).run().catch(() => {});
  }
  await env.DB.prepare("ALTER TABLE deepl_keys DROP COLUMN site_type").run().catch(() => {});
}

function detectProviderByEndpoint(endpoint) {
  const value = String(endpoint || "").toLowerCase();
  return value.includes("api.deepl.com") ? "official" : "deepl_pro";
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

async function createAdminSessionCookie(env) {
  const ttl = getAdminSessionTTLSeconds(env);
  const now = Date.now();
  const payload = encodeBase64UrlFromString(JSON.stringify({ iat: now, exp: now + ttl * 1000 }));
  const signature = await signAdminSessionPayload(payload, env);
  const value = `${payload}.${signature}`;
  return buildCookieString("admin_session", value, { path: "/", httpOnly: true, secure: true, sameSite: "Lax", maxAge: ttl });
}

function clearAdminSessionCookie() {
  return buildCookieString("admin_session", "", { path: "/", httpOnly: true, secure: true, sameSite: "Lax", maxAge: 0 });
}

async function verifyAdminSessionCookie(request, env) {
  try {
    const cookies = parseCookieHeader(request.headers.get("Cookie") || "");
    const raw = cookies.admin_session;
    if (!raw) return false;
    const dotIndex = raw.lastIndexOf(".");
    if (dotIndex <= 0) return false;
    const payload = raw.slice(0, dotIndex);
    const sig = raw.slice(dotIndex + 1);
    const expected = await signAdminSessionPayload(payload, env);
    if (sig !== expected) return false;
    const data = JSON.parse(decodeBase64UrlToString(payload));
    return typeof data?.exp === "number" && Date.now() < data.exp;
  } catch {
    return false;
  }
}

async function signAdminSessionPayload(payload, env) {
  const secret = String(env.ADMIN_COOKIE_SECRET || env.ADMIN_TOKEN || "");
  const key = await crypto.subtle.importKey("raw", new TextEncoder().encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const signature = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(payload));
  return bytesToHex(new Uint8Array(signature));
}

function getAdminSessionTTLSeconds(env) {
  const raw = Number(env.ADMIN_SESSION_TTL_SECONDS || 12 * 60 * 60);
  return Number.isFinite(raw) && raw > 0 ? Math.floor(raw) : 12 * 60 * 60;
}

function buildCookieString(name, value, options = {}) {
  const parts = [`${name}=${value}`];
  if (options.path) parts.push(`Path=${options.path}`);
  if (options.maxAge != null) parts.push(`Max-Age=${options.maxAge}`);
  if (options.httpOnly) parts.push("HttpOnly");
  if (options.secure) parts.push("Secure");
  if (options.sameSite) parts.push(`SameSite=${options.sameSite}`);
  return parts.join("; ");
}

function parseCookieHeader(cookieHeader) {
  const out = {};
  for (const pair of String(cookieHeader || "").split(";")) {
    const idx = pair.indexOf("=");
    if (idx <= 0) continue;
    out[pair.slice(0, idx).trim()] = pair.slice(idx + 1).trim();
  }
  return out;
}

function encodeBase64UrlFromString(input) {
  const bytes = new TextEncoder().encode(input);
  let binary = "";
  for (const b of bytes) binary += String.fromCharCode(b);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

function decodeBase64UrlToString(input) {
  let b64 = String(input || "").replace(/-/g, "+").replace(/_/g, "/");
  while (b64.length % 4) b64 += "=";
  const binary = atob(b64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return new TextDecoder().decode(bytes);
}

function bytesToHex(bytes) {
  return Array.from(bytes).map((b) => b.toString(16).padStart(2, "0")).join("");
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

function numberOrNull(v) {
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

function json(data, status = 200) {
  return new Response(JSON.stringify(data, null, 2), {
    status,
    headers: { "Content-Type": "application/json; charset=utf-8" },
  });
}
