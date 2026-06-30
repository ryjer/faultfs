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

`ProfileByName(name)` 大小写不敏感地解析档名。

## speed 全局倍速

`SetSpeed(s)`：`1.0` 正常、`>1` 慢放、`<1` 快放（`<=0` 视为 1）。实际延迟 =
`profile 值 × speed`。用于在不改 profile 的前提下整体快放/慢放整个 faultfs。

## 顺序 vs 随机判定

`FaultFile` 持 `lastOff atomic.Int64`。每次 read/write：若 `off == lastOff` 判为
**顺序**访问（用 `*Seq`），否则**随机**（用 `*Rand`）；随后更新 `lastOff = off + n`。
首次访问 `off=0 == lastOff(0)` 视为顺序。

## 带宽（per-byte）

read/write 的总延迟 = `*Rand|*Seq` + `n × *Byte`。HDD 档设了 `ReadByte`/`WriteByte`
以模拟顺序带宽受限；SSD/Memory 档为 0（带宽充裕）。

## API / CLI

```go
inj.SetProfile(faultfs.ProfileHDD)
inj.SetSpeed(2.0)
```
```sh
faultfs latency <mp> --profile hdd --speed 2.0
```
