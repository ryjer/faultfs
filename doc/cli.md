# faultfs CLI 用法（模拟各类文件系统错误）

面向**非 Go 用户**（脚本、shell、AI agent）的命令行指南：如何用 `faultfs` CLI 挂载一个
可编程故障注入文件系统，并模拟各类真实文件系统错误（EIO/ENOSPC/EROFS/ESTALE/坏扇区…）。

> 本文每条命令都附带**真实运行的命令日志**（路径以 `$MP` 表示挂载点、`$BD` 表示 backing；
> 日志中的 `warning:` / `error:` 等均为真实输出，仅在排版上压缩了重复行）。环境：Linux +
> fuse3，backing 放在 tmpfs（`/dev/shm` 或 `/tmp`）上。

## 0. 准备

```sh
faultfs mount <backing> <mp> [--detach]   # --detach 后台守护，返回时 control socket 已就绪
faultfs unmount <mp>                       # fusermount3 -u；守护进程随后自动退出
```

真实日志：

```
$ faultfs mount $BD $MP --detach
faultfs mounted at /tmp/ffmp (pid 31173, socket /run/user/1000/faultfs/bdf241789a1465ed.sock)

$ faultfs status $MP
rules=0  spare=-1  speed=1  profile=none
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

> `--op` 可取：`open read write create lookup mkdir rmdir unlink rename getattr statfs
> setattr getxattr setxattr removexattr listxattr fsync flush`（空=任意 op）。
> `--errno` 取名称（`EIO/ENOSPC/EROFS/ESTALE/EUCLEAN/ENODEV/EACCES/EPERM/ENOSYS/EFBIG/EDQUOT`，
> 大小写不敏感）或数字。

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

真实硬盘语义：读坏扇区→EIO；写该区→备用扇区重映射→治愈，后续读正常。`add badsector`
封装为一条 `--heal-on-write` 的 read EIO 规则。

```sh
faultfs clear $MP
faultfs add badsector $MP --path blob.bin --off 4096 --len 4096   # 标记 [4096,8192) 为坏区
faultfs set spare $MP 4                                            # 备用扇区预算（-1 无限）
```

```
# read [4096..8192) before heal: EIO
# write [4096..8192) -> heal:    OK (healed)
# read [4096..8192) after heal:  OK
$ faultfs status $MP | head -1
rules=1  spare=3  speed=1  profile=none      # spare 由 4 -> 3（治愈消耗一格）
```

### 备用预算耗尽 → 写也 EIO；`refresh` 还原预算

```sh
faultfs clear $MP
faultfs add badsector $MP --path blob.bin --off 0    --len 4096   # 坏区 A
faultfs add badsector $MP --path blob.bin --off 8192 --len 4096   # 坏区 B
faultfs set spare $MP 1                                            # 只有 1 格备用
```

```
# heal sector A (consumes sole spare): OK
# heal sector B (spare exhausted):     EIO       # 备用耗尽，写也失败
$ faultfs refresh $MP                            # 重置所有规则到初始态（spare 还原到 1）
# heal sector B again (spare restored): OK
```

## 7. 设备性能模拟（`set latency`）

两个手动旋钮（设备固有属性）：随机寻址延迟（`--rand`，单位 ns/us/ms）与顺序读写速度
（`--seq`，单位 `M`=MiB/s、`G`=GiB/s）；另有预设档 `--profile none|memory|ssd|hdd` 与全局
倍速 `--speed`（`>1` 慢放、`<1` 快放）。

```sh
faultfs set latency $MP --rand 8ms --seq 100M     # 类 HDD：随机寻址 8ms，顺序 100MiB/s
```

```
$ faultfs dump $MP | grep -E "^profile=|read_rand|read_byte|open="
profile=custom speed=1 spare=0 rules=0
  open=8ms
  read_byte=9ns
  read_rand=7.999075ms          # 目标 8ms 减去 backing(tmpfs) 实测延迟后落盘
```

> faultfs 通过**叠加延迟**模拟更慢设备，最快只能到 backing（这里 tmpfs）本身。当目标比
> backing 还快时，会**告警并钳制**到 backing 性能：

```
$ faultfs set latency $MP --rand 1ns --seq 200G
warning: 随机读目标(1ns)快于 backing(925ns)，已钳制到 backing（tmpfs）性能; 随机写目标(1ns)快于 backing(925ns)，已钳制到 backing（tmpfs）性能
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
Error: 规则 id 999 不存在
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

---

### 速查表

| 目标 | 命令 |
|---|---|
| 读 EIO | `faultfs add $MP --op read --path f --errno EIO` |
| 写 ENOSPC | `faultfs add $MP --op write --errno ENOSPC` |
| 创建 EROFS | `faultfs add $MP --op create --errno EROFS` |
| 前 N 次后自愈 | `faultfs add $MP --op read --errno EIO --n 3` |
| 可治愈坏扇区 | `faultfs add badsector $MP --path f --off 4096 --len 4096` |
| 备用预算 | `faultfs set spare $MP 4`（`refresh` 还原） |
| 性能模拟 | `faultfs set latency $MP --rand 8ms --seq 100M` |
| 倍速 | `faultfs set latency $MP --speed 2.0` |
| 预设档 | `faultfs set latency $MP --profile hdd` |
| 诊断 | `faultfs status $MP` / `faultfs dump $MP [--json]` |
| 重置规则 | `faultfs refresh $MP` |
| 删除 | `faultfs rm $MP <id>` / `faultfs clear $MP` |
