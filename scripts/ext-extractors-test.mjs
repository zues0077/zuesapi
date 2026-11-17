#!/usr/bin/env node
// 用 vm + 桩件跑 popup.js 的提取逻辑，验证四个站点的产出形状。
// 为什么不直接在浏览器里点：popup 是 chrome-extension:// 页面，需要 --load-extension
// 启动真实 Chrome，成本远高于收益；这里只验证「规则函数」这一层，DOM 交互由静态检查兜住。
import fs from "node:fs";
import path from "node:path";
import vm from "node:vm";

const dir = path.resolve(import.meta.dirname, "..", "ext/webapi-cred-ext");
let src = fs.readFileSync(path.join(dir, "popup.js"), "utf8");
// 把内部常量暴露出来（只在测试拼接里做，不改源文件）
src += "\n;globalThis.__t = { SITES, bestEffort4, cookieString, isJWT };";

// ---- 最小 DOM / chrome 桩件 ----
const el = () => ({
  textContent: "", innerHTML: "", value: "", disabled: false, className: "",
  classList: { add() {}, remove() {} },
  addEventListener() {}, select() {}, style: {},
});
const ctx = {
  console,
  URL,
  JSON,
  setTimeout,
  globalThis: null,
  document: { getElementById: () => el() },
  navigator: { clipboard: { writeText: async () => {} } },
  chrome: {
    tabs: { query: async () => [{ id: 1, url: "https://example.com/" }] },
    cookies: { getAll: async () => [] },
    scripting: { executeScript: async () => [{ result: {} }] },
  },
};
ctx.globalThis = ctx;
vm.createContext(ctx);
vm.runInContext(src, ctx);

const { SITES } = ctx.__t;
const byId = (id) => SITES.find((s) => s.id === id);

let fail = 0;
function check(name, cond, extra = "") {
  console.log(`${cond ? "PASS" : "FAIL"}  ${name}${extra ? "  " + extra : ""}`);
  if (!cond) fail++;
}

// 1) DeepSeek：正常登录态
const ds = byId("deepseek").build({
  ls: {
    userToken: JSON.stringify({ value: "TOKEN-A", __version: "0" }),
    settingsJwt: JSON.stringify({ value: { jwt: "SETTINGS-B" } }),
    "deepseek-device-id:chat": "DEV-C",
  },
  cookies: [{ name: "intercom", value: "x", httpOnly: false }],
  rawDump: () => "{}",
});
const dsObj = JSON.parse(ds.text);
check("deepseek 四字段齐全",
  dsObj.auth_token === "TOKEN-A" && dsObj.device_id === "DEV-C" &&
  dsObj.settings_token === "SETTINGS-B" && dsObj.cookie.includes("intercom=x"), ds.text.slice(0, 60));
check("deepseek 状态为 ok", ds.level === "ok");

// 2) DeepSeek：未登录应报错而不是产出空 JSON
const ds2 = byId("deepseek").build({ ls: {}, cookies: [], rawDump: () => "{}" });
check("deepseek 未登录 -> err 且无 text", ds2.level === "err" && ds2.text === "");

// 3) Gemini：三条关键 Cookie 齐全
const gm = byId("gemini").build({
  ls: {},
  cookies: [
    { name: "__Secure-1PSID", value: "P1", httpOnly: true },
    { name: "__Secure-1PSIDTS", value: "P2", httpOnly: true },
    { name: "SAPISID", value: "S3", httpOnly: true },
  ],
  rawDump: () => "{}",
});
check("gemini 齐全 -> ok", gm.level === "ok" && gm.text.includes("__Secure-1PSID=P1;"));
check("gemini 输出为 cookie 行", gm.text.split(";").length >= 3);

// 4) Gemini：缺 SAPISID 应 warn 且点名
const gm2 = byId("gemini").build({
  ls: {}, cookies: [{ name: "__Secure-1PSID", value: "P1" }], rawDump: () => "{}",
});
check("gemini 缺 SAPISID -> warn 且提示缺哪条",
  gm2.level === "warn" && gm2.usage.includes("SAPISID"));

// 5) Loomy：JWT 藏在 Cookie 里也应能扫到
const JWT = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig";
const lm = byId("loomy").build({
  ls: { theme: "dark" },
  cookies: [{ name: "session", value: JWT, httpOnly: true }],
  rawDump: () => "{}",
});
const lmObj = JSON.parse(lm.text || "{}");
check("loomy 从 Cookie 扫到 JWT",
  lm.level === "ok" && lmObj.auth && lmObj.auth.accessToken === JWT, lm.usage.match(/session 取自 [^）]*/)?.[0] || "");

// 6) Loomy：没有 JWT 时给可执行的下一步
const lm2 = byId("loomy").build({ ls: {}, cookies: [], rawDump: () => "{}" });
check("loomy 无线索 -> err 且引导复制原始数据",
  lm2.level === "err" && lm2.usage.includes("原始数据"));

// 7) 最佳猜名：至少能识别出一个 token 字段
const qw = byId("qwen").build({
  ls: { access_token: "T".repeat(40), device_id: "D".repeat(12) },
  cookies: [{ name: "sid", value: "S".repeat(30) }],
  rawDump: () => "{}",
});
const qwObj = JSON.parse(qw.text);
check("qwen 猜名能填上 auth_token", qwObj.auth_token.length === 40);
check("qwen 标 warn（映射未验证）", qw.level === "warn");

// 8) 站点匹配
const match = (u) => SITES.find((s) => s.hosts.some((h) => {
  const host = new URL(u).hostname;
  return host === h || host.endsWith("." + h) || host.endsWith(h);
}));
check("匹配 chat.deepseek.com", match("https://chat.deepseek.com/a/chat")?.id === "deepseek");
check("匹配 loomyad.xunfei.cn", match("https://loomyad.xunfei.cn/x")?.id === "loomy");
check("不匹配无关站点", !match("https://www.baidu.com/"));
// 星火网页版走 spark-free-api，提取 ssoSessionId（HttpOnly Cookie）
check("xinghuo.xfyun.cn -> spark", match("https://xinghuo.xfyun.cn/desk")?.id === "spark");
const xh = byId("spark").build({
  ls: {}, cookies: [{ name: "ssoSessionId", value: "SSO-ABC", httpOnly: true }], rawDump: () => "{}",
});
check("spark 输出整行 cookie 且标 warn（GtToken 需手取）",
  xh.level === "warn" && xh.text.includes("ssoSessionId=SSO-ABC") && xh.usage.includes("GT_TOKEN"));
const xh2 = byId("spark").build({ ls: {}, cookies: [], rawDump: () => "{}" });
check("spark 未登录 -> err 且引导复制原始数据",
  xh2.level === "err" && xh2.usage.includes("原始数据"));
check("Loomy 子域仍归 loomy", match("https://loomy.xunfei.cn/chat")?.id === "loomy");

console.log(fail === 0 ? "\n全部通过" : `\n${fail} 项失败`);
process.exit(fail === 0 ? 0 : 1);
