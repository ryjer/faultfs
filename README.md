# faultfs

**faultfs** 是一个可编程故障注入的 FUSE loopback 文件系统，专供需要**真实文件
系统错误**（EIO/ENOSPC/EROFS/ESTALE/…）与**可调设备性能**的测试使用。它把一个
“按规则返回任意 errno、按延迟模型 sleep”的 loopback 挂到某挂载点，backing 目录
内容透传。把它当作被测系统（如 [FSS](https://github.com/ryjer/fss) 的 raif）的某
块物理盘，被测系统对该盘的 syscall（`Open`/`Read`/`Write`/`Getattr`/`Statfs`/
xattr/`Create`/`Mkdir`/`Unlink`/`Rename`）经内核路由到 faultfs，返回的 errno 就是
它看到的真实文件系统错误（`os.PathError{Err: syscall.EIO}`），与底层真盘报错不可
区分——这强于在被测系统内部伪造错误的单元测试钩子。

## 能力一览

- **任意 errno 注入**：按 `(op, path, off)` 精确匹配，EIO/ENOSPC/EROFS/ESTALE/…
  任选；多条规则可**同时并存**，各自命中不同文件/位置/op。
- **可修复坏扇区模型**（`HealOnWrite`）：read 坏扇区→EIO；write 该区→备用扇区
  重映射→治愈、后续 read 正常；备用耗尽→write 也 EIO。规则**有状态**，可用
  `Refresh` 重置回初始态反复重放。
- **设备性能模拟**：`LatencyProfile` 预设 `memory`/`ssd`/`hdd`（随机/顺序延迟 +
  可选带宽），外加全局 `speed` 倍速（快放/慢放）。
- **在线管理**：Go 库 API + per-mount control socket + `faultfs` CLI 增删/刷新
  规则、调延迟、设备用预算——非 Go 程序与 AI 也能驱动。

## 作为 Go 库

```go
inj := faultfs.NewInjector()
mp := faultfs.MountT(t, inj)                     // 挂载；无 /dev/fuse 自动 t.Skip
inj.Add(faultfs.Rule{Op: faultfs.OpRead, Path: "blob.bin",
    Off: 4096, OffLen: 1, Errno: syscall.EIO, N: 1})
disks := raif.Disks{t.TempDir(), mp, t.TempDir()} // 把 fault 挂载点当作其中一块盘
// …被测系统读 blob.bin 时，命中规则的那块盘对它返回真实 EIO…
```

坏扇区（read EIO、write 治愈）：
```go
inj.Add(faultfs.Rule{Op: faultfs.OpRead, Path: "blob.bin", Off: 4096, OffLen: 4096,
    Errno: syscall.EIO, HealOnWrite: true})
inj.SetSpare(4) // 备用扇区预算；-1=无限（默认）
// …测试后…
inj.Refresh()   // 所有规则还原到初始（healed/remaining/spare 全重置）
```

设备性能：
```go
inj.SetProfile(faultfs.ProfileHDD)
inj.SetSpeed(2.0) // 慢放 2 倍
```

## CLI

```sh
faultfs mount <backing> <mp> [--detach]            # 挂载守护；--detach 后台
faultfs add <mp> --op read --path a.bin --errno EIO            # 加规则，打印 ID
faultfs badsector <mp> --path a.bin --off 4096 --len 4096      # 坏扇区（read EIO, write 治愈）
faultfs latency <mp> --profile hdd --speed 2.0                 # 设备档 + 倍速
faultfs spare <mp> 4                                          # 备用扇区预算（-1 无限）
faultfs list <mp>                                             # 查看规则与运行时状态
faultfs status <mp>                                           # 精简概览（规则/spare/speed/profile）
faultfs dump <mp> [--json]                                    # 全量诊断快照（挂载元信息+完整 profile）
faultfs refresh <mp>                                          # 重置所有规则到初始态
faultfs rm <mp> <id> | faultfs clear <mp>                      # 删除
faultfs unmount <mp>
```

## 规格

详见 [`spec/`](spec/)：
- [spec/injector.md](spec/injector.md) — 规则引擎：字段、匹配、坏扇区、Refresh、在线 API
- [spec/latency.md](spec/latency.md) — 性能模型：profile、speed、顺序/随机
- [spec/control.md](spec/control.md) — 控制协议 + CLI 子命令

## 依赖与平台

仅依赖 `go-fuse` + `cobra`（CLI）+ 标准库。需 `fuse3` + `/dev/fuse`（缺失则挂载相关
测试自动 skip）。faultfs 是独立 go module（`github.com/ryjer/fss/faultfs`），可脱离
FSS 单独复用。
