# 规则引擎与故障模型

`Injector` 是线程安全的故障注入规则集 + 设备性能模型（`sync.Mutex` 保护）。FUSE
回调与 control server 并发查询/修改它。

## Rule 字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `Op` | string | 操作类型：`open`/`read`/`write`/`create`/`lookup`/`mkdir`/`rmdir`/`unlink`/`rename`/`getattr`/`statfs`/`getxattr`/`setxattr`/`removexattr`/`listxattr`；`""`=任意 op |
| `Path` | string | 挂载内相对路径**子串**；`""`=任意路径。如 `"blob.bin"` 命中 `"data/blob.bin"` |
| `Off`, `OffLen` | int64 | 仅 read/write：`OffLen<=0`=任意 offset（零值默认）；`OffLen>0`=区间 `[Off, Off+OffLen)`。精确点用 `OffLen:1` |
| `Errno` | syscall.Errno | 命中时返回的 errno（不可为 0） |
| `N` | int | 普通规则：前 N 次命中才注入（0=永久）；对 HealOnWrite 无意义 |
| `HealOnWrite` | bool | 把这条 read 规则变成“可修复坏扇区”（见下） |
| `ID` | int | 由 `Add` 自动分配（从 1 递增）；0=未分配 |

## Check 匹配

`Check(op, path, off)` 按 Add 顺序遍历规则，**首条命中即返回**（多条命中同一请求
时，Add 顺序决定优先级）。命中条件：`Op` 匹配、`Path` 子串匹配、`off` 落入区间
（仅 read/write 且 `OffLen>0` 时启用）。

## 普通规则（N 配额）

命中且 `remaining>0` → 返回 `Errno`，`remaining--`；`remaining==0` 后失效。`N=0`
表示永久（`remaining=-1`）。“前 N 次后自愈”由 `N` 表达。

## HealOnWrite 坏扇区模型（有状态）

真实硬盘语义：read 坏扇区→EIO；write 该区→备用扇区重映射→write 成功、后续 read
正常；备用耗尽→write 也 EIO。规则**持久保留**（不删除），带运行时状态 `healed`：

- read 命中 `HealOnWrite` 规则：`healed`→放行（读到重映射后数据）；`!healed`→返 `Errno`（EIO）。
- write 命中（同 Path+Off 区间）且 `!healed`：`spare!=0`→置 `healed=true`、`spare>0`
  时 `spare--`、放行 write；`spare==0`→返 `Errno`（备用耗尽，write 也失败）。
- write 命中已 `healed` 的规则→放行。

`HealOnWrite` 规则的 op 匹配放宽到 `{read, write}`（注入点是 read，write 触发治愈）。
正是 FSS raif `inlineRepair`（读 EIO → 重构 → 写回触发重映射）所依赖的语义。

## spare 备用预算

`Injector.spare`（int64，`-1`=无限，默认 `-1`）。每治愈一次消耗一格。`SetSpare(n)`
同步更新初始快照（故 `Refresh` 会还原到该值）。CLI: `faultfs set spare <mp> <n>`。

## Refresh

`Refresh()` 把所有规则状态还原到 Add 时的初始态：`healed=false`、`remaining=初始N`、
`spare=初始值`。规则配置不变。用于反复重放同一组故障（治愈→刷新→再次故障）。
CLI: `faultfs refresh <mp>`。

## 在线管理 API

| 方法 | 说明 |
|---|---|
| `Add(r Rule) int` | 追加规则，返回分配的 ID |
| `Delete(id int) bool` | 按 ID 删除 |
| `Clear()` / `Reset()` | 清空所有规则 |
| `List() []RuleView` | 规则视图快照（含 `healed`/`remaining`） |
| `Refresh()` | 重置所有规则到初始态 |
| `SetSpare(n int64)` / `Spare()` | 备用预算 |
| `SetProfile` / `SetSpeed` / `Profile` / `Speed` | 延迟模型（见 latency.md） |

Go 库直接调这些方法；CLI 通过 control socket 触发同样的方法（见 control.md）。
