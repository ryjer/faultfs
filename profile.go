package faultfs

import (
	"math"
	"strings"
	"time"
)

// LatencyProfile 描述一类设备的 I/O 延迟模型：read/write 分随机/顺序，外加各
// 元数据 op 的固定延迟，以及可选的 per-byte 带宽（模拟顺序传输速率）。全 0
// 表示不延迟（[ProfileNone]，默认）。
type LatencyProfile struct {
	ReadRand    time.Duration // 随机读（每次请求）
	ReadSeq     time.Duration // 顺序读（每次请求）
	WriteRand   time.Duration // 随机写
	WriteSeq    time.Duration // 顺序写
	Open        time.Duration
	Getattr     time.Duration
	Statfs      time.Duration
	Setattr     time.Duration
	Getxattr    time.Duration
	Setxattr    time.Duration
	Removexattr time.Duration
	Listxattr   time.Duration
	Create      time.Duration
	Mkdir       time.Duration
	Unlink      time.Duration
	Rename      time.Duration
	Fsync       time.Duration
	Flush       time.Duration
	ReadByte    time.Duration // 每字节额外延迟（带宽限制）；0=不限
	WriteByte   time.Duration
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
		Setattr: 3 * time.Microsecond, Getxattr: 2 * time.Microsecond, Setxattr: 3 * time.Microsecond,
		Removexattr: 2 * time.Microsecond, Listxattr: 2 * time.Microsecond,
		Create: 8 * time.Microsecond, Mkdir: 8 * time.Microsecond,
		Unlink: 8 * time.Microsecond, Rename: 10 * time.Microsecond,
		Fsync: 10 * time.Microsecond, Flush: 2 * time.Microsecond,
	}

	// ProfileSSD 模拟固态盘：随机 ~150μs、顺序明显更快，带宽充裕。
	ProfileSSD = LatencyProfile{
		ReadRand: 150 * time.Microsecond, ReadSeq: 50 * time.Microsecond,
		WriteRand: 200 * time.Microsecond, WriteSeq: 50 * time.Microsecond,
		Open: 100 * time.Microsecond, Getattr: 80 * time.Microsecond, Statfs: 100 * time.Microsecond,
		Setattr: 100 * time.Microsecond, Getxattr: 80 * time.Microsecond, Setxattr: 120 * time.Microsecond,
		Removexattr: 80 * time.Microsecond, Listxattr: 80 * time.Microsecond,
		Create: 200 * time.Microsecond, Mkdir: 200 * time.Microsecond,
		Unlink: 200 * time.Microsecond, Rename: 300 * time.Microsecond,
		Fsync: 500 * time.Microsecond, Flush: 50 * time.Microsecond,
	}

	// ProfileHDD 模拟传统机械盘：随机 ~8ms（寻道主导），顺序 ~200μs + 带宽受限。
	ProfileHDD = LatencyProfile{
		ReadRand: 8 * time.Millisecond, ReadSeq: 200 * time.Microsecond,
		WriteRand: 10 * time.Millisecond, WriteSeq: 300 * time.Microsecond,
		Open: 5 * time.Millisecond, Getattr: 5 * time.Millisecond, Statfs: 5 * time.Millisecond,
		Setattr: 5 * time.Millisecond, Getxattr: 5 * time.Millisecond, Setxattr: 8 * time.Millisecond,
		Removexattr: 5 * time.Millisecond, Listxattr: 5 * time.Millisecond,
		Create: 10 * time.Millisecond, Mkdir: 10 * time.Millisecond,
		Unlink: 10 * time.Millisecond, Rename: 12 * time.Millisecond,
		Fsync: 15 * time.Millisecond, Flush: 5 * time.Millisecond,
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

// SetProfile 设延迟模型（在线可改）；同步更新初始快照，故 Refresh 会还原到该 profile。
func (in *Injector) SetProfile(p LatencyProfile) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.profile = p
	in.initialProfile = p
}

// Profile 返回当前延迟模型。
func (in *Injector) Profile() LatencyProfile {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.profile
}

// SetSpeed 设全局倍速：1.0 正常、>1 慢放、<1 快放（<=0 视为 1）。实际延迟 =
// profile 值 × speed。同步更新初始快照，故 Refresh 会还原到该 speed。
func (in *Injector) SetSpeed(s float64) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if s <= 0 {
		s = 1
	}
	in.speed = s
	in.initialSpeed = s
}

// Speed 返回当前倍速。
func (in *Injector) Speed() float64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.speed
}

// byteOrUnlimited 把零值 Duration 展示为 "unlimited"，非零值用默认格式。
func byteOrUnlimited(d time.Duration) string {
	if d == 0 {
		return "unlimited"
	}
	return d.String()
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
		"read_rand":   p.ReadRand.String(),
		"read_seq":    p.ReadSeq.String(),
		"write_rand":  p.WriteRand.String(),
		"write_seq":   p.WriteSeq.String(),
		"open":        p.Open.String(),
		"getattr":     p.Getattr.String(),
		"statfs":      p.Statfs.String(),
		"setattr":     p.Setattr.String(),
		"getxattr":    p.Getxattr.String(),
		"setxattr":    p.Setxattr.String(),
		"removexattr": p.Removexattr.String(),
		"listxattr":   p.Listxattr.String(),
		"create":      p.Create.String(),
		"mkdir":       p.Mkdir.String(),
		"unlink":      p.Unlink.String(),
		"rename":      p.Rename.String(),
		"fsync":       p.Fsync.String(),
		"flush":       p.Flush.String(),
		"read_byte":   byteOrUnlimited(p.ReadByte),
		"write_byte":  byteOrUnlimited(p.WriteByte),
	}
}

// bwToByteDur 把顺序带宽（字节/秒）换算成 per-byte 延迟；bw<=0 → 0（不限速）。
// 做溢出保护：若 1s/bw 超出 int64 纳秒可表示范围（极端慢速），钳到最大正 Duration
// （仍表达"极慢"），避免回绕成负被 sleepFor 当作 d<=0 而静默不延迟（"要慢却变快"）。
func bwToByteDur(bw float64) time.Duration {
	if bw <= 0 {
		return 0
	}
	d := float64(time.Second) / bw
	if d >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(d)
}

// byteDurToBw 把 per-byte 延迟换算回带宽（字节/秒）；d<=0 → 0（不限速）。
func byteDurToBw(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(time.Second) / float64(d)
}

// ProfileFromKnobs 由两个手动性能旋钮构建一份完整的 LatencyProfile：rand=随机寻址
// 延迟（读/写及各元数据 op 均按一次寻址计），seqBw=顺序读写带宽（转成 per-byte 延迟，
// <=0 表示不限速）。顺序访问的 per-request 部分（*Seq）为 0，由带宽主导。库用户可直接
// 用它构造 profile，再交给 [Injector.SetProfile]；若需按 backing（tmpfs）上限钳制，再用
// [Calibrate] + [AdjustProfile]。
func ProfileFromKnobs(rand time.Duration, seqBw float64) LatencyProfile {
	p := LatencyProfile{}
	applyRandLatency(&p, rand)
	applySeqSpeed(&p, seqBw)
	return p
}

// applyRandLatency 把随机寻址延迟 rand 写入 profile 的随机读/写与各元数据 op 字段
// （真实设备上每个元数据 op 也付出一次寻址代价）。顺序 per-request 字段（*Seq）不动，
// 顺序访问由带宽（*Byte）主导。
func applyRandLatency(p *LatencyProfile, rand time.Duration) {
	p.ReadRand, p.WriteRand = rand, rand
	p.Open, p.Getattr, p.Statfs, p.Setattr = rand, rand, rand, rand
	p.Getxattr, p.Setxattr, p.Removexattr, p.Listxattr = rand, rand, rand, rand
	p.Create, p.Mkdir, p.Unlink, p.Rename = rand, rand, rand, rand
	p.Fsync, p.Flush = rand, rand
}

// applySeqSpeed 把顺序读写带宽 seqBw 写入 profile 的 per-byte 字段。
func applySeqSpeed(p *LatencyProfile, seqBw float64) {
	p.ReadByte = bwToByteDur(seqBw)
	p.WriteByte = bwToByteDur(seqBw)
}

// latencyStateText 把 profile/speed 格式化为紧凑串（profile=<name> speed=<v>）。
func latencyStateText(p LatencyProfile, speed float64) string {
	return "profile=" + profileName(p) + " speed=" + trimFloat(speed)
}
