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

## HealOnWrite 坏扇区模型（有状态，按块治愈）

真实硬盘语义：read 坏扇区→EIO；write 该区→备用扇区重映射→write 成功、后续 read
正常；备用耗尽→write 也 EIO。规则**持久保留**（不删除），带运行时状态。

**整段 vs 按块**（由 `Add` 时的 `spareBlockSize` 决定并固化，避免后续改 blockSize 致标记错位）：

- **按块模式**（`spareBlockSize>1` 且 `OffLen>0`）：按 `ceil(OffLen/spareBlockSize)` 切块，每块独立
  跟踪治愈状态。**部分覆盖 write 只治愈其实际写入的块**——按请求 `[off,off+len)` 与坏区的交集映射
  到块，未覆盖块 read 仍 EIO。符合真硬盘 UNC 语义：上层 `inlineRepair` 若只部分重写，未重写部分
  读到的应是错误/UNC，而不是 backing 旧的正确数据（旧实现整段一次性 `healed` 会给出虚假乐观结果）。
- **整段模式**（`spareBlockSize<=1` 或 `OffLen<=0`；或按块模式下块数超 `maxHealedBlocks` 阈值
  回退以防 `make([]bool,N)` OOM）：单个 `healed` 标志，write 命中即整段治愈（兼容纯次数语义）。

`Check(op, path, off, length)` 多带 `length`（read/write 请求字节数；其他 op 传 0），仅按块治愈判定用：

- read 命中：整段模式 `healed`→放行 / `!healed`→EIO；按块模式：请求覆盖的块**全治愈**才放行，
  任一未治愈→EIO（保守：跨好坏块整体 EIO）。
- write 命中且未全治愈：`need` = 本次 write **新覆盖的未治愈块数**；`spareCount==-1` 或
  `>=need`→置这些块 healed、`spareCount-=need`、放行；否则返 `Errno`（备用不足，**不治愈任何块**，原子）。
- write 命中已全治愈→放行。

`RuleView.HealedBlocks/TotalBlocks` 暴露治愈进度（如 `1/2`）；`Healed` 仅在全治愈时为 true。
`HealOnWrite` 规则的 op 匹配放宽到 `{read, write}`（注入点是 read，write 触发治愈）。
正是 FSS raif `inlineRepair`（读 EIO → 重构 → 写回触发重映射）所依赖的语义。

> 坏扇区模型假设 backing 数据可信（注入的 EIO 是"假坏"）。若 backing 真坏（非 tmpfs），`healed`
> 状态会与真实 EIO 不一致，致自愈死循环——见 [capacity.md](capacity.md) 的 backing 选择建议。

## spare 备用块预算

备用块用「**块数量 + 块大小**」表达：`spareCount` 个 `spareBlockSize` 字节的块。
`spareCount=-1` 无限，**默认 `0`（无备用，需显式分配）**。治愈一段坏区时按
`ceil(坏区长度 / spareBlockSize)` **整块消耗**（`spareBlockSize<=1` 或坏区长度<=0 时算 1 块，
等价旧的"每治愈一格"）。预算不足（剩余块数 < 本次所需）时 write 也返 `Errno`。

所有 setter 同步更新初始快照（故 `Refresh` 复位到**最近一次 set 的值**）：

| 方法 | 说明 |
|---|---|
| `SetSpare(n int64)` | `n` 个默认块（`blockSize=1`，兼容旧的纯次数语义） |
| `SetSpareBlocks(count, blockSize int64)` | `count` 个 `blockSize` 字节的块（`blockSize<1` 钳到 1；`count=-1` 无限） |
| `Spare() int64` / `SpareBlockSize() int64` | 剩余块数 / 每块字节数 |

`ParseSpareSpec("8*4KiB")`→`(8, 4096)`；纯数量 `"8"`→`(8, 1)`（兼容）；`"-1"`→无限。
CLI: `faultfs set spare <mp> <spec>`、`mount --spare <spec>`、`add badsector --spare <spec>`。

## Refresh

`Refresh(opts RefreshOptions) RefreshResult` 把所有规则状态还原到 Add 时的初始态
（`healed=false`、`remaining=初始N`）、spare 还原到最近一次 set 的初始值；默认同时把
profile/speed 复位到初始值（`opts.SkipLatency=true` 时跳过——CLI `--keep-latency`）。
latency 无消耗路径，current 恒等于 initial，故 latency 复位通常 no-op；保留以兑现"重置回初始值"
语义。

返回 `RefreshResult{Entries []ResetEntry}`：**仅记录实际发生变动的条目**（规则按 ID、spare、
latency），每条带 `Before`/`After`，不留静默聚合编号。CLI `refresh` 把这些打到 stderr。
规则配置不变；用于反复重放同一组故障（治愈→刷新→再次故障）。

```go
type RefreshOptions struct{ SkipLatency bool }
type ResetEntry struct{ What string; ID int; Before, After string } // What ∈ "rule"|"spare"|"latency"
type RefreshResult struct{ Entries []ResetEntry }
```

## 在线管理 API

| 方法 | 说明 |
|---|---|
| `Add(r Rule) int` | 追加规则，返回分配的 ID |
| `Delete(id int) bool` | 按 ID 删除 |
| `Clear()` / `Reset()` | 清空所有规则 |
| `List() []RuleView` | 规则视图快照（含 `healed`/`healedBlocks`/`remaining`） |
| `Refresh(opts RefreshOptions) RefreshResult` | 重置规则/spare/（默认）性能参数到初始态，返回变动条目 |
| `SetSpare(n)` / `SetSpareBlocks(count, blockSize)` / `Spare()` / `SpareBlockSize()` | 备用块预算 |
| `SetCapacity(capacity)` / `Capacity()` | 模拟容量上限（mount 固化、不进 Refresh；见 [capacity.md](capacity.md)） |
| `SetProfile` / `SetSpeed` / `Profile` / `Speed` | 延迟模型（见 latency.md） |

Go 库直接调这些方法；CLI 通过 control socket 触发同样的方法（见 control.md）。
