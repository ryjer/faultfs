package faultfs

import (
	"context"
	"sync/atomic"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// FaultFile 按指针嵌入 *fs.LoopbackFile，覆写读/写/属性：命中注入规则时返回
// [Rule.Errno]，否则透传并在透传后按延迟模型 sleep。由 [FaultNode.Open] /
// [FaultNode.Create] 创建，携带该文件的挂载内相对路径与共享的 *Injector。
// lastOff 用于判定顺序/随机访问（off == 上一次结束位置 → 顺序）。
type FaultFile struct {
	*fs.LoopbackFile
	inj     *Injector
	path    string
	lastOff atomic.Int64
}

var (
	_ fs.FileReader    = (*FaultFile)(nil)
	_ fs.FileWriter    = (*FaultFile)(nil)
	_ fs.FileGetattrer = (*FaultFile)(nil)
)

// Read 命中 read 规则时返回 (nil, errno)；否则透传，并按“顺序/随机”+ 字节数
// 计算延迟后 sleep。off 是请求起始 offset，参与注入匹配与顺序判定。
func (f *FaultFile) Read(ctx context.Context, buf []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if e := f.inj.Check(OpRead, f.path, off); e != 0 {
		return nil, e
	}
	res, errno := f.LoopbackFile.Read(ctx, buf, off)
	sequential := off == f.lastOff.Load()
	f.lastOff.Store(off + int64(len(buf)))
	f.inj.DelayRead(sequential, len(buf))
	return res, errno
}

// Write 命中 write 规则时返回 (0, errno)；否则透传并延迟。
func (f *FaultFile) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	if e := f.inj.Check(OpWrite, f.path, off); e != 0 {
		return 0, e
	}
	n, errno := f.LoopbackFile.Write(ctx, data, off)
	sequential := off == f.lastOff.Load()
	f.lastOff.Store(off + int64(len(data)))
	f.inj.DelayWrite(sequential, len(data))
	return n, errno
}

// Getattr 命中 getattr 规则时返回注入的 errno；否则透传并延迟。
func (f *FaultFile) Getattr(ctx context.Context, a *fuse.AttrOut) syscall.Errno {
	if e := f.inj.Check(OpGetattr, f.path, -1); e != 0 {
		return e
	}
	errno := f.LoopbackFile.Getattr(ctx, a)
	f.inj.DelayOp(OpGetattr)
	return errno
}
