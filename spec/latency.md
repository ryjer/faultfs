# 设备性能模型

faultfs 可按延迟模型在每个操作的**透传之后** sleep，模拟设备响应时间。默认
`ProfileNone`（全 0，不延迟），保证故障注入测试不受影响。

## LatencyProfile 字段

| 字段 | 说明 |
|---|---|
| `ReadRand` / `ReadSeq` | 随机读 / 顺序读（每次请求） |
| `WriteRand` / `WriteSeq` | 随机写 / 顺序写 |
| `Open`/`Getattr`/`Statfs`/`Getxattr`/`Setxattr`/`Create`/`Mkdir`/`Unlink`/`Rename` | node 级 op 固定延迟 |
| `ReadByte` / `WriteByte` | 每字节额外延迟（带宽限制）；0=不限 |

## 预设档

| 档名 | 别名 | 量级 |
|---|---|---|
| `ProfileNone` | `none` | 全 0，不延迟（默认） |
| `ProfileMemory` | `memory`/`tmpfs`/`ram` | μs 级，随机≈顺序 |
| `ProfileSSD` | `ssd` | 随机 ~150μs、顺序 ~50μs，带宽充裕 |
| `ProfileHDD` | `hdd`/`disk` | 随机 ~8ms（寻道主导）、顺序 ~200μs + ~10MB/s 带宽 |

`ProfileByName(name)` 大小写不敏感地解析档名。HDD/SSD 是默认组合，覆盖两类典型设备。

## 手动性能旋钮（rand / seq）

除预设档外，可用两个直观旋钮手动调参，对应真实设备的核心指标：

| 旋钮 | 含义 | 单位 | 映射到 profile |
|---|---|---|---|
| **随机寻址延迟** | 一次随机读/写（及每个元数据 op）的寻址代价 | `ns`/`us`/`ms` | `ReadRand`/`WriteRand` + 各元数据 op |
| **顺序读写速度** | 顺序传输带宽 | `M`(=MiB/s)/`G`(=GiB/s) | `ReadByte`/`WriteByte`（= 1s/速度） |

`ProfileFromKnobs(rand, seqBw)` 由这两个旋钮构建完整 profile：随机寻址延迟写入随机读/写
与各元数据 op；顺序访问的 per-request 部分为 0（由带宽主导）。`ParseLatency`/`ParseSpeed`
解析带单位字符串（`8ms`/`200us`/`100ns`、`100M`/`2G`/`100MiB/s`）。

**取值校验**：`--rand` 不可为负（负延迟会让 sleepFor 静默当作不延迟，即"要慢却变快"）；
`--seq` 拒绝 `NaN`/`Inf`，且正带宽须 ≥ 1 B/s（更慢会让 per-byte 延迟 `1s/速度` 溢出 int64
纳秒、回绕成负而静默不限速，或让单次读取 sleep 数十年）；`0`（含 `0M`）= 不限速，合法。

**与预设档互斥**：`set latency` 里 `--profile` 与 `--rand`/`--seq` 不能同给——叠加时旋钮只
覆盖随机/带宽字段、预设的其余字段会静默保留，形成既非预设也非旋钮意图的混合 profile。
自定义组合请用库 API `ProfileFromKnobs`。`--speed` 可与任一组合。

## backing（tmpfs）校准与钳制

faultfs 通过**叠加延迟**模拟更慢的设备，因此可模拟的性能上限 = backing 本身的性能
（最强即基于内存的 tmpfs）。`Calibrate(backing)` 实测 backing 的随机寻址延迟（单次 4KiB
随机读均摊）与顺序读带宽，作为上限。`AdjustProfile(p, rand, bw)` 据此把目标参数钳制：

- `effectiveRand = max(0, targetRand - measuredRand)`
- `effectiveByte = max(0, targetByte - measuredByte)`（带宽同理）

当目标比 backing 还快（如 `--rand 1ns` 而实测 backing 1µs）时，对应字段钳到 0 并告警——
即"用更强的 tmpfs 模拟更弱的系统；预设值超出 tmpfs 性能时提示并改用 tmpfs 模拟"。HDD/SSD
等预设远慢于 tmpfs，钳制几乎无影响；memory 档或激进手动值才会触发告警。钳制覆盖随机读/写、
顺序读/写带宽，以及由 rand 派生的各元数据 op（后者静默钳制，告警由随机读/写覆盖，避免刷屏）。
校准结果缓存在 `Injector` 上（首次 `set latency` 经 `sync.Once` 独占实测，并发首调只跑一次，
之后复用）；校准临时文件放在 backing 的**父目录**（同设备、但不经 FUSE 暴露，校准期间不会被
`ls $MP` 看到），父目录在不同设备时回退 backing。库用户可用 `CalibratedFloor()` 查询、用
`SetProfileCalibrated(backing, target)` 一步完成"校准+钳制+写入"（与 CLI 共用同一实现）。

## speed 全局倍速

`SetSpeed(s)`：`1.0` 正常、`>1` 慢放、`<1` 快放（`<=0` 视为 1）。实际延迟 =
`profile 值 × speed`。用于在不改 profile 的前提下整体快放/慢放整个 faultfs。

CLI `set latency --speed` 对 `<=0` 会追加一条 `warning:`（明示钳为 1.0，避免用户想"清零/
暂停延迟"却静默得到正常速度）。库 `SetSpeed` 仍按既定语义静默钳制。

## 默认（不启用性能模拟）

未指定任何 profile/knob 时为 `ProfileNone`（全 0，不延迟）：faultfs **不叠加任何延迟**，
直接反映 backing（通常 tmpfs）的真实性能。`mount` 不带 `--rand`/`--seq` 即此态；`set latency`
可在线启用模拟。所有 setter 同步更新初始快照，故 `refresh` 复位到最近一次 set 的值。

## 顺序 vs 随机判定

`FaultFile` 持 `lastOff atomic.Int64`。每次 read/write：若 `off == lastOff` 判为
**顺序**访问（用 `*Seq`），否则**随机**（用 `*Rand`）；随后更新 `lastOff = off + n`。
首次访问 `off=0 == lastOff(0)` 视为顺序。

## 带宽（per-byte）

read/write 的总延迟 = `*Rand|*Seq` + `n × *Byte`。per-byte 以整数纳秒存储，故只有 per-byte
≥ 数十 ns 的带宽（HDD 档量级）能精确表达；>~1GiB/s 的 per-byte <1ns 量化为 0（=不限速，
实际由 backing 封顶）。per-byte 叠加（`*Byte × n`）与 `1s/速度` 换算都做了溢出保护：极端慢速
输入会钳到最大正 Duration，而非回绕成负被当作不延迟。HDD 档设了 `ReadByte`/`WriteByte`
模拟顺序带宽受限；SSD/Memory 为 0。

## API / CLI

```go
inj.SetProfile(faultfs.ProfileHDD)          // 预设档
inj.SetProfile(faultfs.ProfileFromKnobs(    // 手动旋钮
    8*time.Millisecond, 100*faultfs.MiB))
inj.SetSpeed(2.0)
// 可选：按 backing 钳制（与 CLI set-latency 共用同一实现）
warns := inj.SetProfileCalibrated(backingDir, target) // 一步：校准 + 钳制 + 写入
// 等价的显式三步（库用户想自行控制时）：
// rand, bw, _ := faultfs.Calibrate(backingDir)
// adj, warns := faultfs.AdjustProfile(target, rand, bw)
// inj.SetProfile(adj)
```
```sh
faultfs set latency <mp> --profile hdd --speed 2.0     # 预设档 + 倍速（--profile 与 --rand/--seq 互斥）
faultfs set latency <mp> --rand 8ms --seq 100M          # 手动旋钮（随机寻址 8ms，顺序 100MiB/s）
faultfs set latency <mp> --rand 1ns --seq 100G          # 超出 tmpfs → 逐条 warning 并钳制
```
