// Package faultfs 提供一个可编程故障注入的 FUSE loopback 文件系统，供需要
// 真实文件系统错误（EIO/ENOSPC/EROFS/ESTALE/EUCLEAN/...）的集成测试使用。
//
// 它把一个“按规则返回任意 errno 的 loopback”挂到某挂载点（backing 一个真实
// 目录），命中规则的操作返回注入的 errno，其余操作透传。把它作为被测系统
// （如 FSS 的 raif）的某块物理盘，被测系统对该盘的 syscall（Open/Read/Write/
// Getattr/Statfs/xattr/Create/Mkdir/Unlink/Rename）经内核路由到 faultfs，
// faultfs 返回的 errno 经内核原样回传——被测系统看到的是真实的文件系统错误
// （os.PathError{Err: syscall.EIO}），与底层真盘报错不可区分，强于在被测系统
// 内部伪造错误的单元测试钩子。
//
// 用法：
//
//	inj := faultfs.NewInjector()
//	mp := faultfs.MountT(t, inj)                       // 挂载；无 /dev/fuse 时自动 t.Skip
//	// MountT 已把卸载注册到 t.Cleanup，无需手动清理。
//	inj.Add(faultfs.Rule{Op: faultfs.OpRead, Path: "blob.bin", Errno: syscall.EIO})
//	disks := raif.Disks{t.TempDir(), mp, t.TempDir()}  // 把 fault 挂载点当作其中一块盘
//	// ... 被测系统读 blob.bin 时，命中规则的那块盘对 raif 返回 EIO ...
//
// 一个 Injector 可同时持有任意多条 Rule，同一时刻多种错误可在不同文件/位置/
// op 上并存。每条 Rule 可精确到 文件（Path 子串）+ 位置（Off/OffLen 区间）+
// 错误（Errno）+ 次数（N，前 N 次后自愈）。详见 [Rule] 字段文档。
//
// 注意（无变更操作是否触发注入）：faultfs 本身、以及 Linux VFS/FUSE 内核，都**不会**
// 短路"无实际变更"的操作——这一点由 [TestNoOpOpsReachFaultfs] 与
// [TestNoOpWritePersistsToBacking] 回归保护。设置与当前值相同的 chmod / utimes /
// truncate，或写入与现状逐字节相同的数据，只要系统调用真的发出，内核就会把它转发给
// faultfs：命中注入规则时照常返回注入的 errno（如 EIO）；未命中则透传到 backing 真实
// 落盘（backing 的 mtime 会推进）。
//
// 但要当心**用户态工具**自身的短路：coreutils/uutils 的 `chmod`、`chown` 在目标值与
// 当前值相同时会**跳过系统调用**（请求根本不发给内核，自然到不了 faultfs），此时注入
// 规则不会触发。这并非内核或 faultfs 的行为，而是这些工具的优化。因此测试 setattr 注入
// 时，要么改一个不同的值，要么用一个总是发起系统调用的方式（`truncate`、`touch`，或 Go
// 的 [os.Chmod]——后者不经"无变化"判断，总是调用 chmod(2)）。write 路径无此问题：`dd`
// 等总是发起 write(2)，写同值也会落盘、也会触发规则。
//
// 早期文档曾担心"FUSE 内核短路同值 setattr"，经实测在 Linux FUSE 上并不成立（内核照
// 发 SETATTR）；真正会短路同值的是上述用户态工具。
package faultfs
