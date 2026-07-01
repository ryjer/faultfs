# faultfs Go 库用法（模拟各类文件系统错误）

面向 **Go 用户**的库指南：把 faultfs 当作被测系统（如 [FSS](https://github.com/ryjer/fss)
的 raif）的某块物理盘，被测系统对该盘的 syscall 经内核路由到 faultfs，返回的 errno 就是它
看到的**真实文件系统错误**（`os.PathError{Err: syscall.EIO}`），与底层真盘报错不可区分——
这强于在被测系统内部伪造错误的单元测试钩子。

> faultfs 是独立 go module（`github.com/ryjer/fss/faultfs`），仅依赖 go-fuse + 标准库，
> 可脱离 FSS 单独复用。CLI 用户请看 [cli.md](cli.md)。

## 0. 快速上手

```go
import (
	"syscall"
	"testing"

	"github.com/ryjer/fss/faultfs"
)

func TestSomething(t *testing.T) {
	inj := faultfs.NewInjector()
	mp := faultfs.MountT(t, inj) // 挂载；无 /dev/fuse 时自动 t.Skip；卸载已注册到 t.Cleanup

	// 把 mp 当作被测系统的一块盘：
	//   disks := raif.Disks{t.TempDir(), mp, t.TempDir()}
	// 被测系统读 mp 里的文件时，命中规则的那块盘会返回真实 EIO。
}
```

`MountT` 把 `inj` 挂到一个临时挂载点（backing 也是临时目录），返回挂载点路径。非测试场景
用 `faultfs.Mount(mp, backing, inj) (cleanup func(), err error)` 自行管理生命周期。

一个 `*Injector` 可同时持有**任意多条 `Rule`**，同一时刻多种错误可在不同文件/位置/op 上并存。
`Check` 按 `Add` 顺序遍历，首条命中即返回（多条命中同一请求时，Add 顺序决定优先级）。

下面逐类给出注入配方。约定 `inj` 是已建好的 `*faultfs.Injector`，`mp` 是挂载点，规则在挂载
**前后**添加均可（在线生效）。

## 1. 读返回 EIO（坏读）

```go
inj.Add(faultfs.Rule{
	Op: faultfs.OpRead, Path: "blob.bin", Errno: syscall.EIO,
})
// 被测系统读 mp/blob.bin → os.PathError{Err: syscall.EIO}
```

## 2. 写返回 ENOSPC（磁盘满）

```go
inj.Add(faultfs.Rule{
	Op: faultfs.OpWrite, Errno: syscall.ENOSPC, // Path 留空 = 任意文件
})
```

## 3. 创建 / 打开 / 元数据 等返回指定 errno

`Op` 取值：`OpOpen / OpCreate / OpRead / OpWrite / OpLookup / OpMkdir / OpRmdir / OpUnlink /
OpRename / OpGetattr / OpStatfs / OpSetattr / OpGetxattr / OpSetxattr / OpRemovexattr /
OpListxattr / OpFsync / OpFlush`；留空表示任意 op。

```go
inj.Add(faultfs.Rule{Op: faultfs.OpCreate, Errno: syscall.EROFS})   // 创建 → 只读文件系统
inj.Add(faultfs.Rule{Op: faultfs.OpOpen, Path: "blob.bin", Errno: syscall.ESTALE}) // 打开 → 失效句柄
inj.Add(faultfs.Rule{Op: faultfs.OpStatfs, Errno: syscall.ENOSYS})  // statfs → 不支持
```

`Errno` 取任意 `syscall.Errno`（`EIO/ENOSPC/EROFS/ESTALE/EUCLEAN/ENODEV/EACCES/EPERM/…`）。

## 4. 精确到文件位置（offset 区间）

`Off`/`OffLen` 仅对 read/write 生效：`OffLen<=0`（零值默认）=任意 offset；`OffLen>0`=仅当请求
起始 offset 落入 `[Off, Off+OffLen)` 才命中。精确命中某个 offset X 用 `Off:X, OffLen:1`。

```go
inj.Add(faultfs.Rule{
	Op: faultfs.OpRead, Path: "blob.bin",
	Off: 4096, OffLen: 4096,        // 只命中 [4096, 8192) 这段条带
	Errno: syscall.EIO,
})
```

## 5. 前 N 次注入后自愈（`N`）

`N>0`：仅前 N 次命中注入，之后该规则失效（"坏几次后自己好了"）。`N=0`（默认）=永久。

```go
inj.Add(faultfs.Rule{
	Op: faultfs.OpRead, Path: "blob.bin", Errno: syscall.EIO, N: 3, // 前 3 次读 EIO，第 4 次起正常
})
```

## 6. 可修复坏扇区（`HealOnWrite`，有状态）

真实硬盘语义：读坏扇区→EIO；写该区→备用扇区重映射→write 成功、后续 read 正常；备用预算耗尽
→write 也 EIO。把一条 read 规则的 `HealOnWrite` 置 true 即启用。规则持久保留，带运行时状态
`healed`。

```go
inj.Add(faultfs.Rule{
	Op: faultfs.OpRead, Path: "blob.bin", Off: 4096, OffLen: 4096,
	Errno: syscall.EIO, HealOnWrite: true,
})
inj.SetSpare(4) // 备用扇区预算；-1=无限（默认）
// 被测系统：读 [4096,8192) → EIO；重写该区 → 治愈；再读 → 正常。
// 正是 FSS raif inlineRepair（读 EIO → 重构 → 写回触发重映射）所依赖的语义。
```

## 7. 重放：`Refresh` 还原到初始态

`Refresh` 把所有规则状态还原到 `Add` 时的初始态：`healed=false`、`remaining=初始 N`、
`spare=SetSpare 设定的初始值`。规则配置不变。用于"治愈→刷新→再次故障"反复重放同一组场景。

```go
inj.Add(faultfs.Rule{Op: faultfs.OpRead, Path: "blob.bin", Errno: syscall.EIO, HealOnWrite: true})
inj.SetSpare(4)
// ... 跑一轮，治愈消耗了 1 格 spare ...
inj.Refresh() // healed 全部复位，spare 回到 4；下一轮可再次故障
```

## 8. 设备性能模拟

延迟模型在每个操作的透传之后 sleep。预设档覆盖三类典型设备；也可用两个手动旋钮（随机寻址
延迟 + 顺序读写速度）自定义，再叠加全局倍速。

```go
inj.SetProfile(faultfs.ProfileHDD)   // 预设：none / memory / ssd / hdd
inj.SetSpeed(2.0)                    // 全局倍速：1.0 正常、>1 慢放、<1 快放

// 手动旋钮：随机寻址 8ms，顺序读写 100MiB/s
inj.SetProfile(faultfs.ProfileFromKnobs(8*time.Millisecond, 100*faultfs.MiB))
```

### 按 backing（tmpfs）上限钳制

faultfs 通过叠加延迟模拟更慢设备，最快只能到 backing 本身（最强即 tmpfs）。库用户可手动校准
并钳制：目标比 backing 还快的部分被钳到 0 并返回告警。

```go
rand, bw, err := faultfs.Calibrate(backingDir)            // 实测 backing 随机延迟 + 顺序带宽
if err == nil {
	adj, warns := faultfs.AdjustProfile(target, rand, bw)  // 钳制 + 告警
	inj.SetProfile(adj)
	if len(warns) > 0 {
		t.Logf("性能参数被钳制到 backing：%v", warns)
	}
}
// 单位解析：faultfs.ParseLatency("8ms")、faultfs.ParseSpeed("100M")（字节/秒）
```

## 9. 在线管理 API 速查

| 方法 | 说明 |
|---|---|
| `Add(r Rule) int` | 追加规则，返回分配的 ID |
| `Delete(id int) bool` | 按 ID 删除 |
| `Clear()` / `Reset()` | 清空所有规则 |
| `List() []RuleView` | 规则视图快照（含 `Healed`/`Remaining`） |
| `Refresh()` | 重置所有规则到初始态（含 spare） |
| `SetSpare(n int64)` / `Spare() int64` | 备用扇区预算（`-1` 无限） |
| `SetProfile` / `Profile` | 延迟模型 |
| `SetSpeed` / `Speed` | 全局倍速 |
| `Calibrate(dir)` / `AdjustProfile` / `CalibratedFloor()` | backing 校准与钳制 |

## 10. 非 Go 进程也可驱动（control socket）

挂载守护进程（`faultfs mount` 或 `Mount`+自起 control server）在每个挂载点开一个 unix socket，
任何能发 JSON 的进程都能在线改规则——便于在测试运行中途由外部脚本/AI 注入故障：

```go
resp, err := control.Send(control.SocketPath(mp), control.Req{
	Cmd: control.CmdAddRule, Op: faultfs.OpRead, Path: "a.bin", Errno: int(syscall.EIO),
})
// resp.ID 是分配的规则 ID。协议详见 spec/control.md，CLI（doc/cli.md）即基于此。
```
