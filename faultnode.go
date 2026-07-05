package faultfs

import (
	"context"
	"path/filepath"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// FaultNode 按指针嵌入 *fs.LoopbackNode（与 FSS 的 fssNode 同一范式），继承
// 全部 loopback 操作，仅覆写被测数据通路会用到的那些：命中注入规则时返回
// [Rule.Errno]，否则调嵌入类型透传，并在透传后按 [Injector] 的延迟模型 sleep
// 模拟设备响应时间。
type FaultNode struct {
	*fs.LoopbackNode
	inj     *Injector
	backing string // backing 目录绝对路径，传给 FaultFile 供容量判定、Statfs 反映容量
}

// 静态断言：确保覆写的方法签名与 go-fuse fs 接口严格一致——签名写错不会
// 编译报错，而是让 go-fuse 静默回落到嵌入类型的实现（注入/延迟失效）。
var (
	_ fs.NodeOpener         = (*FaultNode)(nil)
	_ fs.NodeOpendirHandler = (*FaultNode)(nil)
	_ fs.NodeCreater        = (*FaultNode)(nil)
	_ fs.NodeGetattrer      = (*FaultNode)(nil)
	_ fs.NodeStatfser       = (*FaultNode)(nil)
	_ fs.NodeSetattrer      = (*FaultNode)(nil)
	_ fs.NodeGetxattrer     = (*FaultNode)(nil)
	_ fs.NodeSetxattrer     = (*FaultNode)(nil)
	_ fs.NodeRemovexattrer  = (*FaultNode)(nil)
	_ fs.NodeListxattrer    = (*FaultNode)(nil)
	_ fs.NodeLookuper       = (*FaultNode)(nil)
	_ fs.NodeMkdirer        = (*FaultNode)(nil)
	_ fs.NodeRmdirer        = (*FaultNode)(nil)
	_ fs.NodeUnlinker       = (*FaultNode)(nil)
	_ fs.NodeRenamer        = (*FaultNode)(nil)
	_ fs.NodeFsyncer        = (*FaultNode)(nil)
	_ fs.NodeFlusher        = (*FaultNode)(nil)
)

// WrapChild 由 go-fuse 为每个经 Lookup/Create/Mkdir 等创建的子节点调用，把
// 原始 *fs.LoopbackNode 重新包成 *FaultNode 并注入同一个 *Injector。
func (n *FaultNode) WrapChild(ctx context.Context, ops fs.InodeEmbedder) fs.InodeEmbedder {
	switch v := ops.(type) {
	case *FaultNode:
		return v
	case *fs.LoopbackNode:
		return &FaultNode{LoopbackNode: v, inj: n.inj, backing: n.backing}
	}
	return ops
}

// rel 返回挂载内相对路径（如 "dir/a.txt"，根返回 "."）。
func (n *FaultNode) rel() string {
	return n.Path(n.Root())
}

// ---- 文件句柄 / 属性 ----

// Open 命中 open 规则时返回注入的 errno；否则透传 loopback，并把返回的
// *fs.LoopbackFile 包成 *FaultFile，使其读/写也走注入/延迟。fuseFlags 强制带
// FOPEN_DIRECT_IO：禁用内核 page cache，确保每次 read/write 都进入 faultfs。
func (n *FaultNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	n.inj.DelayOp(OpOpen)
	if e := n.inj.Check(OpOpen, n.rel(), -1, 0); e != 0 {
		return nil, 0, e
	}
	fh, fuseFlags, errno := n.LoopbackNode.Open(ctx, flags)
	fuseFlags |= fuse.FOPEN_DIRECT_IO
	if errno != 0 {
		return nil, 0, errno
	}
	if lf, ok := fh.(*fs.LoopbackFile); ok {
		return &FaultFile{LoopbackFile: lf, inj: n.inj, path: n.rel(), backing: n.backing}, fuseFlags, 0
	}
	return fh, fuseFlags, 0
}

// Create 同理：命中 create 规则返注入 errno；成功时包 FaultFile。
func (n *FaultNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	rel := filepath.Join(n.rel(), name)
	n.inj.DelayOp(OpCreate)
	if e := n.inj.Check(OpCreate, rel, -1, 0); e != 0 {
		return nil, nil, 0, e
	}
	inode, fh, fuseFlags, errno := n.LoopbackNode.Create(ctx, name, flags, mode, out)
	fuseFlags |= fuse.FOPEN_DIRECT_IO
	if errno != 0 {
		return nil, nil, 0, errno
	}
	if lf, ok := fh.(*fs.LoopbackFile); ok {
		return inode, &FaultFile{LoopbackFile: lf, inj: n.inj, path: rel, backing: n.backing}, fuseFlags, 0
	}
	return inode, fh, fuseFlags, 0
}

func (n *FaultNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.inj.DelayOp(OpGetattr)
	if e := n.inj.Check(OpGetattr, n.rel(), -1, 0); e != 0 {
		return e
	}
	return n.LoopbackNode.Getattr(ctx, f, out)
}

func (n *FaultNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	n.inj.DelayOp(OpSetattr)
	if e := n.inj.Check(OpSetattr, n.rel(), -1, 0); e != 0 {
		return e
	}
	return n.LoopbackNode.Setattr(ctx, f, in, out)
}

func (n *FaultNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	n.inj.DelayOp(OpStatfs)
	if e := n.inj.Check(OpStatfs, n.rel(), -1, 0); e != 0 {
		return e
	}
	errno := n.LoopbackNode.Statfs(ctx, out)
	// 模拟容量反映：若设了 capacity，把 total 改为 capacity、avail 改为 capacity-backing真实used，
	// 让 df/上层 statfs 看到模拟容量。used 取 backing 真实（Statfs 已填入 out）。
	if capacity := n.inj.Capacity(); capacity > 0 && errno == 0 {
		reflectCapacity(out, capacity)
	}
	return errno
}

// ---- xattr ----

func (n *FaultNode) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	n.inj.DelayOp(OpGetxattr)
	if e := n.inj.Check(OpGetxattr, n.rel(), -1, 0); e != 0 {
		return 0, e
	}
	return n.LoopbackNode.Getxattr(ctx, attr, dest)
}

func (n *FaultNode) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	n.inj.DelayOp(OpSetxattr)
	if e := n.inj.Check(OpSetxattr, n.rel(), -1, 0); e != 0 {
		return e
	}
	return n.LoopbackNode.Setxattr(ctx, attr, data, flags)
}

func (n *FaultNode) Removexattr(ctx context.Context, attr string) syscall.Errno {
	n.inj.DelayOp(OpRemovexattr)
	if e := n.inj.Check(OpRemovexattr, n.rel(), -1, 0); e != 0 {
		return e
	}
	return n.LoopbackNode.Removexattr(ctx, attr)
}

func (n *FaultNode) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	n.inj.DelayOp(OpListxattr)
	if e := n.inj.Check(OpListxattr, n.rel(), -1, 0); e != 0 {
		return 0, e
	}
	return n.LoopbackNode.Listxattr(ctx, dest)
}

// ---- 目录 / 树操作 ----

// OpendirHandle 命中 opendir 规则时返回注入的 errno（不打开 backing 目录）；否则委托
// [fs.LoopbackNode.OpendirHandle]，把返回的目录 handle 包成 [*FaultDir]，使其 readdir 也
// 走注入。fuseFlags 原样透传 loopback 的 0：不设 FOPEN_CACHE_DIR（否则内核缓存 readdir、破坏
// 每次注入），也不设 FOPEN_DIRECT_IO（那是文件页缓存标志，对目录条目缓存无意义）。
func (n *FaultNode) OpendirHandle(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	n.inj.DelayOp(OpOpendir)
	if e := n.inj.Check(OpOpendir, n.rel(), -1, 0); e != 0 {
		return nil, 0, e
	}
	fh, fuseFlags, errno := n.LoopbackNode.OpendirHandle(ctx, flags)
	if errno != 0 {
		return nil, 0, errno
	}
	return &FaultDir{inner: fh, inj: n.inj, path: n.rel(), backing: n.backing}, fuseFlags, 0
}

func (n *FaultNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.inj.DelayOp(OpLookup)
	if e := n.inj.Check(OpLookup, filepath.Join(n.rel(), name), -1, 0); e != 0 {
		return nil, e
	}
	return n.LoopbackNode.Lookup(ctx, name, out)
}

func (n *FaultNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.inj.DelayOp(OpMkdir)
	if e := n.inj.Check(OpMkdir, filepath.Join(n.rel(), name), -1, 0); e != 0 {
		return nil, e
	}
	return n.LoopbackNode.Mkdir(ctx, name, mode, out)
}

func (n *FaultNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	n.inj.DelayOp(OpRmdir)
	if e := n.inj.Check(OpRmdir, filepath.Join(n.rel(), name), -1, 0); e != 0 {
		return e
	}
	return n.LoopbackNode.Rmdir(ctx, name)
}

func (n *FaultNode) Unlink(ctx context.Context, name string) syscall.Errno {
	n.inj.DelayOp(OpUnlink)
	if e := n.inj.Check(OpUnlink, filepath.Join(n.rel(), name), -1, 0); e != 0 {
		return e
	}
	return n.LoopbackNode.Unlink(ctx, name)
}

func (n *FaultNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	n.inj.DelayOp(OpRename)
	if e := n.inj.Check(OpRename, filepath.Join(n.rel(), name), -1, 0); e != 0 {
		return e
	}
	return n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
}

func (n *FaultNode) Fsync(ctx context.Context, f fs.FileHandle, flags uint32) syscall.Errno {
	n.inj.DelayOp(OpFsync)
	if e := n.inj.Check(OpFsync, n.rel(), -1, 0); e != 0 {
		return e
	}
	if fh, ok := f.(*FaultFile); ok {
		return fh.LoopbackFile.Fsync(ctx, flags)
	}
	return 0
}

func (n *FaultNode) Flush(ctx context.Context, f fs.FileHandle) syscall.Errno {
	n.inj.DelayOp(OpFlush)
	if e := n.inj.Check(OpFlush, n.rel(), -1, 0); e != 0 {
		return e
	}
	if fh, ok := f.(*FaultFile); ok {
		return fh.Flush(ctx)
	}
	return 0
}
