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
// [FaultNode.Create] 创建，携带该文件的挂载内相对路径、backing 绝对路径与共享的 *Injector。
// lastReadOff/lastWriteOff 分别判定读/写的顺序/随机访问（off == 上一次结束位置 → 顺序）。
type FaultFile struct {
	*fs.LoopbackFile
	inj          *Injector
	path         string
	backing      string // backing 目录绝对路径，供容量判定 statfs
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
	if e := f.inj.Check(OpRead, f.path, off, int64(len(buf))); e != 0 {
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
	if e := f.inj.Check(OpWrite, f.path, off, int64(len(data))); e != 0 {
		return 0, e
	}
	// 容量判定（设备级，独立于规则注入）：规则未命中才查。模拟磁盘满时强制 ENOSPC，
	// 保证 faultfs 模拟的"满"先于 backing 真满触发。详见 spec/capacity.md。
	if e := checkWriteCapacity(f.inj.Capacity(), f.backing, len(data)); e != 0 {
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

// checkWriteCapacity 按模拟容量上限判定本次 write 是否放行：cap<=0（未启用）或 n<=0 →放行；
// 实时 statfs(backing) 取真实已用，f_avail = cap - used，n > f_avail → ENOSPC（保守近似：
// 覆盖写也按请求字节数计，仅在接近满时触发）。statfs 失败→放行（不因 statfs 故障误杀写入）。
// 与 [Injector.Check] 的规则注入独立——规则（如 add --op write --errno ENOSPC）优先命中，
// 未命中才查容量，两套 ENOSPC 来源不冲突。
func checkWriteCapacity(cap int64, backing string, n int) syscall.Errno {
	if cap <= 0 || n <= 0 {
		return 0
	}
	var sf syscall.Statfs_t
	if err := syscall.Statfs(backing, &sf); err != nil {
		return 0
	}
	used := int64(sf.Blocks-sf.Bfree) * int64(sf.Bsize)
	if used < 0 {
		used = 0
	}
	if int64(n) > cap-used {
		return syscall.ENOSPC
	}
	return 0
}
