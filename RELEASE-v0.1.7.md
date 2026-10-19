# workbuddy-api v0.1.7

**回退版本**：移除 v0.1.4~v0.1.6 引入的「免费模型每日用量 / 重置时间」功能，面板行为恢复到 v0.1.3。

## 移除了什么

- 「模型列表和费率」页免费模型行上的 `今日 N 次` 与 `已用尽 · HH:MM 重置` 标注。
- 「运行统计 → 模型消耗」表在 v0.1.4 一度加过的「类型 / 额度」列（v0.1.5 已移除，本版一并清干净）。
- 后端为上述功能新增的模型级重置点采集与持久化（`stats.modelReset`、`MarkModelRateLimited`、`ModelQuota`、handler 里对 `6004`「将在 X 重置」的解析入库）。
- v0.1.6 对 WorkBuddyCN 免费模型识别（`x0.00` / 免费徽标）的修复也一并回退——`hy3`、`step-5-preview` 等在该页恢复显示为「unknown（上游未返回倍率）」。

## 保留不变

- v0.1.3 的全部功能：JSON 登录、JSON 导出（获取凭据）、WorkBuddy 快速切号。
- 干净可运行的 `windows-amd64.exe`（`MZ` 头，双击即用）。

## 构建产物

`workbuddy-api-windows-amd64.exe`。
