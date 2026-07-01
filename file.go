package faultfs

import (
	"context"
	"math"
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
	_ fs.FileReader    = (*FaultFile)(nil)
	_ fs.FileWriter    = (*FaultFile)(nil)
	_ fs.FileFsyncer   = (*FaultFile)(nil)
	_ fs.FileAllocater = (*FaultFile)(nil)
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
	// 容量判定在规则 Check 之前：磁盘满是设备级硬限制，应先于规则副作用触发。若反过来
	// （先 Check 后容量），HealOnWrite 治愈（扣 spare、标记 healed）会在随后容量返 ENOSPC
	// 时已落盘却无法回滚——write 失败但坏块显示已治愈、后续 read 放行读到 backing 旧数据
	// （heal-then-ENOSPC 原子性破坏）。详见 spec/capacity.md。
	if e := f.inj.checkWriteCapacity(f.backing, int64(len(data))); e != 0 {
		return 0, e
	}
	if e := f.inj.Check(OpWrite, f.path, off, int64(len(data))); e != 0 {
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

// Allocate 覆写 fallocate：在透传给 backing 前先做容量判定。fallocate 显式分配磁盘块
// （与 truncate 的稀疏打洞不同），会真实增长 backing 已用，必须纳入容量模拟——否则
// `fallocate -l 10M` 在 --capacity 1M 下仍成功，绕过"模拟满先于 backing 真满"的承诺。
// size 为 uint64，饱和到 MaxInt64 再判定（超大 fallocate 直接 ENOSPC）。
func (f *FaultFile) Allocate(ctx context.Context, off uint64, size uint64, mode uint32) syscall.Errno {
	sz := int64(size)
	if uint64(sz) != size { // size 超 int64 范围：必然超容量
		sz = math.MaxInt64
	}
	if e := f.inj.checkWriteCapacity(f.backing, sz); e != 0 {
		return e
	}
	return f.LoopbackFile.Allocate(ctx, off, size, mode)
}
