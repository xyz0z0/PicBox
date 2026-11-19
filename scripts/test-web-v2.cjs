#!/usr/bin/env node
/*
 * PicBox Go 新版：原版 React 静态壳 + 原 UI 实际调用面的 HTTP 回归。
 *
 * 每次运行都自己创建临时数据目录、自己构建/启动环回端口上的二进制、结束后删除
 * 全部临时数据与临时服务，不接触 data、现有服务或生产数据库。
 *
 * 用法：
 *   node scripts/test-web-v2.cjs                # 临时构建并启动
 *   PICBOX_BIN=/path/to/picbox node scripts/test-web-v2.cjs
 *
 * 覆盖：
 *   - 静态壳：内嵌 dist/index.html、index.html 实际引用的 /assets/* 可取、
 *     CSP/安全头/ETag/304、页面 HEAD、非法资源名与未知 API 仍是 JSON 404；
 *     未知页面路径返回 404 + 同一份 404 页壳，已注册页面尾斜杠 308 且保留
 *     query，图片详情初始文档状态跟随 /meta 可见性；写方法仍保持 405 + Allow。
 *     断言壳里不再出现已删除的简化 vanilla 页标记。
 *   - 运行时配置：/api/config 的 uploadMaxBytes 与 mailEnabled（本脚本不配置 SMTP，
 *     因此必须是 false），且不暴露任何 SMTP 主机/账号/密码字段。
 *   - 原 UI 实际 API 全闭环：管理员生成激活码 → 用户凭码注册 →
 *     当前会话/其他旧会话/API Key 语义（change-password 重签 Cookie、保留 Key）、
 *     批量操作与 usage/tags、单图 meta 可见性、导出 HEAD + 真实 ZIP（解析中央目录与
 *     manifest.json）、用户配额/禁用/启用与管理审计。
 *
 * 边界：本脚本只验证静态壳与 HTTP 契约。它**不执行浏览器 JavaScript**，也不模拟点击，
 * 因此通过它不代表 UI 交互（弹窗、导航、窄屏、暗色）已经验收；那必须有真实浏览器证据。
 */
"use strict";

const { spawn, spawnSync } = require("node:child_process");
const fs = require("node:fs");
const net = require("node:net");
const os = require("node:os");
const path = require("node:path");
const zlib = require("node:zlib");

const serverDir = path.resolve(__dirname, "..");
const PNG_1X1 = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
  "base64"
);

let failures = 0;

function check(condition, message, detail) {
  if (condition) {
    console.log("  ok   " + message);
    return true;
  }
  failures += 1;
  console.error("  FAIL " + message + (detail === undefined ? "" : " :: " + detail));
  return false;
}

function freePort() {
  return new Promise((resolve, reject) => {
    const probe = net.createServer();
    probe.once("error", reject);
    probe.listen(0, "127.0.0.1", () => {
      const { port } = probe.address();
      probe.close(() => resolve(port));
    });
  });
}

async function waitForHealth(baseUrl, deadline) {
  let lastError = "";
  while (Date.now() < deadline) {
    try {
      const response = await fetch(baseUrl + "/api/health");
      if (response.status === 200) {
        return;
      }
      lastError = "HTTP " + response.status;
    } catch (error) {
      lastError = String(error && error.message ? error.message : error);
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error("服务未在期限内就绪：" + lastError);
}

function errorCode(payload) {
  if (!payload || typeof payload !== "object") {
    return "";
  }
  if (payload.error && typeof payload.error === "object" && payload.error.code) {
    return String(payload.error.code);
  }
  return typeof payload.code === "string" ? payload.code : "";
}

// readZipEntries 只用 Node 标准库解析 ZIP 中央目录，不依赖第三方包。
// 归档可能对 README/manifest 使用 deflate，原图使用 store，两种都支持。
function readZipEntries(buffer) {
  const EOCD = 0x06054b50;
  const CENTRAL = 0x02014b50;
  const LOCAL = 0x04034b50;
  let eocd = -1;
  for (let i = buffer.length - 22; i >= 0; i -= 1) {
    if (buffer.readUInt32LE(i) === EOCD) {
      eocd = i;
      break;
    }
  }
  if (eocd < 0) {
    throw new Error("ZIP 中央目录结束记录未找到");
  }
  const entryCount = buffer.readUInt16LE(eocd + 10);
  let offset = buffer.readUInt32LE(eocd + 16);
  const entries = new Map();
  for (let index = 0; index < entryCount; index += 1) {
    if (offset + 46 > buffer.length || buffer.readUInt32LE(offset) !== CENTRAL) {
      throw new Error("ZIP 中央目录条目损坏（index=" + index + "）");
    }
    const method = buffer.readUInt16LE(offset + 10);
    const compressedSize = buffer.readUInt32LE(offset + 20);
    const nameLength = buffer.readUInt16LE(offset + 28);
    const extraLength = buffer.readUInt16LE(offset + 30);
    const commentLength = buffer.readUInt16LE(offset + 32);
    const localOffset = buffer.readUInt32LE(offset + 42);
    const name = buffer.subarray(offset + 46, offset + 46 + nameLength).toString("utf8");
    if (buffer.readUInt32LE(localOffset) !== LOCAL) {
      throw new Error("ZIP 本地文件头损坏：" + name);
    }
    const localNameLength = buffer.readUInt16LE(localOffset + 26);
    const localExtraLength = buffer.readUInt16LE(localOffset + 28);
    const dataStart = localOffset + 30 + localNameLength + localExtraLength;
    const raw = buffer.subarray(dataStart, dataStart + compressedSize);
    if (method === 0) {
      entries.set(name, Buffer.from(raw));
    } else if (method === 8) {
      entries.set(name, zlib.inflateRawSync(raw));
    } else {
      throw new Error("不支持的 ZIP 压缩方法 " + method + "：" + name);
    }
    offset += 46 + nameLength + extraLength + commentLength;
  }
  return entries;
}

function main() {
  const tmpRoot = fs.mkdtempSync(path.join(os.tmpdir(), "picbox-web-v2-"));
  const dataDir = path.join(tmpRoot, "data");
  let child = null;
  let baseUrl = "";
  let serverLog = "";

  // 每个 client 一份独立 Cookie jar，用来区分「当前会话」「其他旧会话」「不同账号」。
  function createClient() {
    const jar = new Map();

    function cookieHeader() {
      return Array.from(jar, ([name, value]) => name + "=" + value).join("; ");
    }

    function storeCookies(headers) {
      const list = typeof headers.getSetCookie === "function" ? headers.getSetCookie() : [];
      for (const raw of list) {
        const pair = String(raw).split(";")[0];
        const eq = pair.indexOf("=");
        if (eq > 0) {
          jar.set(pair.slice(0, eq).trim(), pair.slice(eq + 1).trim());
        }
      }
    }

    async function request(method, url, options) {
      const opts = options || {};
      const headers = Object.assign({}, opts.headers || {});
      if (jar.size > 0) {
        headers.cookie = cookieHeader();
      }
      if (opts.bearer) {
        headers.authorization = "Bearer " + opts.bearer;
      }
      let payload = opts.body;
      if (payload && !(payload instanceof FormData) && typeof payload !== "string" &&
        !Buffer.isBuffer(payload)) {
        headers["content-type"] = "application/json";
        payload = JSON.stringify(payload);
      }
      const response = await fetch(baseUrl + url, {
        method,
        headers,
        body: payload,
        redirect: "manual"
      });
      storeCookies(response.headers);
      const buffer = Buffer.from(await response.arrayBuffer());
      const text = opts.binary ? "" : buffer.toString("utf8");
      let json = null;
      if (text) {
        try {
          json = JSON.parse(text);
        } catch (error) {
          json = null;
        }
      }
      return { status: response.status, json, text, buffer, headers: response.headers };
    }

    return {
      request,
      cookieCount: () => jar.size
    };
  }

  function expectStatus(response, expected, label) {
    return check(response.status === expected, label,
      "HTTP " + response.status + " " + String(response.text || "").slice(0, 200));
  }

  async function upload(client, name, tags) {
    const form = new FormData();
    form.append("file", new Blob([PNG_1X1], { type: "image/png" }), name);
    form.append("tags", JSON.stringify(tags));
    return client.request("POST", "/api/images", { body: form });
  }

  function imageID(response) {
    return response.json && response.json.id ? String(response.json.id) : "";
  }

  async function run() {
    const port = await freePort();
    baseUrl = "http://127.0.0.1:" + port;

    let binPath = process.env.PICBOX_BIN || "";
    if (!binPath) {
      binPath = path.join(tmpRoot, "picbox");
      console.log("# 构建临时二进制：" + binPath);
      const build = spawnSync("go", ["build", "-trimpath", "-o", binPath, "./cmd/picbox"], {
        cwd: serverDir,
        stdio: ["ignore", "inherit", "inherit"],
        env: Object.assign({}, process.env, { CGO_ENABLED: "0" })
      });
      if (build.status !== 0) {
        throw new Error("go build 失败（" + build.status + "）");
      }
    }

    console.log("# 启动临时服务：" + baseUrl + "，数据目录 " + dataDir);
    child = spawn(binPath, [], {
      cwd: tmpRoot,
      env: Object.assign({}, process.env, {
        PICBOX_ADDR: "127.0.0.1:" + port,
        PICBOX_DATA_DIR: dataDir,
        BASEURL: baseUrl
      }),
      stdio: ["ignore", "pipe", "pipe"]
    });
    child.stdout.on("data", (chunk) => { serverLog += String(chunk); });
    child.stderr.on("data", (chunk) => { serverLog += String(chunk); });

    await waitForHealth(baseUrl, Date.now() + 15000);

    const admin = createClient();
    const user = createClient();
    const anon = createClient();

    console.log("\n# 静态壳、内嵌资源与安全头");
    console.log("# 注意：以下只验证 HTTP 契约与静态资源，本脚本不执行浏览器 JavaScript。");
    const index = await anon.request("GET", "/");
    expectStatus(index, 200, "GET / 返回静态壳");
    check(String(index.headers.get("content-type") || "").startsWith("text/html"),
      "首页 Content-Type 为 text/html",
      index.headers.get("content-type"));
    check(String(index.headers.get("cache-control") || "") === "no-store",
      "页面使用 no-store", index.headers.get("cache-control"));
    const csp = String(index.headers.get("content-security-policy") || "");
    check(csp.includes("script-src 'self'"), "CSP 的 script-src 只有 'self'", csp);
    check(!csp.includes("unsafe-eval"), "CSP 未放宽 unsafe-eval", csp);
    check(csp.includes("style-src 'self' 'unsafe-inline'"),
      "CSP 允许 React 内联样式（style-src 'unsafe-inline'）", csp);
    check(csp.includes("connect-src 'self'"), "CSP 的 connect-src 只有 'self'", csp);
    check(!index.text.includes("Go 新版预览"), "壳不再包含已删除的简化预览页文案");
    check(!index.text.includes('id="tab-trash"'), "壳不再包含已删除的简化预览页 DOM");

    const assetPaths = [...new Set(
      [...index.text.matchAll(/(?:src|href)="(\/assets\/[^"]+)"/g)].map((match) => match[1])
    )];
    check(assetPaths.length > 0, "index.html 实际引用了 /assets/* 资源",
      JSON.stringify(assetPaths));
    const hasScriptAsset = assetPaths.some((asset) => asset.endsWith(".js"));
    const hasStyleAsset = assetPaths.some((asset) => asset.endsWith(".css"));
    check(hasScriptAsset && hasStyleAsset, "壳同时引用 JS 与 CSS 资源",
      JSON.stringify(assetPaths));

    let jsBody = "";
    for (const asset of assetPaths) {
      const response = await anon.request("GET", asset);
      expectStatus(response, 200, "GET " + asset + " 可取");
      const contentType = String(response.headers.get("content-type") || "");
      const expected = asset.endsWith(".js")
        ? "text/javascript"
        : asset.endsWith(".css") ? "text/css" : "image/svg+xml";
      check(contentType.startsWith(expected), asset + " 的 MIME 为 " + expected, contentType);
      check(String(response.headers.get("cache-control") || "") === "no-cache",
        asset + " 使用 no-cache + ETag 重新验证", response.headers.get("cache-control"));
      const etag = String(response.headers.get("etag") || "");
      check(etag !== "", asset + " 返回 ETag", etag);
      const conditional = await anon.request("GET", asset, { headers: { "if-none-match": etag } });
      check(conditional.status === 304, asset + " 的 If-None-Match 返回 304",
        "HTTP " + conditional.status);
      if (asset.endsWith(".js")) {
        jsBody = response.text;
      }
    }

    // 静态包必须真的接上原 UI 的接口面，而不是被替换成简化页。
    for (const expectedPath of [
      "/api/auth/change-password",
      "/api/config",
      "/api/images/batch",
      "/api/images/usage",
      "/api/images/export",
      "/api/admin/activation-codes",
      "/api/images/from-url"
    ]) {
      check(jsBody.includes(expectedPath), "前端包调用原 UI 接口 " + expectedPath);
    }
    check(!jsBody.includes("/api/auth/password\""), "前端包不再调用旧 /api/auth/password");
    // 邮件入口的 gating 必须真的在打包后的代码里，而不是只存在于源码。
    check(jsBody.includes("mailEnabled"), "前端包读取 /api/config 的 mailEnabled 能力标志");

    const headPage = await anon.request("HEAD", "/admin");
    expectStatus(headPage, 200, "HEAD /admin 返回 200");
    check(String(headPage.headers.get("content-security-policy") || "") === csp,
      "HEAD 页面与 GET 使用同一 CSP");
    check(headPage.text === "", "HEAD 不返回响应体");

    const imagePage = await anon.request("GET", "/images/not-a-real-image-id");
    check(imagePage.status === 404,
      "不存在的图片详情初始 GET 返回 404（浏览器仍渲染 404 页）",
      "HTTP " + imagePage.status);
    check(imagePage.text === index.text,
      "缺失图片详情返回与首页相同的页面壳",
      "body " + imagePage.text.length + " 字节");
    const missingImageHead = await anon.request("HEAD", "/images/not-a-real-image-id");
    check(missingImageHead.status === 404 && missingImageHead.text === "",
      "缺失图片详情 HEAD 返回 404 且无响应体",
      "HTTP " + missingImageHead.status + " body " + missingImageHead.text.length);
    const badImagePage = await anon.request("GET", "/images/bad%2Fid");
    check(badImagePage.status === 404, "非法 /images/{id} 返回 404", "HTTP " + badImagePage.status);

    // 未知路径：原版 Next.js 渲染 app/not-found.tsx，这里必须是 404 而不是 200，
    // 但界面仍由页面壳交给 React 的 404 页渲染。
    const unknownPage = await anon.request("GET", "/ui-audit-unknown");
    check(unknownPage.status === 404, "未知路径返回 HTTP 404", "HTTP " + unknownPage.status);
    check(String(unknownPage.headers.get("content-type") || "").startsWith("text/html") &&
      unknownPage.text === index.text, "未知路径返回与首页相同的 404 页壳",
      String(unknownPage.headers.get("content-type")));
    check(String(unknownPage.headers.get("cache-control") || "") === "no-store",
      "404 页使用 no-store", unknownPage.headers.get("cache-control"));
    const unknownHead = await anon.request("HEAD", "/ui-audit-unknown");
    check(unknownHead.status === 404 && unknownHead.text === "",
      "未知路径 HEAD 返回 404 且无响应体",
      "HTTP " + unknownHead.status + " body " + unknownHead.text.length);

    // 已注册页面的尾斜杠：Next.js 的 trailingSlash:false 是 308，并保留 query。
    for (const [from, to] of [
      ["/dashboard/", "/dashboard"],
      ["/admin/?tab=users&q=a%2Fb", "/admin?tab=users&q=a%2Fb"],
      ["/images/not-a-real-image-id/", "/images/not-a-real-image-id"]
    ]) {
      const redirect = await anon.request("GET", from);
      check(redirect.status === 308, from + " 返回 308", "HTTP " + redirect.status);
      check(String(redirect.headers.get("location") || "") === to,
        from + " 跳转到 " + to + "（保留 query）", redirect.headers.get("location"));
    }

    // 方法行为不能被 404 页壳改变：页面路由仍是 405 + Allow，未知 API 仍走 JSON。
    const postPage = await anon.request("POST", "/dashboard");
    check(postPage.status === 405 && String(postPage.headers.get("allow") || "") === "GET, HEAD",
      "POST /dashboard 仍为 405 + Allow: GET, HEAD",
      "HTTP " + postPage.status + " allow " + postPage.headers.get("allow"));

    const disallowedAsset = await anon.request("GET", "/assets/index.html");
    check(disallowedAsset.status === 404, "非白名单扩展名的 /assets/* 返回 404",
      "HTTP " + disallowedAsset.status);

    const unknownAPI = await anon.request("GET", "/api/does-not-exist");
    expectStatus(unknownAPI, 404, "未知 API 返回 404");
    check(errorCode(unknownAPI.json) === "NOT_FOUND", "未知 API 使用 JSON 错误码 NOT_FOUND",
      errorCode(unknownAPI.json));

    // 上传上限是运行时配置：前端读 /api/config，因此改 UPLOAD_MAX_BYTES 不用重新构建前端。
    const runtimeConfig = await anon.request("GET", "/api/config");
    expectStatus(runtimeConfig, 200, "公开 GET /api/config 可用（无需登录）");
    check(Boolean(runtimeConfig.json) && Number(runtimeConfig.json.uploadMaxBytes) > 0,
      "config.uploadMaxBytes 由服务端运行时下发",
      JSON.stringify(runtimeConfig.json && runtimeConfig.json.uploadMaxBytes));
    // 邮件能力只在 /api/config 里以布尔值公开：本脚本没有配置 SMTP，因此必须明确
    // 为 false（不是缺字段），前端据此把找回密码/改邮箱指向管理员而不是 503。
    check(runtimeConfig.json && runtimeConfig.json.mailEnabled === false,
      "config.mailEnabled 在未配置 SMTP 时明确为 false",
      JSON.stringify(runtimeConfig.json && runtimeConfig.json.mailEnabled));
    const configKeys = Object.keys(runtimeConfig.json || {}).sort().join(",");
    check(configKeys === "mailEnabled,ok,uploadMaxBytes",
      "config 只暴露 ok/uploadMaxBytes/mailEnabled，不含 SMTP 细节", configKeys);

    console.log("\n# 管理员建号与激活码注册");
    const adminEmail = "admin+" + Date.now() + "@example.test";
    const adminPassword = "admin-pass-1";
    const userEmail = "user+" + Date.now() + "@example.test";
    const userPassword = "user-pass-1";
    const userNextPassword = "user-pass-2";

    const registeredAdmin = await admin.request("POST", "/api/auth/register", {
      body: { email: adminEmail, password: adminPassword }
    });
    expectStatus(registeredAdmin, 200, "首个账号免激活码注册");
    check(registeredAdmin.json && registeredAdmin.json.user &&
      registeredAdmin.json.user.role === "ADMIN", "首个账号角色为 ADMIN");
    check(admin.cookieCount() > 0, "注册后获得登录 Cookie");

    const adminMe = await admin.request("GET", "/api/auth/me");
    expectStatus(adminMe, 200, "管理员 GET /api/auth/me 可用");

    const adminLogin = await admin.request("POST", "/api/auth/login", {
      body: { email: adminEmail, password: adminPassword }
    });
    expectStatus(adminLogin, 200, "管理员显式登录（用于 ADMIN_LOGIN 审计）");

    const codesCreated = await admin.request("POST", "/api/admin/activation-codes", {
      body: { count: 1, expiresInDays: 7, note: "regression" }
    });
    expectStatus(codesCreated, 200, "管理员生成激活码");
    const activationCode = codesCreated.json && Array.isArray(codesCreated.json.codes) &&
      codesCreated.json.codes[0] ? String(codesCreated.json.codes[0].code || "") : "";
    check(activationCode !== "", "生成接口返回一次性完整激活码");

    const missingCode = await anon.request("POST", "/api/auth/register", {
      body: { email: "no-code+" + Date.now() + "@example.test", password: userPassword }
    });
    expectStatus(missingCode, 400, "第二账号没有激活码时返回 400");
    check(errorCode(missingCode.json) === "ACTIVATION_CODE_REQUIRED",
      "缺少激活码的错误码为 ACTIVATION_CODE_REQUIRED", errorCode(missingCode.json));

    const registeredUser = await user.request("POST", "/api/auth/register", {
      body: { email: userEmail, password: userPassword, activationCode }
    });
    expectStatus(registeredUser, 200, "用户凭激活码注册");
    check(registeredUser.json && registeredUser.json.user &&
      registeredUser.json.user.role === "USER", "激活码注册的账号角色为 USER");
    const userID = registeredUser.json && registeredUser.json.user
      ? Number(registeredUser.json.user.id)
      : 0;
    check(Number.isInteger(userID) && userID > 0, "注册响应带用户 id", String(userID));

    const reuseCode = await anon.request("POST", "/api/auth/register", {
      body: {
        email: "reuse+" + Date.now() + "@example.test",
        password: userPassword,
        activationCode
      }
    });
    check(reuseCode.status >= 400, "同一激活码不能重复使用", "HTTP " + reuseCode.status);

    console.log("\n# 上传、usage 与 tags");
    const image1 = await upload(user, "one.png", ["alpha", "beta"]);
    expectStatus(image1, 200, "用户上传图片 1（alpha/beta）");
    const id1 = imageID(image1);
    const image2 = await upload(user, "two.png", ["gamma"]);
    expectStatus(image2, 200, "用户上传图片 2（gamma）");
    const id2 = imageID(image2);
    const image3 = await upload(user, "three.png", ["purge"]);
    expectStatus(image3, 200, "用户上传图片 3（用于批量彻底删除）");
    const id3 = imageID(image3);
    check(id1 !== "" && id2 !== "" && id3 !== "", "上传返回图片 ID",
      JSON.stringify([id1, id2, id3]));

    const listAll = await user.request("GET", "/api/images?mine=true&limit=50");
    expectStatus(listAll, 200, "个人图库列表可用");
    const items = listAll.json && Array.isArray(listAll.json.items) ? listAll.json.items : [];
    const sizeOf = (id) => {
      const item = items.find((entry) => String(entry.id) === id);
      return item ? Number(item.size) : 0;
    };
    const expectedUsed = sizeOf(id1) + sizeOf(id2) + sizeOf(id3);
    check(items.length === 3 && expectedUsed > 0, "图库包含 3 张且 size 可读",
      JSON.stringify({ count: items.length, expectedUsed }));

    const usage = await user.request("GET", "/api/images/usage");
    expectStatus(usage, 200, "GET /api/images/usage 可用");
    check(usage.json && Number(usage.json.usedBytes) === expectedUsed,
      "usage.usedBytes 等于全部原图大小之和",
      JSON.stringify({ got: usage.json && usage.json.usedBytes, expectedUsed }));
    check(usage.json && typeof usage.json.quotaBytes === "number",
      "usage.quotaBytes 是数字（0 表示不限）", JSON.stringify(usage.json && usage.json.quotaBytes));

    const tags = await user.request("GET", "/api/images/tags");
    expectStatus(tags, 200, "GET /api/images/tags 可用");
    const tagList = tags.json && Array.isArray(tags.json.tags) ? tags.json.tags : [];
    for (const tag of ["alpha", "beta", "gamma"]) {
      check(tagList.includes(tag), "标签候选包含 " + tag, JSON.stringify(tagList));
    }

    console.log("\n# 单图 meta 可见性");
    const hide2 = await user.request("PATCH", "/api/images/" + encodeURIComponent(id2), {
      body: { visible: false }
    });
    expectStatus(hide2, 200, "用户把图片 2 设为隐藏");

    const ownerMeta = await user.request("GET", "/api/images/" + encodeURIComponent(id2) + "/meta");
    expectStatus(ownerMeta, 200, "上传者本人可读隐藏图片的 meta");
    check(ownerMeta.json && ownerMeta.json.image && ownerMeta.json.image.isVisible === false,
      "meta 如实标记 isVisible=false");

    const adminHiddenMeta = await admin.request("GET",
      "/api/images/" + encodeURIComponent(id2) + "/meta");
    check(adminHiddenMeta.status === 404, "其他会话读取隐藏图片 meta 返回 404",
      "HTTP " + adminHiddenMeta.status);
    const anonHiddenMeta = await anon.request("GET",
      "/api/images/" + encodeURIComponent(id2) + "/meta");
    check(anonHiddenMeta.status === 404, "未登录读取隐藏图片 meta 返回 404",
      "HTTP " + anonHiddenMeta.status);

    const show2 = await user.request("PATCH", "/api/images/" + encodeURIComponent(id2), {
      body: { visible: true }
    });
    expectStatus(show2, 200, "用户把图片 2 恢复可见");
    const adminVisibleMeta = await admin.request("GET",
      "/api/images/" + encodeURIComponent(id2) + "/meta");
    expectStatus(adminVisibleMeta, 200, "公开图片的 meta 对其他会话可读");
    const visiblePage = await anon.request("GET", "/images/" + encodeURIComponent(id2));
    expectStatus(visiblePage, 200, "可见图片详情初始 GET 返回 200");
    check(visiblePage.text === index.text, "可见图片详情返回同一份页面壳",
      "body " + visiblePage.text.length + " 字节");
    const visibleHead = await anon.request("HEAD", "/images/" + encodeURIComponent(id2));
    check(visibleHead.status === 200 && visibleHead.text === "",
      "可见图片详情 HEAD 返回 200 且无响应体",
      "HTTP " + visibleHead.status + " body " + visibleHead.text.length);

    console.log("\n# 批量操作");
    const batchHide = await user.request("PATCH", "/api/images/batch", {
      body: { ids: [id1, id2], action: "visibility", visible: false }
    });
    expectStatus(batchHide, 200, "批量设为隐藏");
    check(batchHide.json && batchHide.json.updatedCount === 2, "批量隐藏 updatedCount=2",
      JSON.stringify(batchHide.json));

    const hiddenList = await user.request("GET",
      "/api/images?mine=true&limit=50&visibility=hidden");
    check(hiddenList.json && hiddenList.json.total === 2, "隐藏筛选返回 2 张",
      JSON.stringify(hiddenList.json && hiddenList.json.total));

    const batchShow = await user.request("PATCH", "/api/images/batch", {
      body: { ids: [id1, id2], action: "visibility", visible: true }
    });
    expectStatus(batchShow, 200, "批量恢复可见");
    check(batchShow.json && batchShow.json.updatedCount === 2, "批量显示 updatedCount=2",
      JSON.stringify(batchShow.json));

    const batchTagAdd = await user.request("PATCH", "/api/images/batch", {
      body: { ids: [id1, id2], action: "tag", mode: "add", tags: ["batch"] }
    });
    expectStatus(batchTagAdd, 200, "批量追加标签");
    check(batchTagAdd.json && batchTagAdd.json.updatedCount === 2,
      "批量改标签 updatedCount=2", JSON.stringify(batchTagAdd.json));

    const taggedList = await user.request("GET", "/api/images?mine=true&limit=50&tag=batch");
    expectStatus(taggedList, 200, "按标签筛选");
    check(taggedList.json && taggedList.json.total === 2, "带 batch 标签的是 2 张",
      JSON.stringify(taggedList.json && taggedList.json.total));

    const batchTagRemove = await user.request("PATCH", "/api/images/batch", {
      body: { ids: [id1, id2], action: "tag", mode: "remove", tags: ["batch"] }
    });
    expectStatus(batchTagRemove, 200, "批量移除标签");
    check(batchTagRemove.json && batchTagRemove.json.updatedCount === 2,
      "批量移除标签 updatedCount=2", JSON.stringify(batchTagRemove.json));

    const invalidBatch = await user.request("PATCH", "/api/images/batch", {
      body: { ids: "not-an-array", action: "visibility", visible: true }
    });
    expectStatus(invalidBatch, 400, "非法 ids 返回 400");
    check(errorCode(invalidBatch.json) === "INVALID_IDS", "非法 ids 的错误码为 INVALID_IDS",
      errorCode(invalidBatch.json));

    const batchDelete3 = await user.request("PATCH", "/api/images/batch", {
      body: { ids: [id3], action: "delete" }
    });
    expectStatus(batchDelete3, 200, "批量软删除图片 3");
    check(batchDelete3.json && batchDelete3.json.updatedCount === 1,
      "批量软删除 updatedCount=1", JSON.stringify(batchDelete3.json));

    const trash3 = await user.request("GET", "/api/images?mine=true&deleted=true&limit=20");
    check(trash3.json && trash3.json.total === 1, "回收站包含图片 3",
      JSON.stringify(trash3.json && trash3.json.total));

    const batchPurge3 = await user.request("PATCH", "/api/images/batch", {
      body: { ids: [id3], action: "purge" }
    });
    expectStatus(batchPurge3, 200, "批量彻底删除图片 3");
    check(batchPurge3.json && batchPurge3.json.updatedCount === 1,
      "批量彻底删除 updatedCount=1", JSON.stringify(batchPurge3.json));
    check(Boolean(batchPurge3.json) && Array.isArray(batchPurge3.json.updatedIds) &&
      batchPurge3.json.updatedIds.length === 1 &&
      String(batchPurge3.json.updatedIds[0]) === id3,
      "updatedIds 只包含真正成功彻底删除的 id（UI 只移除这些）",
      JSON.stringify(batchPurge3.json && batchPurge3.json.updatedIds));
    check(!batchPurge3.json || !batchPurge3.json.cleanupPending,
      "文件清理完成时不报告 cleanupPending", JSON.stringify(batchPurge3.json));

    for (const variant of ["original", "thumb"]) {
      const purged = await anon.request("GET",
        "/api/images/" + encodeURIComponent(id3) + "?variant=" + variant);
      check(purged.status === 404, "彻底删除后图片 3 的 " + variant + " 返回 404",
        "HTTP " + purged.status);
    }
    const usageAfterPurge = await user.request("GET", "/api/images/usage");
    check(usageAfterPurge.json &&
      Number(usageAfterPurge.json.usedBytes) === sizeOf(id1) + sizeOf(id2),
      "彻底删除后 usage.usedBytes 只统计剩余图片",
      JSON.stringify(usageAfterPurge.json && usageAfterPurge.json.usedBytes));

    console.log("\n# 导出 HEAD 与真实 ZIP");
    const hideForExport = await user.request("PATCH", "/api/images/" + encodeURIComponent(id2), {
      body: { visible: false }
    });
    expectStatus(hideForExport, 200, "导出前把图片 2 设为隐藏（验证隐藏图片也导出）");

    const exportHead = await user.request("HEAD", "/api/images/export?scope=all");
    expectStatus(exportHead, 200, "导出 HEAD 预检返回 200");
    check(String(exportHead.headers.get("x-export-count") || "") === "2",
      "HEAD 报告 X-Export-Count=2（隐藏图片也计入）",
      exportHead.headers.get("x-export-count"));
    const exportBytes = Number(exportHead.headers.get("x-export-bytes") || "0");
    check(exportBytes === sizeOf(id1) + sizeOf(id2), "HEAD 报告 X-Export-Bytes 等于原图总字节",
      String(exportBytes));
    check(String(exportHead.headers.get("x-export-cooldown-hours") || "") === "12",
      "HEAD 报告默认 12 小时冷却", exportHead.headers.get("x-export-cooldown-hours"));

    const exportHeadAgain = await user.request("HEAD", "/api/images/export?scope=all");
    expectStatus(exportHeadAgain, 200, "HEAD 不会消耗冷却（可重复预检）");

    const invalidScope = await user.request("HEAD", "/api/images/export?scope=nope");
    expectStatus(invalidScope, 400, "非法 scope 的 HEAD 返回 400");
    check(String(invalidScope.headers.get("x-export-error") || "") === "INVALID_SCOPE",
      "HEAD 错误通过 X-Export-Error 传递", invalidScope.headers.get("x-export-error"));

    const exportGet = await user.request("GET", "/api/images/export?scope=all", { binary: true });
    expectStatus(exportGet, 200, "导出 GET 返回 200");
    check(String(exportGet.headers.get("content-type") || "").startsWith("application/zip"),
      "导出 Content-Type 为 application/zip", exportGet.headers.get("content-type"));
    check(/attachment; filename="picbox-images-all-\d{8}-\d{6}\.zip"/.test(
      String(exportGet.headers.get("content-disposition") || "")),
      "导出带 attachment 文件名", exportGet.headers.get("content-disposition"));

    let entries = new Map();
    try {
      entries = readZipEntries(exportGet.buffer);
      check(true, "ZIP 中央目录可解析（" + entries.size + " 个条目）");
    } catch (error) {
      check(false, "ZIP 中央目录可解析", String(error && error.message ? error.message : error));
    }
    check(entries.has("README.txt"), "ZIP 包含 README.txt");
    check(entries.has("manifest.json"), "ZIP 包含 manifest.json");
    check(entries.has("images/" + id1 + ".png") && entries.has("images/" + id2 + ".png"),
      "ZIP 按 id 包含两张原图", JSON.stringify([...entries.keys()]));
    const manifestEntry = entries.get("manifest.json");
    let manifest = null;
    try {
      manifest = manifestEntry ? JSON.parse(manifestEntry.toString("utf8")) : null;
    } catch (error) {
      manifest = null;
    }
    check(Boolean(manifest), "manifest.json 是合法 JSON");
    check(Boolean(manifest) && manifest.scope === "all" && manifest.count === 2,
      "manifest 记录 scope=all 与 count=2", JSON.stringify(manifest && {
        scope: manifest.scope, count: manifest.count
      }));
    const manifestIDs = manifest && Array.isArray(manifest.images)
      ? manifest.images.map((image) => String(image.id)).sort()
      : [];
    check(JSON.stringify(manifestIDs) === JSON.stringify([id1, id2].sort()),
      "manifest 的 images[] 与实际归档一致", JSON.stringify(manifestIDs));
    const original1 = entries.get("images/" + id1 + ".png");
    check(Boolean(original1) && original1.equals(PNG_1X1), "ZIP 内原图字节与上传一致");

    const secondExport = await user.request("GET", "/api/images/export?scope=all");
    expectStatus(secondExport, 429, "成功导出后立即再次导出进入冷却 429");
    check(errorCode(secondExport.json) === "EXPORT_COOLDOWN", "冷却错误码为 EXPORT_COOLDOWN",
      errorCode(secondExport.json));
    const headDuringCooldown = await user.request("HEAD", "/api/images/export?scope=all");
    expectStatus(headDuringCooldown, 429, "冷却期内 HEAD 如实返回 429");
    check(String(headDuringCooldown.headers.get("x-export-error") || "") === "EXPORT_COOLDOWN",
      "冷却期 HEAD 通过 X-Export-Error 报告 EXPORT_COOLDOWN",
      headDuringCooldown.headers.get("x-export-error"));

    console.log("\n# change-password、旧会话与 API Key 语义");
    const keyCreated = await user.request("POST", "/api/apikeys", { body: { name: "回归密钥" } });
    expectStatus(keyCreated, 200, "创建 API Key");
    const rawKey = keyCreated.json && keyCreated.json.key ? keyCreated.json.key.rawKey : "";
    check(typeof rawKey === "string" && rawKey !== "", "创建接口返回原始密钥");

    const keyPatch = await user.request("PATCH", "/api/images/" + encodeURIComponent(id1), {
      body: { visible: false },
      bearer: rawKey
    });
    expectStatus(keyPatch, 200, "API Key 可执行允许的写操作（PATCH 可见性）");

    const oldSession = createClient();
    const oldSessionLogin = await oldSession.request("POST", "/api/auth/login", {
      body: { email: userEmail, password: userPassword }
    });
    expectStatus(oldSessionLogin, 200, "第二个会话（模拟另一台设备）登录成功");

    const wrongOld = await user.request("POST", "/api/auth/change-password", {
      body: { oldPassword: "definitely-wrong", newPassword: userNextPassword }
    });
    expectStatus(wrongOld, 400, "原密码错误时 change-password 返回 400");
    check(errorCode(wrongOld.json) === "INVALID_CREDENTIALS", "错误码为 INVALID_CREDENTIALS",
      errorCode(wrongOld.json));

    const samePassword = await user.request("POST", "/api/auth/change-password", {
      body: { oldPassword: userPassword, newPassword: userPassword }
    });
    expectStatus(samePassword, 400, "新密码与原密码相同时返回 400");
    check(errorCode(samePassword.json) === "SAME_PASSWORD", "错误码为 SAME_PASSWORD",
      errorCode(samePassword.json));

    const changed = await user.request("POST", "/api/auth/change-password", {
      body: { oldPassword: userPassword, newPassword: userNextPassword }
    });
    expectStatus(changed, 200, "change-password 成功返回 200");
    check(Boolean(changed.json && changed.json.ok === true), "change-password 响应 ok:true");

    const meAfterChange = await user.request("GET", "/api/auth/me");
    expectStatus(meAfterChange, 200, "改密后当前会话保持登录（Cookie 已重签）");

    const oldSessionMe = await oldSession.request("GET", "/api/auth/me");
    expectStatus(oldSessionMe, 401, "其他设备的旧会话立即失效");

    const keyAfterChange = await user.request("PATCH", "/api/images/" + encodeURIComponent(id1), {
      body: { visible: true },
      bearer: rawKey
    });
    expectStatus(keyAfterChange, 200, "change-password 不撤销 API Key");

    const oldPasswordLogin = await anon.request("POST", "/api/auth/login", {
      body: { email: userEmail, password: userPassword }
    });
    expectStatus(oldPasswordLogin, 400, "旧密码不能再登录");
    check(errorCode(oldPasswordLogin.json) === "INVALID_CREDENTIALS",
      "旧密码错误码为 INVALID_CREDENTIALS", errorCode(oldPasswordLogin.json));

    const newSession = createClient();
    const newPasswordLogin = await newSession.request("POST", "/api/auth/login", {
      body: { email: userEmail, password: userNextPassword }
    });
    expectStatus(newPasswordLogin, 200, "新密码可以登录");

    const unavailableRecovery = await anon.request("POST", "/api/auth/forgot-password", {
      body: { email: userEmail }
    });
    expectStatus(unavailableRecovery, 503, "未配置 SMTP 时 forgot-password 返回 503");
    check(errorCode(unavailableRecovery.json) === "MAIL_UNAVAILABLE",
      "未配置邮件的错误码为 MAIL_UNAVAILABLE", errorCode(unavailableRecovery.json));

    console.log("\n# 用户配额、禁用与管理审计");
    const usersList = await admin.request("GET", "/api/admin/users?limit=50");
    expectStatus(usersList, 200, "管理员用户列表可用");
    const listedUser = usersList.json && Array.isArray(usersList.json.users)
      ? usersList.json.users.find((entry) => Number(entry.id) === userID)
      : null;
    check(Boolean(listedUser), "用户列表包含刚注册的账号");
    check(Boolean(listedUser) && Number(listedUser.storageUsedBytes) > 0,
      "用户列表的 storageUsedBytes > 0", JSON.stringify(listedUser && listedUser.storageUsedBytes));

    const setQuota = await admin.request("PATCH", "/api/admin/users/" + userID, {
      body: { operation: "set_quota", quotaBytes: 1048576 }
    });
    expectStatus(setQuota, 200, "管理员设置账号配额");
    check(Boolean(setQuota.json && setQuota.json.quota && setQuota.json.quota.changed === true),
      "首次设置配额 changed=true", JSON.stringify(setQuota.json && setQuota.json.quota));
    check(Boolean(setQuota.json && setQuota.json.quota &&
      Number(setQuota.json.quota.effectiveBytes) === 1048576),
      "生效配额为 1048576 字节", JSON.stringify(setQuota.json && setQuota.json.quota));

    const setQuotaAgain = await admin.request("PATCH", "/api/admin/users/" + userID, {
      body: { operation: "set_quota", quotaBytes: 1048576 }
    });
    expectStatus(setQuotaAgain, 200, "重复设置同一配额仍然返回 200");
    check(Boolean(setQuotaAgain.json && setQuotaAgain.json.quota &&
      setQuotaAgain.json.quota.changed === false),
      "重复设置同一配额为 no-op（changed=false）",
      JSON.stringify(setQuotaAgain.json && setQuotaAgain.json.quota));

    const userUsageWithQuota = await user.request("GET", "/api/images/usage");
    check(userUsageWithQuota.json && Number(userUsageWithQuota.json.quotaBytes) === 1048576,
      "usage.quotaBytes 反映管理员覆盖值",
      JSON.stringify(userUsageWithQuota.json && userUsageWithQuota.json.quotaBytes));

    const disableSelf = await admin.request("PATCH", "/api/admin/users/" +
      (adminMe.json && adminMe.json.user ? adminMe.json.user.id : 0), {
      body: { operation: "disable" }
    });
    expectStatus(disableSelf, 403, "管理员不能禁用自己");

    const disableUser = await admin.request("PATCH", "/api/admin/users/" + userID, {
      body: { operation: "disable" }
    });
    expectStatus(disableUser, 200, "管理员禁用账号");
    check(Boolean(disableUser.json && disableUser.json.user &&
      disableUser.json.user.disabledAt), "禁用响应带 disabledAt",
      JSON.stringify(disableUser.json && disableUser.json.user));

    const disabledMe = await newSession.request("GET", "/api/auth/me");
    expectStatus(disabledMe, 403, "被禁用账号的 /api/auth/me 返回 403");
    check(errorCode(disabledMe.json) === "ACCOUNT_DISABLED",
      "/api/auth/me 的错误码在顶层为 ACCOUNT_DISABLED", errorCode(disabledMe.json));

    const disabledLogin = await anon.request("POST", "/api/auth/login", {
      body: { email: userEmail, password: userNextPassword }
    });
    expectStatus(disabledLogin, 403, "被禁用账号不能登录");

    const enableUser = await admin.request("PATCH", "/api/admin/users/" + userID, {
      body: { operation: "enable" }
    });
    expectStatus(enableUser, 200, "管理员重新启用账号");
    const enabledMe = await user.request("GET", "/api/auth/me");
    expectStatus(enabledMe, 200, "重新启用后当前会话恢复可用");

    const stats = await admin.request("GET", "/api/admin/stats");
    expectStatus(stats, 200, "管理端统计可用");
    check(Boolean(stats.json && Number(stats.json.userCount) >= 2),
      "统计的 userCount >= 2", JSON.stringify(stats.json && stats.json.userCount));

    const codesList = await admin.request("GET",
      "/api/admin/activation-codes?status=used&limit=20");
    expectStatus(codesList, 200, "激活码列表（已使用）可用");
    const usedCode = codesList.json && Array.isArray(codesList.json.codes)
      ? codesList.json.codes[0]
      : null;
    check(Boolean(usedCode) && usedCode.usedBy && usedCode.usedBy.email === userEmail,
      "已使用的激活码记录注册账号", JSON.stringify(usedCode && usedCode.usedBy));

    const adminImages = await admin.request("GET", "/api/admin/images?limit=50");
    expectStatus(adminImages, 200, "管理端图片列表可用");
    check(Boolean(adminImages.json && Array.isArray(adminImages.json.items) &&
      adminImages.json.items.length >= 2), "管理端图片列表包含用户图片",
      JSON.stringify(adminImages.json && adminImages.json.items &&
        adminImages.json.items.length));

    const badAuditAction = await admin.request("GET",
      "/api/admin/audit-logs?action=NOT_A_REAL_ACTION");
    expectStatus(badAuditAction, 400, "非法审计动作过滤返回 400");
    check(errorCode(badAuditAction.json) === "INVALID_ACTION", "错误码为 INVALID_ACTION",
      errorCode(badAuditAction.json));

    for (const action of [
      "ADMIN_LOGIN",
      "ACTIVATION_CODE_USED",
      "ADMIN_USER_QUOTA_UPDATE",
      "ADMIN_USER_DISABLE",
      "ADMIN_USER_ENABLE"
    ]) {
      const logs = await admin.request("GET",
        "/api/admin/audit-logs?limit=20&action=" + encodeURIComponent(action));
      expectStatus(logs, 200, "审计日志按 " + action + " 过滤可用");
      check(Boolean(logs.json && Array.isArray(logs.json.logs) && logs.json.logs.length > 0),
        "审计日志包含 " + action, JSON.stringify(logs.json && logs.json.logs &&
          logs.json.logs.length));
    }

    const usedLogs = await admin.request("GET",
      "/api/admin/audit-logs?limit=20&action=ACTIVATION_CODE_USED");
    const usedLog = usedLogs.json && Array.isArray(usedLogs.json.logs)
      ? usedLogs.json.logs[0]
      : null;
    check(Boolean(usedLog) && usedLog.actor && usedLog.actor.email === userEmail,
      "ACTIVATION_CODE_USED 的操作者是新注册用户",
      JSON.stringify(usedLog && usedLog.actor));
  }

  const hardStop = setTimeout(() => {
    console.error("FAIL 超时：脚本超过 180 秒未完成");
    failures += 1;
    try {
      if (child) {
        child.kill("SIGKILL");
      }
    } catch (error) {
      /* 忽略 */
    }
    try {
      fs.rmSync(tmpRoot, { recursive: true, force: true });
    } catch (error) {
      /* 忽略 */
    }
    process.exit(1);
  }, 180000);
  hardStop.unref();

  run().then(() => {
    console.log("");
    if (failures > 0) {
      console.error("失败断言：" + failures + " 条");
      process.exitCode = 1;
      return;
    }
    console.log("全部断言通过");
    console.log("注意：本脚本不执行浏览器 JavaScript，以上结果只代表 HTTP 契约与静态壳，");
    console.log("不代表弹窗/导航/窄屏/暗色等 UI 交互已经验收。");
  }).catch((error) => {
    failures += 1;
    console.error("运行失败：" + (error && error.stack ? error.stack : error));
    if (serverLog) {
      console.error("服务输出：\n" + serverLog);
    }
    process.exitCode = 1;
  }).finally(() => {
    clearTimeout(hardStop);
    if (child && child.exitCode === null) {
      child.kill("SIGTERM");
      const killTimer = setTimeout(() => {
        try {
          child.kill("SIGKILL");
        } catch (error) {
          /* 忽略 */
        }
      }, 3000);
      killTimer.unref();
      child.once("exit", () => clearTimeout(killTimer));
    }
    try {
      fs.rmSync(tmpRoot, { recursive: true, force: true });
    } catch (error) {
      console.error("清理临时目录失败：" + tmpRoot + " :: " + error.message);
    }
  });
}

main();
