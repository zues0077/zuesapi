# v0.1.9

并入上游 sddvcm/workbuddy-api v0.1.6 全部更新（本仓库此前停在 v0.1.3 基线）：

## 网关 / 稳定性（来自上游）
- **图片压缩（imageslim）**：请求里的图片自动压缩，修复「图片撑爆上下文」导致的请求失败
- **DeepSeek Harness 修复**：脱敏 DSH 身份句，修复 DeepSeek 桌面版调用 workbuddy 渠道被 11-128 / 11128 拦截
- **账本修复**：整包到期误记消耗、5xx 罚号等 5 项（credit pool 收缩逻辑 + 测试）
- **僵尸进程修复**：启动时绑端口失败直接退出；修复重复启动留下无服务进程

## 本仓库定制（保留）
- JSON 登录 / JSON 导出凭据 / WorkBuddy 快速切号
- 免费模型额度标注与用尽重置时间

## 产物
- `workbuddy-api-windows-amd64.exe`：Windows 单文件程序（带图标资源）
- `wb2apifusion-0.1.9-{x86,arm}.fpk`：飞牛 NAS 应用包（appname 沿用 wb2apifusion，端口 7865，**在 NAS 上直接升级安装即可，数据与账号保留**）
- macOS dmg ×2
