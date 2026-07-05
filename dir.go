package faultfs

import (
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// FaultDir 包装 [fs.LoopbackNode.OpendirHandle] 返回的目录 handle，使其 readdir 也走
// 故障注入。go-fuse loopback 的目录 handle 具体类型是未导出的 *fs.loopbackDirStream
// （不是 *fs.LoopbackFile），故这里以 fs.FileHandle 接口持有，通过类型断言把目录 handle
// 操作委托给底层，仅在 Readdirent 上注入 OpReaddir。
//
// 由 [FaultNode.OpendirHandle] 创建，携带该目录的挂载内相对路径、backing 绝对路径与共享的
// *Injector。范式与 [FaultFile] 对齐（inj/path/backing 三字段），区别仅在持接口而非嵌入。
type FaultDir struct {
	inner   fs.FileHandle // 来自 LoopbackNode.OpendirHandle（动态类型 *fs.loopbackDirStream）
	inj     *Injector
	path    string
	backing string
}

// 静态断言：确保覆写的方法签名与 go-fuse fs 目录 handle 接口严格一致——签名写错不会
// 编译报错，而是让 go-fuse 静默回落（readdir 返空 / 不关闭 fd）。注意 FileReleasedirer.
// Releasedir 是目录 handle 接口里唯一无返回值的方法。
var (
	_ fs.FileReaddirenter = (*FaultDir)(nil)
	_ fs.FileSeekdirer    = (*FaultDir)(nil)
	_ fs.FileReleasedirer = (*FaultDir)(nil)
	_ fs.FileFsyncdirer   = (*FaultDir)(nil)
)

// Readdirent 命中 readdir 规则时返回 (nil, errno)（不调底层 getdents）；否则委托底层 handle。
//
// 语义注意：go-fuse bridge（fs/bridge.go readDirMaybeLookup）在每轮 READDIR 的首个
// Readdirent 返 errno 时直接透传给内核；轮次中途的 errno 会被吞为 overflow（返 OK + 部分
// 条目）。故 OpReaddir 推荐用 N=0（永久）——首个 Readdirent 即 EIO 且属轮次首条，稳定传播。
// N>0 时 N 按"每个 Readdirent 调用"计（loopback 流先吐 "."/".."），非"用户可见条目数"。
//
// 不调 DelayOp：与 PRD FR-2 一致，且当前 LatencyProfile 无 readdir 字段（DelayOp 对未知 op
// 是安全 no-op，见 [Injector.DelayOp]）。
func (f *FaultDir) Readdirent(ctx context.Context) (*fuse.DirEntry, syscall.Errno) {
	if e := f.inj.Check(OpReaddir, f.path, -1, 0); e != 0 {
		return nil, e
	}
	if rd, ok := f.inner.(fs.FileReaddirenter); ok {
		return rd.Readdirent(ctx)
	}
	return nil, syscall.ENOSYS // 理论不可达：loopbackDirStream 必实现 FileReaddirenter
}

// Seekdir 纯透传给底层 handle（不注入）。内核经 READDIR 的 offset 驱动 seek。
func (f *FaultDir) Seekdir(ctx context.Context, off uint64) syscall.Errno {
	if sd, ok := f.inner.(fs.FileSeekdirer); ok {
		return sd.Seekdir(ctx, off)
	}
	return syscall.ENOSYS
}

// Fsyncdir 纯透传（不注入；PRD §7 非目标）。
func (f *FaultDir) Fsyncdir(ctx context.Context, flags uint32) syscall.Errno {
	if fd, ok := f.inner.(fs.FileFsyncdirer); ok {
		return fd.Fsyncdir(ctx, flags)
	}
	return syscall.ENOSYS
}

// Releasedir 纯透传（不注入）。必须委托：底层 loopbackDirStream.Releasedir 关闭 backing 目录
// fd，漏委托会泄漏 fd。注意本方法无返回值（与其它目录 handle 方法不同）。
func (f *FaultDir) Releasedir(ctx context.Context, releaseFlags uint32) {
	if rd, ok := f.inner.(fs.FileReleasedirer); ok {
		rd.Releasedir(ctx, releaseFlags)
	}
}
