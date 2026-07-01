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
// lastReadOff/lastWriteOff 分别判定读/写的顺序/随机访问（off == 上一次结束位置 → 顺序）。
type FaultFile struct {
	*fs.LoopbackFile
	inj          *Injector
	path         string
	lastReadOff  atomic.Int64
	lastWriteOff atomic.Int64
}

var (
	_ fs.FileReader  = (*FaultFile)(nil)
	_ fs.FileWriter  = (*FaultFile)(nil)
	_ fs.FileFsyncer = (*FaultFile)(nil)
)

// Read 命中 read 规则时返回 (nil, errno)；否则透传，并按”顺序/随机”+ 字节数
// 计算延迟后 sleep。off 是请求起始 offset，参与注入匹配与顺序判定。
// 使用 res.Size()（实际读取字节数）而非 len(buf) 以保证短读时顺序检测正确。
func (f *FaultFile) Read(ctx context.Context, buf []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	f.inj.DelayRead(off == f.lastReadOff.Load(), len(buf))
	if e := f.inj.Check(OpRead, f.path, off); e != 0 {
		return nil, e
	}
	res, errno := f.LoopbackFile.Read(ctx, buf, off)
	f.lastReadOff.Store(off + int64(res.Size()))
	return res, errno
}

// Write 命中 write 规则时返回 (0, errno)；否则透传并延迟。
// 使用 n（实际写入字节数）而非 len(data) 以保证部分写入时顺序检测正确。
func (f *FaultFile) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	f.inj.DelayWrite(off == f.lastWriteOff.Load(), len(data))
	if e := f.inj.Check(OpWrite, f.path, off); e != 0 {
		return 0, e
	}
	n, errno := f.LoopbackFile.Write(ctx, data, off)
	f.lastWriteOff.Store(off + int64(n))
	return n, errno
}

// Fsync 透传并施加延迟（注：Getattr 由 [FaultNode.Getattr] 在 node 层处理，无需在此覆写）。
func (f *FaultFile) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	f.inj.DelayOp(OpFsync)
	return f.LoopbackFile.Fsync(ctx, flags)
}
