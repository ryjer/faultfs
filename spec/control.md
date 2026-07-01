# 控制协议与 CLI

faultfs 挂载守护进程在每个挂载点上启动一个 control server（unix socket），CLI 与
外部进程作为客户端通过 JSON 协议在线操控规则引擎与延迟模型。control 是纯协议 +
传输层：它不 import 父 package faultfs，而通过 `func(Req) Resp` 回调把请求交回挂载
方处理（`handleControl`），避免循环依赖。

## SocketPath

`SocketPath(mp)` 把挂载点稳定映射到一个 socket 路径：

- `$XDG_RUNTIME_DIR/faultfs/<fnv64(mp) 前16hex>.sock`
- 回退 `/tmp/faultfs-<uid>/<fnv64(mp) 前16hex>.sock`

## 协议（JSON over unix socket，一请求一连接）

### Cmd

| Cmd | 说明 |
|---|---|
| `add-rule` | 加规则（按 Req 字段构造 Rule），Resp 返回 `ID` |
| `delete-rule` | 按 `ID` 删除；未命中 Resp `Err="rule id <id> does not exist"`（纯英文） |
| `clear` | 清空所有规则 |
| `list-rules` | Resp 返回 `Rules []RuleView`（含 `healed`/`remaining`） |
| `refresh-rules` | 重置规则/spare/（默认）性能参数到初始态；`SkipLatency` 跳过 latency 复位（CLI `--keep-latency`）；Resp `Resets []ResetView` 携带每条变动（规则 ID/spare/latency 的 Before→After） |
| `set-latency` | 按 `Profile` 设档、按 `Speed`（`HasSpeed`）设倍速、按 `RandNs`/`SeqBw`（`HasRand`/`HasSeq`）设手动性能旋钮（`--profile` 与 `--rand`/`--seq` 互斥）；按 backing 钳制后写入，`Warns []string` 携带钳制告警（逐条）；`Speed<=0` 时追加一条明示告警（钳为 1.0） |
| `set-spare` | 按 `Spare`（`HasSpare`）+`SpareBlockSize`（0→1）设备用块预算 |
| `status` | 精简快照：Resp 返回 `Rules`/`Profile`/`Speed`/`Spare`/`SpareBlockSize` |
| `dump` | 全量快照：Resp 返回 `Dump *DumpView`（规则完整配置 + 挂载元信息 + 完整 profile 字段） |

### Req 字段

`Cmd`、`Op`、`Path`、`Off`、`OffLen`、`Errno`(int)、`N`、`HealOnWrite`、`ID`、
`Profile`、`Speed`/`HasSpeed`、`RandNs`/`HasRand`（随机寻址延迟，纳秒）、`SeqBw`/`HasSeq`
（顺序读写速度，字节/秒）、`Spare`/`HasSpare`、`SpareBlockSize`（set-spare 每块字节数，0→1）、
`SkipLatency`（refresh 跳过 profile/speed 复位）。各 `Has*` 区分“未设”与 0。`Errno` 用 int
表达 `syscall.Errno`（可序列化）。

### Resp 字段

`OK`、`Err`、`Warns []string`（非致命告警，如性能参数被钳制到 backing 上限；逐条输出）、`ID`、
`Rules []RuleView`、`Profile`、`Speed`、`Spare`、`SpareBlockSize`、`Resets []ResetView`
（refresh 的变动条目）、`Dump *DumpView`（仅 `dump` 命令）。

### ResetView（refresh 返回的变动条目）

`What`（`"rule"`（含 `ID`）/`"spare"`/`"latency"`）、`Before`、`After`。仅记录实际变动的条目。

### DumpView（dump 命令返回的全量快照）

`Rules`、`MountPID`、`Backing`、`Socket`、`MountTime`(RFC3339)、`ProfileName`、
`Speed`、`Spare`、`SpareBlockSize`、`ProfileFields map[string]string`（完整 LatencyProfile 各字段名→值）。
挂载元信息（PID/backing/socket/挂载时刻）由 `mountInternal` 在挂载时捕获，经
`handleControl` 闭包传递给 dump 处理分支。

## CLI（`faultfs`）

| 子命令 | 说明 |
|---|---|
| `mount <backing> <mp> [--detach] [--rand D] [--seq S] [--spare spec]` | 挂载守护（前台；`--detach` 后台 fork，返回前等 control socket 就绪）。`--rand`/`--seq` 设**初始**性能参数（启用性能模拟；省略则 ProfileNone 直透 backing）；`--spare` 设初始备用块预算（省略则 0）。均作 refresh 的复位目标 |
| `unmount <mp>` | `fusermount3 -u`；挂载进程随后自动退出（server.Wait 返回） |
| `add <mp> [flags]` | 加规则，打印分配的 ID。flags：`--op --path --off --off-len --errno --n --heal-on-write` |
| `add badsector <mp> --path --off --len [--spare spec]` | 封装为 `--heal-on-write` read EIO 规则（坏扇区：read EIO，write 治愈）。`--spare` 同设备用块预算（不带则不改当前预算，默认 0 → 需先 `set spare` 才能治愈） |
| `rm <mp> <id>` | 按 ID 删规则；未命中报 `Error: rule id <id> does not exist`（纯英文） |
| `clear <mp>` | 清空 |
| `refresh <mp> [--keep-latency]` | 重置规则/spare/（默认）性能参数到初始态；`--keep-latency` 跳过 latency 复位。每条变动打到 stderr（无静默编号） |
| `list <mp>` | 列出规则与运行时状态 |
| `status <mp> [--json]` | 精简概览（规则数/spare/speed/profile + 每规则一行）；`spare` 形如 `8*4KiB`/`8`/`0`/`unlimited` |
| `dump <mp> [--json]` | 全量诊断快照（挂载元信息 + 完整规则 + 完整 profile 字段），适合日志沉淀 |
| `set latency <mp> [--profile X] [--speed N] [--rand D] [--seq S]` | 设备档 / 倍速 / 手动性能旋钮（设备固有属性）。`--profile` 与 `--rand`/`--seq` 互斥；`--rand` 不可为负，`--seq` 须 ≥1 B/s（`0`=不限速）；`--speed<=0` 明示告警并钳为 1.0；超出 backing(tmpfs) 上限时告警并钳制 |
| `set spare <mp> <spec>` | 备用块预算（`<count>*<size>` 如 `8*4KiB`，或纯数量 `8`；`-1` 无限）；`refresh` 会还原到该初始值 |

CLI 错误已全局静默 cobra 的 `Usage:` 刷屏（`SilenceUsage`），仅保留 `Error: <msg>` 一行。

`--errno` 接受名称（`EIO`/`ENOSPC`/`EROFS`/`ESTALE`/`EUCLEAN`/`ENODEV`/`EACCES`/
`EPERM`/`ENOSYS`/`EFBIG`/`EDQUOT`，以及 xattr 相关的 `ENODATA`/`ENOTSUP`/`EOPNOTSUPP`/
`ERANGE`/`E2BIG`，大小写不敏感）或数字。

## Go 客户端

```go
resp, err := control.Send(control.SocketPath(mp), control.Req{
    Cmd: control.CmdAddRule, Op: "read", Path: "a.bin", Errno: int(syscall.EIO),
})
```
