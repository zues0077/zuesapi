# workbuddy-api v0.1.3

本版本在 v0.1.2 基础上并入三项实用功能，并修复了「Windows 版 exe 双击无法运行」的问题。

## 新增功能

- **JSON 登录**：在面板粘贴凭据 JSON 即可直接添加账号，无需走浏览器 OAuth / CDP / 本机客户端导入。
  同时兼容两种格式——嵌套（与 `auths/*.json` 一致）与扁平；`uid` 缺失时自动从 access_token(JWT) 推导，
  智谱清言（glm）渠道可仅填 `refreshToken`。落盘格式与加密方式和浏览器登录完全一致，token 仅存本机。
  适用于跨机器迁移账号、或 OAuth 回调在本机不可用时的兜底。
- **JSON 导出（获取凭据）**：把面板里已有账号的凭据导出为可粘贴的 JSON（明文 token，等同账号本身，
  请妥善保管、勿外传），用于迁移到另一台机器（配合 JSON 登录）或备份。
- **WorkBuddy 快速切号**：账号卡片新增切号按钮（⇄ / 📌），一键把指定账号置顶为其渠道的优先账号，
  后续新请求恒走它；再点一次取消置顶、恢复自动轮换。进行中的粘性会话不受影响，任务不丢失；
  置顶账号一旦冷却/禁用，自动回落到原策略，服务不中断。

## 修复

- **Windows exe 无法运行**：上游 v0.1.2 的 release 附件被 `multipart/form-data` MIME 信封包裹
  （首字节为 `------…` 而非 PE 的 `MZ`），导致 Windows 判定「不是有效的 Win32 应用程序」。
  本版通过 GitHub Actions 以标准 `go build` 产物 + 官方上传流程发布，资产为干净的 PE 可执行文件。

## 构建产物

由 CI 针对五平台自动构建并上传：`windows-amd64.exe`、`macos-arm64/amd64`（含 .app/.dmg）、`linux-amd64/arm64`。
