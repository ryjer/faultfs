# 模拟容量（capacity）与 backing 选择

faultfs 可在挂载时设一个**模拟容量上限** `capacity`，用于：
1. 让 `df`/上层 `statfs` 看到一个比 backing 更小的虚拟容量；
2. 在 write 累计达到该容量时强制返回 `ENOSPC`（无需额外加注入规则）——即用 capacity 直接模拟"磁盘满"；
3. 保证这个模拟的"满"**先于 backing 真满**触发，从而被测系统看到的 `ENOSPC` 一定来自 faultfs 的模拟、而非 backing 真的写满。

## 模型

| 量 | 含义 | 来源 |
|---|---|---|
| `capacity` | 模拟总容量（字节） | 用户设（`SetCapacity` / `mount --capacity`）；`0`=未启用 |
| backing 已用 | backing 真实已用字节 | `statfs(backing)` 的 `(Blocks-Bfree)*Frsize`（带 ~10ms TTL 缓存） |
| faultfs 可用 | `capacity - backing已用` | 推算 |

关键不变量：**模拟的总量 = capacity，已用 = backing 真实已用**。capacity 不自己维护写入计数——
直接读 backing 的 `statfs` 取真实已用，因此覆盖写、truncate、稀疏文件、甚至外部进程对 backing
的写入都能被自动、正确地反映（无需 faultfs 维护文件大小表）。字节换算用 `f_frsize`（statfs 基本
块），而非 `f_bsize`（优选传输块）——二者在 tmpfs/ext4 等通常相等，但不同的 fs 上只有 frsize 正确
（block 计数字段以 frsize 为单位）。`statfs` 结果在 Injector 上有 ~10ms TTL 缓存，避免每个 write
都打一次 statfs（高 IOPS 下 statfs 取 superblock 锁会成为热点）。

挂载校验（`checkCapacityAtMount`）保证 capacity 落在合法区间，使 faultfs 先满：

```
backing已用 < capacity < backing总量
```

- `capacity > backing已用`：否则挂载即满，无可用空间可模拟。
- `capacity < backing总量`：等价于 `capacity - 已用 < backing剩余`，即 faultfs 的可用 **小于** backing
  的真实可用——所以写到 capacity 时 faultfs 先返 ENOSPC，backing 此时仍有 `(总量-capacity)` 余量，不会真满。

任一条件不满足则**拒绝挂载**（CLI 非零退出），避免给出一个无法保证"先满"的配置。

## 运行时行为

- **write / fallocate**：`FaultFile.Write` 与 `FaultFile.Allocate`（fallocate）在规则 `Check` **之前**
  先调 `checkWriteCapacity`：若 `n > capacity - backing已用` 则返 `ENOSPC`（不写、也不进规则副作用）。
  容量判定先于规则是为了避免 heal-then-ENOSPC：若先 `Check`（HealOnWrite 治愈会扣 spare、标记
  healed）再判容量返 ENOSPC，write 失败却已落下治愈副作用、无法回滚，后续 read 会放行读到 backing
  旧数据。保守近似——覆盖写也按请求字节数 `n` 计，仅在接近满时触发。`backing已用` 取 statfs 的
  TTL 缓存；statfs 失败沿用上次缓存值（fail-open，不因 statfs 故障误杀写入；与挂载时 statfs 失败
  拒绝挂载的 fail-closed 互补）。注：`ftruncate`/`O_TRUNC` 建稀疏文件不分配块、不增长已用，故不
  在此判定之列（与真实稀疏文件语义一致）。
- **statfs**：`FaultNode.Statfs` 先透传 backing 真实值，再若 `capacity>0` 用 `reflectCapacity` 改写
  `out.Blocks/Bfree/Bavail`：`total = capacity/frsize`、`avail = total - backing真实已用块`。`df` 与
  上层 `statfs` 据此看到模拟容量。
- **优先级**：容量是设备级硬上限，先于规则判定——磁盘满时任何 write/fallocate 直接 ENOSPC、不进
  规则副作用。容量未满时才走规则 `Check`（规则如 `add --op write --errno ENOSPC` 可再注入）。两套
  ENOSPC 来源分工明确：容量=设备物理上限，规则=针对 op/path 的注入。

## API / CLI

```go
inj.SetCapacity(100 * faultfs.MiB)   // 100MiB 模拟容量；<0 钳到 0（未启用）。mount 时校验
fmt.Println(inj.Capacity())          // 读回
// capacity 是 mount 固化的设备属性：不被消耗、不进 Refresh。改值需重新挂载。
// 单位解析：faultfs.ParseCapacity("100M") → 100*MiB；展示用 faultfs.FormatSize。
```
```sh
faultfs mount <backing> <mp> --capacity 100M        # 模拟 100MiB 容量
faultfs mount <backing> <mp> --capacity 1G --detach
# status / dump 输出 capacity=（unlimited 表示未启用）
```

`capacity` 是**挂载时固化**的设备属性（同 backing 本身），故**不走 `set` 子命令、不进 `refresh`**——
它描述的是"这块盘多大"，运行中不改变。在线调整需卸载重挂。

## backing 应使用 tmpfs（强烈建议）

faultfs 的全部模拟（故障注入、坏扇区重映射、容量记账、性能校准）都建立在同一个前提上：
**backing 是可靠的真实数据源（ground truth）**——faultfs 注入的 `EIO` 是"假坏"，backing 里的数据
其实完好。把 backing 放在 **tmpfs**（如 `/dev/shm`、或挂为 tmpfs 的 `/tmp`）即可保证：

- tmpfs 是内存文件系统，**没有物理坏道**，不会因介质损坏产生真实 `EIO`；
- tmpfs 不会"真的写满"（除受 RAM 限制外，且容量校验保证模拟满先于该限制）；
- tmpfs 性能稳定，让 `--rand`/`--seq` 的延迟校准与钳制可重复。

### 不采用 tmpfs 的后果

若 backing 在真实磁盘（机械盘/SSD/有缺陷介质）上，会破坏上述前提，产生难以排查的 bug：

1. **坏扇区 `healed` 状态与真实损坏不一致 → 自愈死循环**。`HealOnWrite` 规则一旦被 write 触发治愈
   就永久标记 `healed=true`（见 [injector.md](injector.md)）。若 backing 那段区域**真的**坏了：
   - `read`：`Check` 见 `healed` 放行 → `LoopbackFile.Read` 返回**真实 EIO** 原样透传给上层；
   - 上层想再次自愈，于是 `write` 回 → `Check` 见 `healed` 又放行，**不再翻状态、不再扣 spare、
     不再触发任何"再修复"**；
   - 结果：上层卡在"read 永远 EIO、write 永远看似成功却不触发治愈"的死循环。真硬盘重新坏会再次
     reallocate 并扣备用块，faultfs 的状态机无法区分"模拟的已修复"与"backing 的真实损坏"。

2. **容量记账失真**。`capacity` 模型用 backing `statfs` 取真实已用；真实磁盘的坏道/保留块/配额
   会让"已用"与 faultfs 预期不符，模拟的"满"点漂移。

3. **性能校准/钳制失真**。`Calibrate` 实测 backing 性能作为可模拟上限；真实磁盘的随机/顺序性能
   波动大，让 `--rand`/`--seq` 的告警与钳制不可重复。

> 总之：**只要 backing 是 tmpfs，faultfs 的模拟就是自洽的"假故障"**；一旦 backing 可能真坏/真慢，
> 模型的 ground-truth 前提就破了。CLI/库示例默认 backing 放 tmpfs。
