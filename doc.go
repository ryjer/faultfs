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
// 注意：FUSE 在内核层会短路某些“无实际变更”的操作。例如 chmod 设置与当前权限
// 相同的值时，内核不会向 faultfs 发送 SETATTR 请求，注入规则因此不会触发。
// 测试时请确保操作确实改变了文件属性（如 chmod 755 而非 chmod 644），否则
// 操作会静默成功而非触发注入错误。
package faultfs
