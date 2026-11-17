// WebAPI 凭证提取器 —— popup 逻辑
//
// 为什么用扩展而不是书签脚本：
//   1. 书签只能跑在页面上下文，`document.cookie` 读不到 **HttpOnly** 的 Cookie，
//      而 Gemini 的 __Secure-1PSID / Loomy 的登录态恰好都在 HttpOnly 里；
//      扩展有 `cookies` 权限，能直接读到全部（含 HttpOnly），这是关键差别。
//   2. 书签要每个站点单独写一段、还要用户自己判断在哪个页面点；
//      扩展能识别当前域名，自动选择对应的提取规则。
//   3. 提取逻辑随站点变化，扩展可以版本化更新，书签做不到。

// ---------- 站点规则 ----------
// build(ctx) 返回 { text, usage, raw, level }
//   level: ok / warn / err —— 只影响提示颜色

const SITES = [
  {
    id: "deepseek",
    name: "DeepSeek 网页版",
    hosts: ["chat.deepseek.com"],
    build: (ctx) => {
      const ut = parseJSON(ctx.ls["userToken"]);
      const st = parseJSON(ctx.ls["settingsJwt"]);
      const dev = ctx.ls["deepseek-device-id:chat"] || "";
      const cookie = cookieString(ctx.cookies);
      if (!ut || !ut.value) {
        return { level: "err", text: "", raw: ctx.rawDump(),
          usage: "没在 localStorage 里找到 userToken —— 确认这个标签页已经登录 DeepSeek，然后刷新页面再试。" };
      }
      const doc = {
        auth_token: ut.value,
        device_id: dev,
        settings_token: (st && st.value && st.value.jwt) || "",
        cookie,
      };
      return {
        level: "ok",
        text: JSON.stringify(doc, null, 2),
        raw: ctx.rawDump(),
        usage: `把上面的 JSON 整段粘到网关面板的 <b>WebAPI 接入 → duruoxian → ① 粘贴凭证</b>，
                点「拆开填入」再点「导入到 webAPI 服务」。
                <br>device_id 或 settings_token 为空也能用，但建议齐全（换号/续期时需要）。`,
      };
    },
  },
  {
    id: "gemini",
    name: "Gemini 网页版",
    hosts: ["gemini.google.com"],
    build: (ctx) => {
      const names = ctx.cookies.map((c) => c.name);
      const need = ["__Secure-1PSID", "__Secure-1PSIDTS", "SAPISID"];
      const missing = need.filter((n) => !names.includes(n));
      const cookie = cookieString(ctx.cookies);
      const usage = `把上面这一整行存成文件：
        <code>/vol1/@appdata/gemini-web2api/cookie.txt</code>
        （飞牛 → 文件管理，或用 SSH 写入），然后重启 gemini-web2api 应用。
        <br>带上登录 Cookie 后请求改按你账号的配额计算，能避开匿名访问的 429 限流。`;
      if (missing.length) {
        return { level: "warn", text: cookie, raw: ctx.rawDump(),
          usage: `缺少关键 Cookie：<b>${missing.join("、")}</b> —— 说明这个标签页可能没登录成功。
            ${usage}` };
      }
      return { level: "ok", text: cookie, raw: ctx.rawDump(), usage };
    },
  },
  {
    // 讯飞星火的**主站**（xinghuo.xfyun.cn）与 Loomy（loomy.xunfei.cn）是两个站点：
    // 网关的 loomy 渠道对接的是 loomyad.xunfei.cn，session 也只在 loomy 域名下。
    // 用户实测就在主站上点扩展，看到"未识别"一头雾水 —— 这里明确指路。
    id: "xinghuo-main",
    name: "讯飞星火主站（不是 Loomy）",
    hosts: ["xinghuo.xfyun.cn", "xfyun.cn"],
    build: () => ({
      level: "err",
      text: "",
      raw: "",
      usage: `这里是<b>讯飞星火主站</b>，网关对接的 Loomy 是另一个站点。
        <br>请打开 <a href="https://loomy.xunfei.cn" target="_blank">loomy.xunfei.cn</a>
        （标题是「Loomy · 让工作更轻松」）并在那里登录，再点本扩展的「提取」。
        <br>两个站的登录态不通用，主站的 Cookie 不能给 Loomy 用。`,
    }),
  },
  {
    id: "loomy",
    name: "讯飞星火 Loomy",
    hosts: ["loomy.xunfei.cn", "loomyad.xunfei.cn"],
    build: (ctx) => {
      // Loomy 的 session 是一个 JWT，可能放在 localStorage 的某个键或 Cookie 里；
      // 不猜键名，直接把两边所有值扫一遍找 JWT 形状（eyJ 开头、两段点）。
      const pool = [];
      for (const [k, v] of Object.entries(ctx.ls)) pool.push({ k, v });
      for (const c of ctx.cookies) pool.push({ k: "cookie:" + c.name, v: c.value });
      const hit = pool.find(({ v }) => isJWT(v));
      const usage = `把上面的 JSON 粘到网关面板的 <b>JSON 登录 → 渠道选 Loomy</b>。
        <br>Loomy 的 session 约 14 天有效且没有 refresh，到期后回这个页面重新登录、再提取一次。`;
      if (!hit) {
        return {
          level: "err", text: "", raw: ctx.rawDump(),
          usage: `没扫到 JWT 形状的 session。请确认已登录并随手发一句话（让请求带出 token），
                  再点一次「提取」；仍不行就点「复制原始数据」把内容发我，我加一条针对性的规则。`,
        };
      }
      const doc = { auth: { accessToken: hit.v }, account: { uid: "xunfei" } };
      return {
        level: "ok",
        text: JSON.stringify(doc, null, 2),
        raw: ctx.rawDump(),
        usage: `session 取自 <code>${hit.k}</code>。${usage}`,
      };
    },
  },
  {
    id: "chatglm",
    name: "智谱 GLM 网页版",
    hosts: ["chatglm.cn"],
    // 智谱/Kimi/通义 走的是 duruoxian 的四平台；它要的四个字段名在各站不同，
    // 这里做「最佳猜名 + 原始数据兜底」，标 warn 表示需要在面板里核对。
    build: (ctx) => bestEffort4(ctx, "智谱 GLM"),
  },
  {
    id: "kimi",
    name: "Kimi 网页版",
    hosts: ["www.kimi.com", "kimi.moonshot.cn"],
    build: (ctx) => bestEffort4(ctx, "Kimi"),
  },
  {
    id: "qwen",
    name: "通义千问 网页版",
    hosts: ["chat.qwen.ai"],
    build: (ctx) => bestEffort4(ctx, "通义千问"),
  },
];

// bestEffort4 尽力按名字猜四个字段（auth_token / device_id / settings_token / cookie）。
// 这四个站点我还没有逐个验证过映射，所以结果标 warn，并在提示里说明要先核对。
function bestEffort4(ctx, label) {
  const pick = (re) => {
    for (const [k, v] of Object.entries(ctx.ls)) {
      if (re.test(k) && isLongToken(v)) return v;
    }
    for (const c of ctx.cookies) {
      if (re.test(c.name) && isLongToken(c.value)) return c.value;
    }
    return "";
  };
  const doc = {
    auth_token: pick(/token|session|jwt/i),
    device_id: pick(/device/i),
    settings_token: pick(/settings/i),
    cookie: cookieString(ctx.cookies),
  };
  const filled = ["auth_token", "device_id", "settings_token"].filter((k) => doc[k]).length;
  return {
    level: "warn",
    text: JSON.stringify(doc, null, 2),
    raw: ctx.rawDump(),
    usage: `${label} 的字段映射还没逐个验证过（猜中 ${filled}/3 个）。把 JSON 粘进
      <b>WebAPI 接入 → duruoxian → ① 粘贴凭证</b> 后，请核对四项是否都有值；
      缺的那项点「复制原始数据」发我，我补一条精确规则。
      <br>另外这个平台<b>必须先在 duruoxian 里登录该平台账号</b>，否则上游没有它的模型。`,
  };
}

// ---------- 工具 ----------
const parseJSON = (s) => { try { return JSON.parse(s); } catch { return null; } };
const isJWT = (v) => typeof v === "string" && v.startsWith("eyJ") && (v.match(/\./g) || []).length >= 2;
const isLongToken = (v) => typeof v === "string" && v.length >= 20;

function cookieString(cookies) {
  return cookies.map((c) => `${c.name}=${c.value}`).join("; ");
}

// ---------- 页面上下文函数（会被序列化注入页面执行，不能引用外部变量） ----------
function dumpLocalStorage() {
  const out = {};
  try {
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i);
      out[k] = localStorage.getItem(k);
    }
  } catch (e) { /* 某些页面禁用了 storage */ }
  return out;
}

// ---------- UI ----------
const $ = (id) => document.getElementById(id);
let current = { site: null, tab: null, data: null };

async function getActiveTab() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  return tab || null;
}

function matchSite(urlStr) {
  if (!urlStr) return null;
  let host;
  try { host = new URL(urlStr).hostname; } catch { return null; }
  return SITES.find((s) => s.hosts.some((h) => host === h || host.endsWith("." + h) || host.endsWith(h))) || null;
}

async function collectContext(tab) {
  const url = tab.url;
  const cookies = await chrome.cookies.getAll({ url });
  let ls = {};
  try {
    const [res] = await chrome.scripting.executeScript({
      target: { tabId: tab.id },
      func: dumpLocalStorage,
    });
    ls = (res && res.result) || {};
  } catch (e) {
    // chrome:// 或未授权页面会抛错，保持空对象即可
  }
  const rawDump = () => JSON.stringify({
    url,
    cookies: cookies.map((c) => ({ name: c.name, value: c.value, httpOnly: c.httpOnly })),
    localStorage: ls,
  }, null, 2);
  return { url, cookies, ls, rawDump };
}

async function init() {
  const tab = await getActiveTab();
  const site = matchSite(tab && tab.url);
  current.tab = tab;
  current.site = site;
  if (!site) {
    $("siteName").textContent = "当前页面不是支持的站点";
    $("siteBadge").textContent = "未识别";
    $("btnExtract").disabled = true;
    $("fromHint").textContent = "支持：" + SITES.map((s) => s.hosts[0]).join(" / ");
    return;
  }
  $("siteName").textContent = site.name;
  $("siteBadge").textContent = "已识别";
  $("siteBadge").classList.remove("off");
  $("btnExtract").disabled = false;
  $("fromHint").textContent = "来源：" + (new URL(tab.url).hostname);
}

function setMsg(text, level) {
  const el = $("msg");
  el.className = "msg" + (level ? " " + level : "");
  el.innerHTML = text || "";
}

$("btnExtract").addEventListener("click", async () => {
  if (!current.site) return;
  $("btnExtract").disabled = true;
  setMsg("正在读取 Cookie 与本地存储…", "");
  try {
    const ctx = await collectContext(current.tab);
    const r = current.site.build(ctx);
    current.data = r;
    $("out").value = r.text || "";
    $("usage").innerHTML = r.usage || "";
    $("btnCopy").disabled = !r.text;
    $("btnRaw").disabled = false;
    setMsg(
      r.level === "ok" ? "提取完成，可以直接复制"
        : r.level === "warn" ? "提取完成，但有需要核对的地方"
        : "没提取到关键字段，见下方说明",
      r.level
    );
  } catch (e) {
    setMsg("提取失败：" + (e && e.message ? e.message : e), "err");
  } finally {
    $("btnExtract").disabled = false;
  }
});

$("btnCopy").addEventListener("click", async () => {
  const text = $("out").value;
  if (!text) return;
  try {
    await navigator.clipboard.writeText(text);
    setMsg("已复制到剪贴板", "ok");
  } catch {
    $("out").select();
    setMsg("浏览器不允许自动复制，已选中内容，按 Ctrl+C 即可", "warn");
  }
});

$("btnRaw").addEventListener("click", async () => {
  if (!current.data || !current.data.raw) return;
  try {
    await navigator.clipboard.writeText(current.data.raw);
    setMsg("原始数据（Cookie + localStorage）已复制", "ok");
  } catch {
    $("out").value = current.data.raw;
    setMsg("浏览器不允许自动复制，原始数据已填到上面的框里", "warn");
  }
});

init();
