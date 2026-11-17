# WebAPI 凭证提取器（浏览器扩展）

给 wb2api 网关用的凭证抓取扩展。在已登录的网页里点一下，就把该平台的登录凭证整理成
网关能直接吃的格式，省掉 F12 → Console/Application → 手工复制 这一套。

## 为什么必须是扩展，不能用书签脚本

| | 书签脚本 | 本扩展 |
|---|---|---|
| 读 HttpOnly Cookie | 不可能（`document.cookie` 读不到） | 可以（`cookies` 权限） |
| Gemini 的 `__Secure-1PSID`、Loomy 的登录态 | 拿不到 | 能拿到 |
| 识别当前站点 | 要自己保证在正确页面点 | 自动识别域名 |
| 规则更新 | 每个用户各自维护 | 改一版扩展即可 |

## 安装（Chrome / Edge，1 分钟）

1. 打开 `chrome://extensions/`（Edge 是 `edge://extensions/`）
2. 右上角打开 **开发者模式**
3. 点 **加载已解压的扩展程序** → 选择本目录（`webapi-cred-ext`）
4. 工具栏出现「WebAPI 凭证提取器」图标，可以钉住

> 用压缩包分发时，先解压再选目录；Chrome 只能加载解压后的目录。

## 用法

打开对应平台的页面并**确认已登录**，点扩展图标 → 点「提取」→ 点「复制结果」。
下方「拿到结果后怎么用」会按当前站点给出具体去处。

| 站点 | 产出 | 粘到哪 |
|---|---|---|
| `chat.deepseek.com` | 四字段 JSON：`auth_token` / `device_id` / `settings_token` / `cookie` | 网关面板 → WebAPI 接入 → duruoxian → ① 粘贴凭证 → 拆开填入 → 导入到 webAPI 服务 |
| `gemini.google.com` | 整行 Cookie（`k=v; k=v; …`） | 存成 `/vol1/@appdata/gemini-web2api/cookie.txt`，再重启 gemini-web2api 应用 |
| `loomy.xunfei.cn` | `{"auth":{"accessToken":"…"},"account":{"uid":"xunfei"}}` | 网关面板 → ⌨ JSON 登录 → 渠道选 Loomy |
| `chatglm.cn` / `www.kimi.com` / `chat.qwen.ai` | 四字段 JSON（**字段映射未逐个验证**，界面会提示） | 同 DeepSeek；缺字段时点「复制原始数据」反馈 |

## 几个设计取舍

- **不联网**：扩展只读当前页面的 Cookie 和 localStorage，结果只落在 popup 的文本框里，
  不向任何服务器发送。（manifest 里没有 `<all_urls>`，也没有任何 fetch。）
- **只读不写**：不修改页面、不注入脚本到运行中的业务逻辑，仅在点击时执行一次
  `dumpLocalStorage()` 读取键值。
- **Loomy 用扫描而不是写死键名**：它的 session 存放位置我没能从前端 bundle 里确认
  （几个 chunk 里都没有 `localStorage` 字样，可能走 Cookie），所以直接把
  「localStorage 全部值 + 全部 Cookie 值」扫一遍找 JWT 形状（`eyJ` 开头、含两个点）。
  命中即用；没命中会提示「复制原始数据」反馈，我再补精确规则。
- **Gemini 缺 Cookie 会给 warn**：明确列出缺哪几个（`__Secure-1PSID` /
  `__Secure-1PSIDTS` / `SAPISID`），避免拿着半截 Cookie 去怀疑别处。

## 已知限制

- 只支持 Manifest V3 的 Chromium 系浏览器；Firefox 需要改 `background`/权限写法。
- 平台改版会导致规则失效：表现为「没找到 userToken / 没扫到 JWT」，
  点「复制原始数据」把内容发我即可定位。
- 凭证等同于账号登录态，**只在自己的机器上用**，别外传。

## 目录

```
manifest.json   权限与站点白名单
popup.html      界面
popup.js        站点规则与提取逻辑（新增平台只需加一条 SITES 记录）
README.md       本文件
```
