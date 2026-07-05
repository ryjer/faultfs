package faultfs

import (
	"math"
	"time"
)

// sleepFor 阻塞 d×sp（d<=0 不阻塞）。调用时已离开 mu 锁。
func (in *Injector) sleepFor(d time.Duration, sp float64) {
	if d <= 0 {
		return
	}
	if sp <= 0 {
		sp = 1
	}
	time.Sleep(time.Duration(float64(d) * sp))
}

// addByteDelay 把 per-byte 带宽延迟（perByte × n 字节）叠加到 d，并做溢出保护：
// perByte 与 n 均非负，乘积溢出 int64 时可能回绕成负、0 或任意正值（如 1e10×2^55 恰好
// 回绕到 0），故用"逆除校验"而非仅判负，溢出则钳到最大正 Duration（仍表达"极慢"），
// 避免 sleepFor 把回绕值当作 d<=0 而静默不限速（即"要慢却变快"）。
func addByteDelay(d time.Duration, perByte time.Duration, n int) time.Duration {
	if n <= 0 || perByte <= 0 {
		return d
	}
	pb := int64(perByte)
	bd := pb * int64(n)
	if bd/pb != int64(n) { // 溢出：回绕后的积除回因子不等于另一因子
		bd = math.MaxInt64
	}
	return d + time.Duration(bd)
}

// DelayRead 按“顺序/随机”选取 read 延迟并叠加 per-byte 带宽后阻塞。
// sequential 由调用方据 lastOff 判定；n 为本次读字节数。
func (in *Injector) DelayRead(sequential bool, n int) {
	in.mu.Lock()
	p, sp := in.profile, in.speed
	in.mu.Unlock()
	d := p.ReadRand
	if sequential {
		d = p.ReadSeq
	}
	d = addByteDelay(d, p.ReadByte, n)
	in.sleepFor(d, sp)
}

// DelayWrite 同理，作用于写。
func (in *Injector) DelayWrite(sequential bool, n int) {
	in.mu.Lock()
	p, sp := in.profile, in.speed
	in.mu.Unlock()
	d := p.WriteRand
	if sequential {
		d = p.WriteSeq
	}
	d = addByteDelay(d, p.WriteByte, n)
	in.sleepFor(d, sp)
}

// DelayOp 阻塞 node 级 op（open/getattr/statfs/setattr/xattr/create/mkdir/unlink/
// rename/fsync/flush）对应的固定延迟。
func (in *Injector) DelayOp(op string) {
	in.mu.Lock()
	p, sp := in.profile, in.speed
	in.mu.Unlock()
	var d time.Duration
	switch op {
	case OpOpen:
		d = p.Open
	case OpGetattr:
		d = p.Getattr
	case OpStatfs:
		d = p.Statfs
	case OpSetattr:
		d = p.Setattr
	case OpGetxattr:
		d = p.Getxattr
	case OpSetxattr:
		d = p.Setxattr
	case OpRemovexattr:
		d = p.Removexattr
	case OpListxattr:
		d = p.Listxattr
	case OpCreate:
		d = p.Create
	case OpMkdir:
		d = p.Mkdir
	case OpUnlink:
		d = p.Unlink
	case OpRename:
		d = p.Rename
	case OpFsync:
		d = p.Fsync
	case OpFlush:
		d = p.Flush
	}
	in.sleepFor(d, sp)
}
