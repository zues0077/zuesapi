# 0.1.11 发版与真机验证记录

本文汇总 `v0.1.11` 的发版结果、本次并入的修复项，以及在真机（246，fpk 0.1.11 包）上完成的接口级验证。记录内容来自已完成的工作与核实记录，未作补充推断；凡无法验证的部分均已如实标注。

- 记录日期：2026-11-03
- 版本：`0.1.11`
- 验证环境：真机 246，fpk 包 `wb2apifusion-0.1.11-x86.fpk`

## 1. 发布信息

| 项目 | 值 |
| --- | --- |
| Tag | `v0.1.11` |
| Tag 指向提交 | `87f8a2e` |
| 附注 tag 对象 | `b0f2888353b6614896a0053d9da60278ffe204bd` |
| 远端 `main` | 同为 `87f8a2e` |
| 触发方式 | `workflow_dispatch`，ref `v0.1.11` |
| CI run | `41638284496`，结论 `success`，8 个 job 全绿 |
| Release id | `432692996` |
| Release 状态 | 非 draft、非 prerelease |
| published_at | `2026-11-03T12:50:54Z` |
| Release 说明来源 | 仓库内 `RELEASE-v0.1.11.md` |

### 资产清单（9 个）

| 资产 | 字节数 |
| --- | --- |
| `workbuddy-api-windows-amd64.exe` | 15,741,440 |
| `wb2apifusion-0.1.11-x86.fpk` | 5,573,439 |
| `wb2apifusion-0.1.11-arm.fpk` | 5,090,475 |
| `workbuddy-api-linux-amd64` | 18,218,459 |
| `linux-arm64` | 17,070,724 |
| `darwin-amd64` | 15,728,392 |
| `darwin-arm64` | 14,703,100 |
| `WorkBuddyAPI-0.1.11-amd64.dmg` | 9,698,768 |
| `arm64.dmg` | 8,760,151 |

文件头校验：`.exe` 头部为 `MZ`，`.fpk` 为 gzip。

### 版本常量

版本常量唯一来源为 `internal/app/app.go` 中的：

```go
const Version = "0.1.11"
```

与 tag `v0.1.11` 一致。

## 2. 本次并入的修复

| 提交 | 内容 |
| --- | --- |
| `bdfc65a` | 0.1.11 主体：导出加锁、userinfo 拒绝、Save 失败回滚 |
| `aaf5728` | U2：登录回调支持手工粘贴（容器/NAS 上 code 送不回时的兜底）+ 回调失败语义修正；`config.json` 已存在时改用文件中的凭据 |
| `10d3f73` | U3：release note 与最终行为对齐（删掉“面板改密会被向导值回滚”的旧结论） |
| `f24fda8` | U7：`StopGC` 等 GC goroutine 真正退出后才返回 |
| `b08bbd1` | U5+U6：GLM 批量导入验证预算（最多 20 次上游校验，用满后快速失败）+ 导出回执 `warnings` |
| `d80e453` | A：监听端口改为“首次启动注入、之后以 `config.json` 为准”（fpk + docker + 两侧回归脚本） |
| `4f19f72` | B：监听切换失败时把 bind 原文写日志并回显响应体（含中文提示） |
| `f368652` | RELEASE note 补端口归属与失败诊断 |
| `87f8a2e` | `serveLocked` 改为“先绑新地址、成功后再停旧”，失败时旧面板继续服务；同端口换网卡分支单独降级处理 |

## 3. 真机验证（246，fpk 0.1.11）

### 3.1 版本与运行状态

- 面板 `/api/auth/state` 可读到 `version=0.1.11`。
- `/api/state` 显示 `listen_host=0.0.0.0`、`listen_port=7865`、`running=true`。

### 3.2 serveLocked + B：监听切换失败时的保活与回显

操作：`POST /api/config/listen`，请求体 `{"host":"0.0.0.0","port":7863}`。

结果：

- 返回 HTTP 400，响应正文原文：

  ```
  监听 0.0.0.0:7863 失败：listen tcp 0.0.0.0:7863: bind: address already in use（端口已被其它程序占用…）
  ```

- `app.log` 中有同一行日志，另有 `access POST /api/config/listen status=400 dur=1ms`。
- 失败后原 7865 面板仍返回 200，`listen_port` 仍为 7865，配置未落盘。

结论：通过。

补充：7863 端口被 wb2api+ 占用，与飞牛运行设置中的提示一致。

### 3.3 导出 `warnings` 字段的实际表现

- 请求 `GET /api/account/export_all_json`，顶层键为 `accounts / count / exported_at / version`，**无** `warnings`。
- 源码中该字段仅在渠道加载失败（`errs` 非空）时写入；本次 3 个渠道全部成功，故不出现。
- 按用户决定**不改代码**；使用说明的口径写成“看日志的 `警告=N`，或改口径后始终返回 `warnings`”。

### 3.4 由用户自行操作、用户报告通过的项

以下三项由用户本人操作，用户报告“测试都通过了”。此处仅记录结论，不复述未核实的具体现象细节：

- A：端口以文件为准
- U2：授权后手工粘贴回调
- U5：GLM 无 uid 批量预算

### 3.5 无法验证项

- 程序启动不写启动日志，API 无运行时长字段，因此**无法直接从接口读出 commit / 启动时间**。
- 当前运行包的来源只能通过行为反推（失败后旧面板存活这一表现）间接判断，不能作为确证。

## 4. 遗留与注意事项

- 监听变更失败时旧面板保活；成功切换会同步 `Shutdown` 旧 server（最多等 2s），SSE 长连接会等到超时。
- 升级须知：
  - Docker 版 `config.json` 由 `/app` 迁到 `/data`；
  - 改过的密码需重设一次。
- 未做 / 未纳入本次范围：
  - 代理清单未重跑（按用户要求保持不动）；
  - `workbuddy2api-panel` 相关项不在本次范围；
  - U8 剩余观察项：TraeWork 旧导出缺 `machineId`/`deviceId`，升级后需重新导出导入。
- 网络提示：本机访问 GitHub 需经代理 `192.168.1.246:7890`。
