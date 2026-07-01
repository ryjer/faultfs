# faultfs 架构

faultfs 是一个 FUSE loopback 文件系统：把一个 backing 目录透传到某挂载点，命中注入规则
的操作返回指定 errno，其余透传；并在透传后按延迟模型 sleep 模拟设备响应时间。挂载守护
进程另起一个 control socket，供 CLI/外部进程在线改规则与设备属性。

下面分**数据通路**（被测系统怎么打到 faultfs）、**控制通路**（CLI 怎么在线改 faultfs）、
**包布局**三部分。

## 数据通路（syscall → faultfs → backing）

```
        ┌────────────────────────────────────┐
        │  被测系统（如 FSS / raif）          │
        │  open/read/write/getattr/statfs/   │
        │  xattr/create/mkdir/unlink/rename  │
        └─────────────────┬──────────────────┘
                          │ syscall（经内核路由）
                          ▼
        ┌────────────────────────────────────┐
        │  Linux VFS + FUSE 内核 (/dev/fuse) │
        │  · FOPEN_DIRECT_IO：禁 page cache，│
        │    每次 read/write 都进 faultfs     │
        │  · attr/entry timeout = 0：每次重校│
        │    验，不缓存属性/目录项            │
        └─────────────────┬──────────────────┘
                          │ FUSE 请求
                          ▼
┌─────────────────────────────────────────────────────────────┐
│  faultfs 守护进程（faultfs mount = 一个 go-fuse server）     │
│                                                              │
│  FaultNode / FaultFile                                       │
│   （指针嵌入 go-fuse LoopbackNode/LoopbackFile，             │
│    仅覆写被测数据通路用到的 op；WrapChild 把每个新建子节点    │
│    重新包成 FaultNode，注入同一个 *Injector）                │
│        │                                                     │
│        │ ① 延迟：DelayOp / DelayRead / DelayWrite            │
│        │    按 LatencyProfile × speed sleep                  │
│        │ ② 注入：Injector.Check(op, path, off)               │
│        │    ├ 命中 → 返回注入 errno（EIO/ENOSPC/…），不落盘  │
│        │    └ 未命中 → 透传 ─┐                               │
│        ▼                      │                              │
│  嵌入的 LoopbackNode ◄────────┘  对 backing 真实             │
│        │                         read/write/chmod/…（落盘）   │
│        ▼                                                      │
│  backing 目录（透传，内容原样可见于挂载点）                   │
│                                                              │
│  *Injector ── 共享、线程安全（sync.Mutex）                   │
│    · rules[] ruleState：op/path/off/errno/N/HealOnWrite      │
│      + 运行时状态 remaining/healed                           │
│    · spare 备用扇区预算（Refresh 还原到 SetSpare 的初始值）   │
│    · LatencyProfile + speed（+ backing 校准缓存）            │
└─────────────────────────────────────────────────────────────┘
                          │
                          ▼
                   真实 backing 设备
              （测试环境通常 tmpfs，作为可模拟的性能上限）
```

**为什么被测系统看到的是"真实错误"**：errno 由内核经 FUSE 原样回传，被测系统拿到的是
`os.PathError{Err: syscall.EIO}`，与底层真盘报错不可区分——这强于在被测系统内部伪造错误的
单元测试钩子。

## 控制通路（CLI → control socket → Injector）

```
┌──────────────────────┐   ┌──────────────────────────────────┐
│ faultfs CLI（cobra） │   │ 任何能发 JSON 的进程 / AI agent   │
│ add / set latency /  │   │ control.Send(socket, Req)        │
│ refresh / status /   │   └──────────────┬───────────────────┘
│ dump / rm / clear    │                  │
└──────────┬───────────┘                  │
           │   control.Req{Cmd,Op,...}    │  （JSON over unix socket，一请求一连接）
           ▼                               ▼
   ┌────────────────────────────────────────────┐
   │ control server（per-mount unix socket）     │
   │ SocketPath(mp) 稳定映射 mp → socket 路径   │
   │ Serve(): 读 Req → handler(Req) → 写 Resp   │
   └──────────────────┬─────────────────────────┘
                      │ handler 回调 = handleControl
                      ▼
   ┌────────────────────────────────────────────┐
   │ handleControl（faultfs 包提供）             │
   │  Cmd → Injector 方法                        │
   │   add-rule      → inj.Add                   │
   │   delete-rule   → inj.Delete                │
   │   clear         → inj.Clear                 │
   │   refresh-rules → inj.Refresh               │
   │   set-latency   → setLatency（校准+钳制）   │
   │   set-spare     → inj.SetSpare              │
   │   list/status/dump → 只读快照                │
   └────────────────────────────────────────────┘
```

`set-latency` 的特殊处理：`rand`（随机寻址延迟）是叠加增量、不钳制、不告警；`seq`（顺序速度）是
上限，首次给 `--seq` 时（经 `Injector.SetProfileCalibrated`）触发 `Calibrate(backing)` 实测 backing
顺序带宽并缓存（`sync.Once` 保证并发首调只跑一次），`AdjustProfile` 把 seq 目标里超出 backing 的
部分钳到 0 并在 `Resp.Warns` 告警。rand-only 配置跳过校准。`--profile` 与 `--rand`/`--seq` 互斥，
`--speed` 可与任一组合。

## 包布局与依赖方向

```
cmd/faultfs   ──►  control   （协议 + unix socket 传输）
            ──►  faultfs   （库：Injector / 挂载 / handleControl）

faultfs     ──►  control   （handleControl 用 control.Req/Resp）
            ──►  go-fuse/v2/fs, /fuse  （Loopback + FUSE server）

control     ──►  （仅标准库；刻意不 import faultfs，避免循环依赖）
```

- **`faultfs`**（库）：`Injector`（规则+性能模型）、`FaultNode`/`FaultFile`（FUSE 回调）、
  `latency.go`（profile/校准/钳制）、`Mount`/`Run`/`MountT`、`handleControl`。被测系统与
  Go 测试直接用它。
- **`control`**（协议+传输）：`Req`/`Resp`/`Cmd`、`Server`、`Send`、`SocketPath`。纯协议层，
  **不 import `faultfs`**——通过 `func(Req) Resp` 回调把请求交回挂载方（`handleControl`），
  从而切断循环依赖。
- **`cmd/faultfs`**（CLI）：cobra 子命令。`mount` 在守护进程里起 FUSE server + control server；
  其余子命令作为客户端走 control socket。

## 关键设计点

- **指针嵌入而非继承**：`FaultNode` 嵌入 `*fs.LoopbackNode`、`FaultFile` 嵌入
  `*fs.LoopbackFile`，继承全部 loopback 行为，只覆写被测数据通路用到的 op。未覆写的 op 自动
  透传。配 `var _ fs.NodeXxx = (*FaultNode)(nil)` 静态断言，防签名写错时静默回落到嵌入实现。
- **配置/状态分离 + Refresh**：`Rule`（配置）与 `ruleState`（运行时 `remaining`/`healed`）
  分开，`Refresh` 只重置运行时状态与 `spare`，配置不变——用于"治愈→刷新→再次故障"反复重放。
- **坏扇区有状态模型（按块治愈）**：`HealOnWrite` 把 read 规则变成"read EIO / write 治愈"的真实硬盘
  语义。`spareBlockSize>1` 且 `OffLen>0` 时按块跟踪治愈——部分覆盖 write 只治愈其实际写入的块，
  未覆盖块 read 仍 EIO（避免 backing 旧数据被误读为已修复）。正是 FSS raif `inlineRepair`
  （读 EIO→重构→写回触发重映射）所依赖的语义。
- **模拟容量（capacity）**：挂载时设 `capacity` 上限，运行时 write 超 capacity 返 `ENOSPC`、
  `statfs` 反映模拟容量；挂载校验保证"模拟的满先于 backing 真满"。详见 [capacity.md](capacity.md)。
- **backing 须为 tmpfs**：故障/坏扇区/容量模拟都假设 backing 是可信数据源（注入的 EIO 是"假坏"）；
  非 tmpfs 的真实坏道会让 `healed` 状态与真实 EIO 不一致、致上层自愈死循环。见 [capacity.md](capacity.md)。
- **不短路无变更操作**：faultfs 与内核都不短路"无实际变更"的 op；唯一会"丢"请求的是用户态
  工具（coreutils `chmod`/`chown` 同值时跳过系统调用）。详见 [injector.md](injector.md) 与
  包文档。
