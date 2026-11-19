# zuesapi v0.1.16

本版修的是「应用在飞牛 NAS 上装得上、却起不来」这一类问题，外加一处在读数时才暴露的
清理逻辑空转。v0.1.15 的 FPK 是在这几处修复之前打的，从 Release 重装会复现故障，请用本版。

## 修复

### 1. 面板显示「已停止」，实际进程还活着（启动失败的真因）
`cmd/main` 停止旧进程时用 `pgrep -f "<app>/bin/<binary>"` 匹配，但进程实际命令行是
`/var/apps/<app>/target/bin/<binary>` —— 而 `/var/apps/<app>/target` 与 `/vol2/@appcenter/<app>`
是**同一目录的两种拼写**（symlink）。于是匹配不到残留进程，旧进程继续占着 7865，
新进程 bind 失败退出，`appcenter-cli` 在有无 PID 文件上判断失误，界面显示「已停止」，
服务其实还在跑旧代码。

- 改为按 `/proc/<pid>/exe` 的 `readlink -f` 结果比对，两种拼写归一后等价；
- 停止先 `TERM`，5 秒未退再 `KILL`，并排除 ` (deleted)` 状态；
- 启动失败时若端口占用者**就是本应用的二进制**，写入 PID 文件并返回成功（接管已有实例），
  不再让「自己占着自己的端口」变成启动失败。

### 2. 启动失败时面板只说「失败」，没有任何线索
`app.log` 里的 `listen tcp :7865: bind: address already in use` / `fatal` / `panic`
此前不会出现在面板能看到的 `info.log` 里。现在启动失败会把这几类行抄进 `info.log`，
一眼能看出是端口占用、配置错误还是崩溃。

### 3. FPK 内的 `cmd/main` 变成 CRLF，Linux 报 `bad interpreter`
`fpk/*/cmd/*` 与 `wizard/*` 是没有扩展名的脚本，`.gitattributes` 里只有 `*.sh eol=lf`，
盖不到它们；Windows 上 `core.autocrlf` 会把检出内容转成 CRLF，打进包后在 NAS 上执行
报 `/bin/bash^M: bad interpreter`。已为这两类路径显式指定 `eol=lf`。

### 4. 移除 ledger 里永不生效的按天裁剪
`trimCurrentMonth` 只读**当月分段**，而当月最老的条目（本月 1 号）永远晚于
「今天 − keepDays」（keepDays ≥ 当月天数），快速路径每次命中，一行都裁不掉 ——
`keepDays=30` 与 `60` 的量测结果逐行相同，属永久空转。整套删除（含 `findLineTs`、
`parseTs`、`indexByte`、`indexBytes` 与那条把 40 天前的行塞进当月文件的无效测试）。

保留窗口现在只由 `keepMonths=1` 决定：按整月分段删除，保留「上月 + 当月」，
实际跨度 29~62 天。量级参考：credit ≈ 140 KB/月、usage ≈ 22 KB/月。

## 实测

246 上以本版 `cmd/main` + 0.1.15 二进制验证：`appcenter-cli list` 显示 running，
`:7865` LISTEN，12 个账号 / 4 个渠道 / 10 个模型，`GLM-5.3` 真实调用返回「2」。
`go build ./...`、`go vet ./internal/ledger/`、`go test ./internal/ledger/` 全通过。
