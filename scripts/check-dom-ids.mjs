#!/usr/bin/env node
// 静态校验：app.js 里 $("id") 引用的每个 id 必须在 index.html 中存在。
// 为什么需要：删/改 HTML 元素后忘记同步 JS 绑定，$("x") 返回 null，
// init() 直接抛 TypeError → 后面所有事件绑定全部失效，表现为「按钮点不动」。
// 这类 bug 语法检查（node --check）和现有 ui-static-check 都查不出来。
import fs from "node:fs";
import path from "node:path";

const root = path.resolve(import.meta.dirname, "..");
const web = path.join(root, "cmd/workbuddy-api/web");
const html = fs.readFileSync(path.join(web, "index.html"), "utf8");
const js = fs.readFileSync(path.join(web, "app.js"), "utf8");

// 只统计 getElementById 的直接字面量用法
const refs = new Set();
for (const m of js.matchAll(/\$\("([A-Za-z0-9_-]+)"\)/g)) refs.add(m[1]);
for (const m of js.matchAll(/document\.getElementById\("([A-Za-z0-9_-]+)"\)/g)) refs.add(m[1]);

// 已知 id = 静态 HTML 里的 + app.js 里**动态生成的**（innerHTML 模板中的 id="x"）。
// 后者是有意为之（弹层/列表按需渲染），不能当成缺失。
const ids = new Set();
for (const m of html.matchAll(/\bid="([A-Za-z0-9_-]+)"/g)) ids.add(m[1]);
for (const m of js.matchAll(/\bid="([A-Za-z0-9_-]+)"/g)) ids.add(m[1]);
for (const m of js.matchAll(/\.id\s*=\s*"([A-Za-z0-9_-]+)"/g)) ids.add(m[1]);

const missing = [...refs].filter((id) => !ids.has(id)).sort();
if (missing.length) {
  console.error(`✗ app.js 引用了 index.html 里不存在的 id（会导致 init 抛错、按钮全部失效）：`);
  for (const id of missing) console.error(`   - ${id}`);
  process.exit(1);
}
console.log(`✅ id 引用全部存在（${refs.size} 个引用 / ${ids.size} 个元素）`);
