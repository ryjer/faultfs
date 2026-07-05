# faultfs CLI 用法（模拟各类文件系统错误）

面向**非 Go 用户**（脚本、shell、AI agent）的命令行指南：如何用 `faultfs` CLI 挂载一个
可编程故障注入文件系统，并模拟各类真实文件系统错误（EIO/ENOSPC/EROFS/ESTALE/坏扇区…）。

> 本文每条命令都附带**真实运行的命令日志**（路径以 `$MP` 表示挂载点、`$BD` 表示 backing；
> 日志中的 `warning:` / `error:` 等均为真实输出，仅在排版上压缩了重复行）。环境：Linux +
> fuse3，backing 放在 tmpfs（`/dev/shm` 或 `/tmp`）上。

## 0. 准备

> **backing 应放在 tmpfs**（`/dev/shm` 或挂为 tmpfs 的 `/tmp`）。faultfs 的故障/坏扇区/容量模拟都
> 假设 backing 是可信数据源；非 tmpfs 的真实坏道会让 `healed` 状态与真实 EIO 不一致、致上层自愈
> 死循环。详见 [spec/capacity.md](../spec/capacity.md)。

```sh
faultfs mount <backing> <mp> [--detach] [--rand D] [--seq S] [--spare spec] [--capacity C]
#   --detach            后台守护，返回时 control socket 已就绪
#   --rand/--seq        初始性能参数（rand=叠加的随机寻址延迟增量 / seq=顺序读写速度上限）；省略则不模拟、直透 backing
#   --spare <spec>      初始备用块预算（如 8*4KiB）；省略则 0（挂载后用 set spare 设）
#   --capacity <C>      模拟容量上限（如 100M/1G）；须 > backing 已用且 < 总量，写满自动 ENOSPC；省略则不限制
faultfs unmount <mp>                       # fusermount3 -u；守护进程随后自动退出
```

真实日志：

```
$ faultfs mount $BD $MP --detach
faultfs mounted at /tmp/ffmp (pid 31173, socket /run/user/1000/faultfs/bdf241789a1465ed.sock)

$ faultfs status $MP
rules=0  spare=0  speed=1  profile=none
```

子命令分组：`add`（加规则，含 `add badsector`）、`rm`/`clear`/`refresh`/`list`（管理规则）、
`set latency`/`set spare`（**设备固有属性**，非规则）、`status`/`dump`（只读快照）。
设备延迟与备用扇区是设备的属性、不能像规则那样增删，故用 `set` 设置；`refresh` 会把备用扇区
预算还原到 `set spare` 时的初始值。

下面先在 backing 放一个 16 KiB 的 `blob.bin`（4 个对齐 4K 块），作为被注入的对象。

```sh
head -c 16384 /dev/urandom > $BD/blob.bin
```

## 1. 读返回 EIO（坏读）

```sh
faultfs add $MP --op read --path blob.bin --errno EIO   # 返回分配的规则 ID
```

读该文件即得到真实的 `EIO`（与底层真盘报错不可区分）：

```
$ dd if=$MP/blob.bin of=/dev/null bs=4096 count=1
dd: IO error: Input/output error
```

## 2. 写返回 ENOSPC（磁盘满）

```sh
faultfs clear $MP
faultfs add $MP --op write --errno ENOSPC
```

```
$ dd if=/dev/zero of=$MP/blob.bin bs=4096 count=1 conv=notrunc
dd: IO error: No space left on device
```

### 用 `--capacity` 模拟磁盘满（无需加规则）

也可在挂载时设模拟容量上限：写到该容量自动返 `ENOSPC`，且 `df` 看到模拟容量。比规则更贴近
真实"写满"——按累计写入量触发，而非按 op/path。挂载时校验 `capacity ∈ (backing已用, backing总量)`，
保证模拟的"满"先于 backing 真满（否则拒绝挂载）。详见 [spec/capacity.md](../spec/capacity.md)。

```sh
faultfs mount $BD $MP --capacity 10M --detach
df $MP            # 总量=10M（而非 backing 真实容量）
head -c 20M /dev/zero > $MP/big   # 写到 10M 触发 ENOSPC（backing 此时尚未真满）
```

## 3. 创建返回 EROFS（只读文件系统）

```sh
faultfs clear $MP
faultfs add $MP --op create --errno EROFS
```

```
$ touch $MP/x
touch: cannot touch '$MP/x': Read-only file system
```

## 4. 打开返回 ESTALE（失效文件句柄）

```sh
faultfs clear $MP
faultfs add $MP --op open --path blob.bin --errno ESTALE
```

```
$ dd if=$MP/blob.bin of=/dev/null bs=4096 count=1
dd: failed to open '$MP/blob.bin': Stale file handle
```

> `--op` 可取：`open opendir read readdir write create lookup mkdir rmdir unlink rename getattr statfs
> setattr getxattr setxattr removexattr listxattr fsync flush`（空=任意 op）。
> `--errno` 取名称（`EIO/ENOSPC/EROFS/ESTALE/EUCLEAN/ENODEV/EACCES/EPERM/ENOSYS/EFBIG/EDQUOT`，
> 大小写不敏感）或数字。

### 目录读取返回 EIO（`opendir` / `readdir`）

列目录故障有两个粒度，分别命中 readdir 链路的不同环节。`opendir`——打开目录即失败（不打开
backing 目录）：

```sh
faultfs clear $MP
faultfs add $MP --op opendir --path adir --errno EIO
ls $MP/adir
```

```
$ ls $MP/adir
ls: cannot open directory '$MP/adir': Input/output error
```

`readdir` 更细：`opendir` 成功、但读取条目（getdents）失败，模拟"打开正常、读到一半失败"：

```sh
faultfs clear $MP
faultfs add $MP --op readdir --path adir --errno EIO
ls $MP/adir
```

```
$ ls $MP/adir
ls: reading directory '$MP/adir': Input/output error
```

> `opendir` 每次新调，注入稳定。`readdir` 推荐永久规则（默认 `--n 0`）：go-fuse bridge 仅在
> 每轮 READDIR 的首个 getdents 返 errno 时透传给内核，轮次中途的 errno 会被吞掉——永久规则下
> 首个条目即 EIO，错误稳定传播。`--path` 仍是子串匹配（如 `hash` 命中 `hash/md5/ab/cd`），可让
> "某盘某子树 opendir 失败、合并挂载跳过该盘"这类测试精确命中。

## 5. 前N次注入后自愈（`--n`）

```sh
faultfs clear $MP
faultfs add $MP --op read --path blob.bin --errno EIO --n 2   # 只注入前 2 次
```

第 3 次起恢复正常：

```
# read #1: EIO
# read #2: EIO
# read #3: OK
$ faultfs list $MP
id=5 op=read path="blob.bin" off=0 off-len=0 errno=5 n=2 heal=false healed=false rem=0
```

`rem` 是剩余命中次数（`0`=已耗尽，`-1`=永久）。

## 6. 精确到 offset 的坏扇区（`add badsector`，可治愈）

真实硬盘语义：读坏扇区→EIO；写该区→备用块重映射→治愈，后续读正常。`add badsector`
封装为一条 `--heal-on-write` 的 read EIO 规则。

备用块预算用「**块数量 + 块大小**」表达，如 `8*4KiB` = 8 个 4KiB 块；治愈一段坏区时按
`ceil(坏区长度 / 块大小)` **整块消耗**（如 `--len 8192` + 4KiB 块 → 消耗 2 块）。**默认 spare=0**
（无备用），故治愈前需显式分配预算——`set spare`、`mount --spare` 或 `add badsector --spare`。

```sh
faultfs clear $MP
faultfs add badsector $MP --path blob.bin --off 4096 --len 4096   # 标记 [4096,8192) 为坏区
faultfs set spare $MP 4*4KiB                                      # 备用块预算（4 个 4KiB 块；-1 无限）
```

```
# read [4096..8192) before heal: EIO
# write [4096..8192) -> heal:    OK (healed, consumes 1 block)
# read [4096..8192) after heal:  OK
$ faultfs status $MP | head -1
rules=1  spare=3*4KiB  speed=1  profile=none      # spare 由 4*4KiB -> 3*4KiB（治愈消耗 1 块）
```

### 备用预算耗尽 → 写也 EIO；`refresh` 还原预算

```sh
faultfs clear $MP
faultfs add badsector $MP --path blob.bin --off 0    --len 4096   # 坏区 A
faultfs add badsector $MP --path blob.bin --off 8192 --len 4096   # 坏区 B
faultfs set spare $MP 1*4KiB                                      # 只有 1 块备用
```

```
# heal sector A (consumes sole spare): OK
# heal sector B (spare exhausted):     EIO       # 备用耗尽，写也失败
$ faultfs refresh $MP                            # 重置规则/spare 到初始态（spare 还原到 1*4KiB）
reset rule 2: healed=false rem=-1 -> healed=false rem=-1   # 仅示例格式；实际仅列变动的条目
# heal sector B again (spare restored): OK
```

> `refresh` 把每条**实际发生变动**的复位打到 stderr（规则按 ID、spare、latency 的
> `before -> after`），无静默编号。`--keep-latency` 可跳过性能参数的复位。

## 7. 设备性能模拟（`set latency`）

两个手动旋钮（设备固有属性）：随机寻址延迟增量（`--rand`，单位 ns/us/ms，**叠加在 backing 上**，
不可为负、永不告警）与顺序读写速度上限（`--seq`，单位 `M`=MiB/s、`G`=GiB/s，最小 1 B/s，
`0`=不限速；目标 > backing 时取 backing 并告警）；另有预设档
`--profile none|memory|ssd|hdd` 与全局倍速 `--speed`（`>1` 慢放、`<1` 快放；`<=0` 会告警并按 1.0 处理）。

> `--profile` 与 `--rand`/`--seq` **互斥**（叠加会产生难解释的混合 profile）；`--speed` 可与
> 任一组合。自定义组合请用 Go 库 API `ProfileFromKnobs`。

```sh
faultfs set latency $MP --rand 8ms --seq 100M     # 类 HDD：随机寻址 8ms，顺序 100MiB/s
```

```
$ faultfs dump $MP | grep -E "^profile=|read_rand|read_byte|open="
profile=custom speed=1 spare=0 rules=0
  open=8ms
  read_byte=9ns
  read_rand=8ms                 # rand 是叠加增量，透传不减 backing（host 总随机延迟 = backing + 8ms）
```

> faultfs 通过**叠加延迟**模拟更慢设备，最快只能到 backing（这里 tmpfs）本身。`--rand` 是叠加
> 增量、永不告警；`--seq` 是速度上限，当目标 > backing（想限到的速度比 backing 还快）时**告警并
> 取 backing**（rand 仍透传）：

```
$ faultfs set latency $MP --rand 1ns --seq 500M
warning: 顺序读目标带宽超出 backing，已钳制到 backing（tmpfs）性能
warning: 顺序写目标带宽超出 backing，已钳制到 backing（tmpfs）性能
# 注：--rand 1ns 是合法增量，不告警（host 总随机延迟 = backing + 1ns）
```

倍速与复位（`--profile none` 只重置 profile，倍速需单独 `--speed 1`）：

```
$ faultfs set latency $MP --rand 2ms --seq 50M --speed 3
$ faultfs status $MP | head -1
rules=0  spare=0  speed=3  profile=custom
$ faultfs set latency $MP --profile none --speed 1
$ faultfs status $MP | head -1
rules=0  spare=0  speed=1  profile=none
```

## 8. 诊断：list / status / dump

```sh
faultfs add $MP --op read --path blob.bin --errno EIO --n 3
faultfs add badsector $MP --path blob.bin --off 12288 --len 4096
```

```
$ faultfs list $MP
id=9  op=read path="blob.bin" off=0     off-len=0    errno=5 n=3 heal=false healed=false rem=3
id=10 op=read path="blob.bin" off=12288 off-len=4096 errno=5 n=0 heal=true  healed=false rem=-1

$ faultfs status $MP
rules=2  spare=0  speed=1  profile=none
  [9]  op=read path="blob.bin" healed=false rem=3  errno=5(EIO)
  [10] op=read path="blob.bin" healed=false rem=-1 errno=5(EIO)

$ faultfs dump $MP --json            # 全量快照（含挂载元信息+完整 profile），适合日志沉淀
{
  "rules": [
    { "id": 9, "op": "read", "path": "blob.bin", "errno": 5, "n": 3, "remaining": 3 },
    { "id": 10, ...
```

## 9. 删除规则：rm / clear

```
$ faultfs rm $MP 1            # 命中：静默成功
$ echo $?
0
$ faultfs rm $MP 999          # 未命中：报错
Error: rule id 999 does not exist
$ faultfs clear $MP           # 清空所有规则
$ faultfs list $MP
(no rules)
```

## 10. 卸载

```
$ faultfs unmount $MP
$ echo $?
0
```

## 11. 编辑 xattr 时注入错误（文件与目录）

`add --op` 对 xattr 四个 op 都生效（`getxattr`/`setxattr`/`removexattr`/`listxattr`），
且对**文件与目录**都命中（xattr 在 node 级覆写）。`--errno` 覆盖 xattr 常见错误：

| errno | 场景 |
|---|---|
| `ENODATA` | 属性不存在（getxattr/removexattr） |
| `ENOTSUP` / `EOPNOTSUPP` | 文件系统不支持 xattr |
| `ERANGE` | 缓冲过小（getxattr/listxattr） |
| `E2BIG` | 属性名/值过大（setxattr） |

```sh
faultfs add $MP --op setxattr --path blob.bin --errno E2BIG      # 文件：写 xattr 报 E2BIG
faultfs add $MP --op getxattr  --path adir      --errno ENODATA  # 目录：读 xattr 报 ENODATA
```

```
$ setfattr -n user.k -v v $MP/blob.bin      # → 命中注入：E2BIG（Argument list too long / 仿造）
$ getfattr -n user.k $MP/adir               # → 命中注入：ENODATA（No data available）
```

---

### 速查表

| 目标 | 命令 |
|---|---|
| 读 EIO | `faultfs add $MP --op read --path f --errno EIO` |
| 写 ENOSPC | `faultfs add $MP --op write --errno ENOSPC` |
| 创建 EROFS | `faultfs add $MP --op create --errno EROFS` |
| 前 N 次后自愈 | `faultfs add $MP --op read --errno EIO --n 3` |
| 可治愈坏扇区 | `faultfs add badsector $MP --path f --off 4096 --len 4096` |
| xattr 错误 | `faultfs add $MP --op setxattr --path f --errno E2BIG`（文件/目录均可） |
| 目录读取 EIO | `faultfs add $MP --op opendir --path d --errno EIO`（`--op readdir`：opendir 正常、列条目 EIO） |
| 备用块预算 | `faultfs set spare $MP 8*4KiB`（或 `mount --spare 8*4KiB`；`refresh` 还原） |
| 性能模拟 | `faultfs set latency $MP --rand 8ms --seq 100M`（rand=叠加增量、seq=速度上限） |
| 模拟磁盘满 | `faultfs mount $BD $MP --capacity 10M`（写满自动 ENOSPC、df 见 10M） |
| 倍速 | `faultfs set latency $MP --speed 2.0`（`<=0` 告警并按 1.0） |
| 预设档 | `faultfs set latency $MP --profile hdd` |
| 诊断 | `faultfs status $MP` / `faultfs dump $MP [--json]` |
| 重置规则 | `faultfs refresh $MP [--keep-latency]` |
| 删除 | `faultfs rm $MP <id>` / `faultfs clear $MP` |
