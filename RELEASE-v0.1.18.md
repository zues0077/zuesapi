# v0.1.18

## 自动别名 max_tokens 下限兜底（解决 thinking 模型正文空）

**问题**：Deva 等客户端若把「最大输出长度」设成 32/64，thinking 模型（如 `deva-free` 的免费模型）会把 token 全花在思考上，正文为空（`finish_reason=length`）。A/B 实测：32 时免费档 0/8、付费档 4/6 正文完整；256 时两档 14/14 全过。

**修复**：网关对系统自动生成的别名（`deva-main` / `deva-free`）在转发前把过小的 `max_tokens` 抬到 256：
- 只作用于 `auto_aliases` 登记的自动别名，普通模型和用户自定义别名一律不动（零误伤）；
- 请求值 ≥256 则尊重不动；上限封顶（`max_tokens_cap`）逻辑不变，两者共存；
- Chat / Messages / Responses 三个协议入口同一口径。

**用户侧效果**：Deva 里把模型填 `deva-main` 或 `deva-free`，「最大输出长度」即使设成 32 也能正常出正文，不必再手动调到 ≥256。

## 验证
- 8 条 Go 单测（抬起/尊重大值/零误伤/开关/三协议/封顶共存）+ build/vet/test/UI 全绿；
- 246 实机补丁 + 闭环实测 4/4：两个自动别名发 32 均返回正文（`deva-main` 思考 token 达 899 仍完整）、裸模型名发 32 仍 `length`（对照组证明零误伤）；
- 原始数据与报告见 `docs/experiments/max-tokens-256-ab-20261119/`（42 条 A/B + 4 条闭环，逐条可复核）。
