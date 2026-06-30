package faultfs

import (
	"strings"
	"time"
)

// LatencyProfile 描述一类设备的 I/O 延迟模型：read/write 分随机/顺序，外加各
// 元数据 op 的固定延迟，以及可选的 per-byte 带宽（模拟顺序传输速率）。全 0
// 表示不延迟（[ProfileNone]，默认）。
type LatencyProfile struct {
	ReadRand  time.Duration // 随机读（每次请求）
	ReadSeq   time.Duration // 顺序读（每次请求）
	WriteRand time.Duration // 随机写
	WriteSeq  time.Duration // 顺序写
	Open      time.Duration
	Getattr   time.Duration
	Statfs    time.Duration
	Getxattr  time.Duration
	Setxattr  time.Duration
	Create    time.Duration
	Mkdir     time.Duration
	Unlink    time.Duration
	Rename    time.Duration
	ReadByte  time.Duration // 每字节额外延迟（带宽限制）；0=不限
	WriteByte time.Duration
}

// 预设档位，覆盖三类典型设备。数值仅作量级参考（测试里可再叠加全局 speed）。
var (
	// ProfileNone 全 0，不延迟（默认）：保证 v1 注入测试性能不受影响。
	ProfileNone = LatencyProfile{}

	// ProfileMemory 模拟 tmpfs / 内存盘：μs 级，随机≈顺序。
	ProfileMemory = LatencyProfile{
		ReadRand: 2 * time.Microsecond, ReadSeq: 1 * time.Microsecond,
		WriteRand: 3 * time.Microsecond, WriteSeq: 1 * time.Microsecond,
		Open: 5 * time.Microsecond, Getattr: 2 * time.Microsecond, Statfs: 10 * time.Microsecond,
		Getxattr: 2 * time.Microsecond, Setxattr: 3 * time.Microsecond,
		Create: 8 * time.Microsecond, Mkdir: 8 * time.Microsecond,
		Unlink: 8 * time.Microsecond, Rename: 10 * time.Microsecond,
	}

	// ProfileSSD 模拟固态盘：随机 ~150μs、顺序明显更快，带宽充裕。
	ProfileSSD = LatencyProfile{
		ReadRand: 150 * time.Microsecond, ReadSeq: 50 * time.Microsecond,
		WriteRand: 200 * time.Microsecond, WriteSeq: 50 * time.Microsecond,
		Open: 100 * time.Microsecond, Getattr: 80 * time.Microsecond, Statfs: 100 * time.Microsecond,
		Getxattr: 80 * time.Microsecond, Setxattr: 120 * time.Microsecond,
		Create: 200 * time.Microsecond, Mkdir: 200 * time.Microsecond,
		Unlink: 200 * time.Microsecond, Rename: 300 * time.Microsecond,
	}

	// ProfileHDD 模拟传统机械盘：随机 ~8ms（寻道主导），顺序 ~200μs + 带宽受限。
	ProfileHDD = LatencyProfile{
		ReadRand: 8 * time.Millisecond, ReadSeq: 200 * time.Microsecond,
		WriteRand: 10 * time.Millisecond, WriteSeq: 300 * time.Microsecond,
		Open: 5 * time.Millisecond, Getattr: 5 * time.Millisecond, Statfs: 5 * time.Millisecond,
		Getxattr: 5 * time.Millisecond, Setxattr: 8 * time.Millisecond,
		Create: 10 * time.Millisecond, Mkdir: 10 * time.Millisecond,
		Unlink: 10 * time.Millisecond, Rename: 12 * time.Millisecond,
		ReadByte:  100 * time.Nanosecond, // ~10 MB/s 顺序读带宽
		WriteByte: 120 * time.Nanosecond,
	}
)

// ProfileByName 按名查预设："none"/""、"memory"/"tmpfs"/"ram"、"ssd"、"hdd"/"disk"
// （大小写不敏感）。ok 为 false 表示未知档名。
func ProfileByName(name string) (LatencyProfile, bool) {
	switch strings.ToLower(name) {
	case "", "none":
		return ProfileNone, true
	case "memory", "tmpfs", "ram":
		return ProfileMemory, true
	case "ssd":
		return ProfileSSD, true
	case "hdd", "disk":
		return ProfileHDD, true
	}
	return LatencyProfile{}, false
}

// SetProfile 设延迟模型（在线可改）。
func (in *Injector) SetProfile(p LatencyProfile) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.profile = p
}

// Profile 返回当前延迟模型。
func (in *Injector) Profile() LatencyProfile {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.profile
}

// SetSpeed 设全局倍速：1.0 正常、>1 慢放、<1 快放（<=0 视为 1）。实际延迟 =
// profile 值 × speed。
func (in *Injector) SetSpeed(s float64) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if s <= 0 {
		s = 1
	}
	in.speed = s
}

// Speed 返回当前倍速。
func (in *Injector) Speed() float64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.speed
}

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
	if n > 0 {
		d += time.Duration(int64(p.ReadByte) * int64(n))
	}
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
	if n > 0 {
		d += time.Duration(int64(p.WriteByte) * int64(n))
	}
	in.sleepFor(d, sp)
}

// DelayOp 阻塞 node 级 op（open/getattr/statfs/xattr/create/mkdir/unlink/rename）
// 对应的固定延迟。
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
	case OpGetxattr:
		d = p.Getxattr
	case OpSetxattr:
		d = p.Setxattr
	case OpCreate:
		d = p.Create
	case OpMkdir:
		d = p.Mkdir
	case OpUnlink:
		d = p.Unlink
	case OpRename:
		d = p.Rename
	}
	in.sleepFor(d, sp)
}

// profileName 反查 LatencyProfile 对应的预设档名；不匹配任何预设则 "custom"。
func profileName(p LatencyProfile) string {
	switch p {
	case ProfileNone:
		return "none"
	case ProfileMemory:
		return "memory"
	case ProfileSSD:
		return "ssd"
	case ProfileHDD:
		return "hdd"
	}
	return "custom"
}

// profileFields 把 LatencyProfile 各字段展平为 名→值（duration.String()）的 map，
// 供 dump 序列化展示。
func profileFields(p LatencyProfile) map[string]string {
	return map[string]string{
		"read_rand":  p.ReadRand.String(),
		"read_seq":   p.ReadSeq.String(),
		"write_rand": p.WriteRand.String(),
		"write_seq":  p.WriteSeq.String(),
		"open":       p.Open.String(),
		"getattr":    p.Getattr.String(),
		"statfs":     p.Statfs.String(),
		"getxattr":   p.Getxattr.String(),
		"setxattr":   p.Setxattr.String(),
		"create":     p.Create.String(),
		"mkdir":      p.Mkdir.String(),
		"unlink":     p.Unlink.String(),
		"rename":     p.Rename.String(),
		"read_byte":  p.ReadByte.String(),
		"write_byte": p.WriteByte.String(),
	}
}
