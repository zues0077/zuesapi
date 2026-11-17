# zuesapi v0.1.15

本版是「WebAPI 凭证接入」这条线的一次集中修复。背景：把 DeepSeek / 智谱 / Kimi / 讯飞星火
四个网页版平台接进网关时，从「扩展取不到凭证」到「渠道建了但模型不出现」一路踩坑，
本版把其中属于本仓库的问题全部修掉，并把已验证不可用的源从界面上摘除。

## 修复

### 1. 导入平台账号报 404（导入根本发不出去）
`isPrivateBaseURL` 只 `TrimRight("/")`，而面板传的地址是 `http://127.0.0.1:8800/v1`，
于是请求打到 `/v1/admin/api/login` → 404，面板显示「登录目标服务失败（HTTP 404）」。
新增 `normalizeAdminBase()`，统一剥掉 `/`、`/v1`、`/api` 三层；localhost 分支同样走它。

### 2. 自定义渠道的模型清单缺席（加了渠道，模型里却没有）
`rt.fetchModels` 走的是 `Pool.Pick()` 只取一个账号，而 customopenai 渠道是**多实例聚合**
（deepseek / 智谱清言 / kimi / gemini / spark 各有自己的模型），其它实例的模型被整批藏掉。
新增 `Runtime.fetchModelsAllInstances()`：遍历 `Pool.List()` → `AuthByUID` → 逐个 `FetchModels`
→ 按 ID 去重合并，并写入 models 缓存与 health(live)。实测 `customopenai/*` 条目 5 → 18。

### 3. 面板「添加渠道」后静默丢失
添加后不核对，失败也关闭弹层，用户以为成功。现在添加完成会**回读 `/api/state` 确认**，
不存在就明确报错并保持弹层；`api_key` 增加必填校验（空 key 的渠道必然 401）。

### 4. 两个按钮语义混淆
「导入到 webAPI 服务」与「添加为 webAPI 账号」都在说"添加"。改为 ① / ② 编号并附解释：
① 只把登录态写进上游反代服务（平台未登录时才需要）；② 才在网关里建渠道（必须做）。
高级设置里的「平台」改名「模型平台」，与 ① 的平台选择区分。

### 5. 渠道卡片没有补登录态的入口
webAPI 渠道卡片原来只有 ⏸/✕，用户只能删掉重建（而重建也补不上账号）。
新增 **🔑 补登录态**：跳到对应源卡片并展开 ① 区，toast 提示「只点 ① 导入，完成后取消」。

### 6. ① 选平台不带出登录网址
更新逻辑挂错了控件（挂到高级设置的 `.wa-plat`），且只监听 `onclick` —— `<select>` 取值变化
不触发 click。改为 `box.onchange` 监听 `.wa-prov`，并新增 `PLATFORM_LOGIN` 映射，
选平台即显示可点的登录地址。

### 7. 导入接口无日志
面板无访问日志，失败时无从定位。导入路径补了三行日志：尝试 / 失败原因 / 成功。

## 移除（已验证不可用）

- **gemini-web2api 源卡片整张移除**：cookie 拿不到 `SAPISID`（浏览器只有
  `COMPASS`+`ACCOUNT_CHOOSER` 那套半登录组合），服务端校验一直被 Google 重定向到登录页。
  同时清掉 `index.html` 教程与 `custom-channel-helper.html` 里的 gemini 预设。
- **duruoxian 的「通义千问」平台选项移除**：其 qwen 适配器依赖 **WebView2**（Windows 组件）
  做浏览器中继，飞牛/Linux 上永远起不来（日志固定 `千问中继启动超时`，每次 45s）；
  新版 `www.qianwen.com` 又加了 baxia 请求签名（`clt-acs-sign` 每次现算、与 timestamp 绑定），
  无法自建反代；开源项目指向的老接口 `api.tongyi.com` 已废弃，只回「请使用最新版」。

## 校正

平台默认模型按实测调整：`kimi-k3` 换 `kimi-k2.8`/`kimi-k2.6`（k3 实测 200 但内容为空），
智谱补 `GLM-5.3-Flash`，「全部平台」去掉 `qwen3.7-plus`。

## 配套（不在本包内）

浏览器扩展 `webapi-cred-ext` 已到 1.5.1：智谱/Kimi 按适配器真实字段映射提取
（智谱 refresh→auth_token，Kimi refresh→settings_token），新增「结构诊断」，
并补 `*.qianwen.com` / `*.qwen.ai` 域权限（缺权限时 `chrome.cookies` 静默返回空）。

## 实测

`/v1/models` 的 `customopenai/*` 共 10 个模型全部返回真实内容：
`deepseek-chat`、`deepseek-reasoner`、`GLM-5.3`、`GLM-5.3-Flash`、
`kimi-k2.8`、`kimi-k2.6`、`spark-3.5-max`、`spark-pro`、`spark-v2.0`、`spark-lite`。
