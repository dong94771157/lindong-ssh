// 琳东SSH管理软件 —— 零知识云同步自建端点（Cloudflare Worker 参考实现）
//
// 部署步骤：
//   1. 安装 wrangler：npm i -g wrangler && wrangler login
//   2. 创建 KV 命名空间：wrangler kv:namespace create SYNC_KV
//   3. 新建 wrangler.toml，内容如下（id 替换为上一步输出）：
//        name = "lindong-ssh-sync"
//        main = "sync-worker.js"
//        compatibility_date = "2024-06-01"
//        [[kv_namespaces]]
//        binding = "SYNC_KV"
//        id = "<你的KV命名空间ID>"
//        [triggers]
//        crons = ["0 3 * * *"]   # 每天 03:00 执行清理任务
//   4. 部署：wrangler deploy
//   5. 在软件「云同步」设置中填写端点：https://lindong-ssh-sync.<你的子域>.workers.dev
//
// 协议（极简 KV，零知识）：
//   PUT /v1/sync/snapshot   Authorization: Bearer aiss_xxx   Body: {"ts":123,"data":"<base64密文>"}
//   GET /v1/sync/snapshot   Authorization: Bearer aiss_xxx   → 200 {"ts":123,"data":"..."} / 404
//
// 安全说明：
//   - 服务端不保存明文 aiss_ 密钥，仅保存其 SHA-256 哈希作为 KV 键；
//   - 存储内容全程为客户端 AES-256-GCM 加密后的密文，服务端无法解密（零知识）；
//   - 每次访问刷新 lastAccess；Cron 每天清理连续 365 天未使用的账号数据。

const PREFIX = "lindong-ssh-sync:";
const MAX_BODY = 4 * 1024 * 1024; // 单账号快照上限 4MB
const EXPIRE_DAYS = 365;

async function accountKey(authHeader) {
  if (!authHeader || !authHeader.startsWith("Bearer aiss_")) return null;
  const token = authHeader.slice(7).trim();
  if (token.length < 12 || token.length > 128) return null;
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(token));
  const hex = [...new Uint8Array(digest)].map(b => b.toString(16).padStart(2, "0")).join("");
  return PREFIX + hex;
}

function jsonResp(obj, status = 200) {
  return new Response(JSON.stringify(obj), {
    status,
    headers: { "Content-Type": "application/json; charset=utf-8", "Access-Control-Allow-Origin": "*" },
  });
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (request.method === "OPTIONS") {
      return new Response(null, {
        headers: {
          "Access-Control-Allow-Origin": "*",
          "Access-Control-Allow-Methods": "GET,PUT,OPTIONS",
          "Access-Control-Allow-Headers": "Authorization,Content-Type",
        },
      });
    }
    if (url.pathname !== "/v1/sync/snapshot") return jsonResp({ error: "not found" }, 404);

    const key = await accountKey(request.headers.get("Authorization"));
    if (!key) return jsonResp({ error: "unauthorized" }, 401);

    if (request.method === "PUT") {
      const len = Number(request.headers.get("Content-Length") || 0);
      if (len > MAX_BODY) return jsonResp({ error: "payload too large" }, 413);
      let body;
      try {
        body = await request.json();
      } catch {
        return jsonResp({ error: "invalid json" }, 400);
      }
      if (typeof body.data !== "string" || !body.data || body.data.length > MAX_BODY) {
        return jsonResp({ error: "invalid data" }, 400);
      }
      const record = { ts: Number(body.ts) || Date.now() / 1000, data: body.data, lastAccess: Date.now() };
      await env.SYNC_KV.put(key, JSON.stringify(record));
      return jsonResp({ ok: true, ts: record.ts });
    }

    if (request.method === "GET") {
      const raw = await env.SYNC_KV.get(key);
      if (!raw) return jsonResp({ error: "no data" }, 404);
      const record = JSON.parse(raw);
      // 刷新活跃时间（异步，不阻塞响应）
      record.lastAccess = Date.now();
      await env.SYNC_KV.put(key, JSON.stringify(record));
      return jsonResp({ ts: record.ts, data: record.data });
    }

    return jsonResp({ error: "method not allowed" }, 405);
  },

  // Cron Trigger：清理连续 EXPIRE_DAYS 天未使用的账号数据
  async scheduled(event, env) {
    const cutoff = Date.now() - EXPIRE_DAYS * 24 * 3600 * 1000;
    let cursor;
    do {
      const page = await env.SYNC_KV.list({ prefix: PREFIX, cursor });
      for (const k of page.keys) {
        const raw = await env.SYNC_KV.get(k.name);
        if (!raw) continue;
        try {
          const record = JSON.parse(raw);
          if (!record.lastAccess || record.lastAccess < cutoff) {
            await env.SYNC_KV.delete(k.name);
          }
        } catch {
          await env.SYNC_KV.delete(k.name);
        }
      }
      cursor = page.list_complete ? undefined : page.cursor;
    } while (cursor);
  },
};
