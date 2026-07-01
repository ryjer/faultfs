package faultfs

import (
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// calibrateCached 懒校准 backing 目录的实测性能上限（随机寻址延迟 + 顺序带宽），
// 结果缓存在 Injector 上供后续 set-latency 复用。用 sync.Once 保证首次调用独占
// 实测（耗时约几十毫秒）：并发的 set-latency 请求会阻塞等待首个调用完成，而不是
// 各自重跑 Calibrate（既浪费 I/O，又会在 backing 内同时创建多份校准文件）。之后
// 命中缓存。backing 为空或不复存在时，Calibrate 返回错误，钳制阶段据此透传不钳。
func (in *Injector) calibrateCached(backing string) (randLatency time.Duration, seqBw float64, err error) {
	in.calibOnce.Do(func() {
		r, b, e := Calibrate(backing)
		in.mu.Lock()
		in.calibRand, in.calibBw, in.calibErr, in.calibDone = r, b, e, true
		in.mu.Unlock()
	})
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.calibRand, in.calibBw, in.calibErr
}

// CalibratedFloor 返回缓存的 backing 实测性能上限（随机寻址延迟、顺序带宽）与是否
// 已校准。未校准时 randLatency/seqBw 为 0、ok 为 false。库用户可据此判断当前 profile
// 是否已被钳制到 backing（tmpfs）上限。
func (in *Injector) CalibratedFloor() (randLatency time.Duration, seqBw float64, ok bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.calibRand, in.calibBw, in.calibDone && in.calibErr == nil && in.calibRand > 0
}

// SetProfileCalibrated 按 backing 实测上限钳制 target 后写入 profile，返回钳制告警
// （可能为空）。库用户与 set-latency 控制路径共用本方法，确保"按 backing 钳制"的策略
// 只此一处实现（避免 CLI 与文档示例两套钳制逻辑各自漂移）。校准失败不阻断：透传目标，
// 仅返回告警。
func (in *Injector) SetProfileCalibrated(backing string, target LatencyProfile) []string {
	measRand, measBw, calibErr := in.calibrateCached(backing)
	switch {
	case calibErr != nil:
		in.SetProfile(target)
		return []string{"backing 性能校准失败(" + calibErr.Error() + ")，已跳过 tmpfs 钳制"}
	case measRand > 0 || measBw > 0:
		adj, warns := AdjustProfile(target, measRand, measBw)
		in.SetProfile(adj)
		return warns
	}
	in.SetProfile(target)
	return nil
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
		"read_rand":    p.ReadRand.String(),
		"read_seq":     p.ReadSeq.String(),
		"write_rand":   p.WriteRand.String(),
		"write_seq":    p.WriteSeq.String(),
		"open":         p.Open.String(),
		"getattr":      p.Getattr.String(),
		"statfs":       p.Statfs.String(),
		"setattr":      p.Setattr.String(),
		"getxattr":     p.Getxattr.String(),
		"setxattr":     p.Setxattr.String(),
		"removexattr":  p.Removexattr.String(),
		"listxattr":    p.Listxattr.String(),
		"create":       p.Create.String(),
		"mkdir":         p.Mkdir.String(),
		"unlink":       p.Unlink.String(),
		"rename":       p.Rename.String(),
		"fsync":        p.Fsync.String(),
		"flush":        p.Flush.String(),
		"read_byte":    byteOrUnlimited(p.ReadByte),
		"write_byte":   byteOrUnlimited(p.WriteByte),
	}
}

// ---- 手动性能参数（随机寻址延迟 + 顺序读写速度）----

// 随机寻址延迟与顺序读写速度是用户可手动调节的两个性能旋钮，对应真实设备的两个
// 核心指标：随机寻道/访问延迟、顺序传输带宽。它们与预设档（ProfileHDD/SSD/Memory）
// 等价但更直观——前者用 ns/us/ms，后者用 MiB/s、GiB/s。

const (
	// MiB / GiB 字节数，用于顺序速度单位换算。
	MiB float64 = 1 << 20
	GiB float64 = 1 << 30
)

// ParseLatency 把延迟旋钮字符串解析为 time.Duration。接受 Go duration（"8ms"/
// "200us"/"200µs"/"100ns"/"5s"，单位 ns/us/ms/s）以及裸整数（视为 ns）。
// 空串与负值均报错（负延迟会让 sleepFor 静默当作不延迟，即"要慢却变快"）。
func ParseLatency(s string) (time.Duration, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, &knobParseError{kind: "latency", raw: s, hint: "不能为空；示例：8ms / 200us / 100ns"}
	}
	if d, err := time.ParseDuration(t); err == nil {
		if d < 0 {
			return 0, &knobParseError{kind: "latency", raw: s, hint: "延迟不可为负"}
		}
		return d, nil
	}
	// 裸整数 → 纳秒（与 latency 的 SI 基本单位一致）。
	if n, err := parseStrictInt(t); err == nil {
		if n < 0 {
			return 0, &knobParseError{kind: "latency", raw: s, hint: "延迟不可为负"}
		}
		return time.Duration(n), nil
	}
	return 0, &knobParseError{kind: "latency", raw: s, hint: "示例：8ms / 200us / 100ns"}
}

// ParseSpeed 把顺序速度旋钮字符串解析为字节/秒。接受 "100M"/"100MiB/s"（=100MiB/s）、
// "2G"/"2GiB/s"（=2GiB/s）、"512K"/"512KiB/s"，以及裸数字（=字节/秒）。大小写不敏感。
// 0（含 "0"/"0M"）=不限速；正带宽必须 ≥ 1 B/s（更慢会让 per-byte 延迟溢出或挂死）。
func ParseSpeed(s string) (float64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, &knobParseError{kind: "speed", raw: s, hint: "不能为空；示例：100M / 2G / 100MiB/s"}
	}
	u := strings.ToLower(t)
	u = strings.TrimSuffix(u, "/s")
	mult := 1.0
	switch {
	case strings.HasSuffix(u, "gib"):
		mult, u = GiB, strings.TrimSuffix(u, "gib")
	case strings.HasSuffix(u, "g"):
		mult, u = GiB, strings.TrimSuffix(u, "g")
	case strings.HasSuffix(u, "mib"):
		mult, u = MiB, strings.TrimSuffix(u, "mib")
	case strings.HasSuffix(u, "m"):
		mult, u = MiB, strings.TrimSuffix(u, "m")
	case strings.HasSuffix(u, "kib"):
		mult, u = 1 << 10, strings.TrimSuffix(u, "kib")
	case strings.HasSuffix(u, "k"):
		mult, u = 1 << 10, strings.TrimSuffix(u, "k")
	}
	f, err := parseFloat(strings.TrimSpace(u))
	if err != nil {
		return 0, &knobParseError{kind: "speed", raw: s, hint: "示例：100M / 2G / 100MiB/s（M=MiB/s，G=GiB/s）"}
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, &knobParseError{kind: "speed", raw: s, hint: "速度不能为 NaN/Inf"}
	}
	if f < 0 {
		return 0, &knobParseError{kind: "speed", raw: s, hint: "速度不可为负"}
	}
	bw := f * mult
	// bw=0 合法（=不限速）。正带宽须 ≥ 1 B/s：per-byte 延迟 = 1s/bw 以 int64 纳秒存储，
	// bw<1 会让单字节延迟 >1s（大读挂死）乃至 1s/bw 溢出 int64 回绕成负（被 sleepFor
	// 当作不延迟，"要慢却变快"）。
	if bw > 0 && bw < 1 {
		return 0, &knobParseError{kind: "speed", raw: s, hint: "速度过小（最小 1 B/s；0=不限速）"}
	}
	return bw, nil
}

// FormatSpeed 把字节/秒格式化为人类可读的速度串（如 "100MiB/s"、"2.5GiB/s"）。
// 用最短浮点表示（strconv 精度 -1），自动省去末尾多余的 0，且不损失精度。
func FormatSpeed(bw float64) string {
	if bw <= 0 {
		return "unlimited"
	}
	switch {
	case bw >= GiB:
		return trimFloat(bw/GiB) + "GiB/s"
	case bw >= MiB:
		return trimFloat(bw/MiB) + "MiB/s"
	case bw >= 1<<10:
		return trimFloat(bw/(1<<10)) + "KiB/s"
	default:
		return trimFloat(bw) + "B/s"
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

// ---- backing（通常 tmpfs）校准 + 性能钳制 ----
//
// faultfs 通过"叠加延迟"模拟更慢的设备，因此可模拟的性能上限 = backing 本身的
// 性能（最强即基于内存的 tmpfs）。Calibrate 实测 backing 的随机寻址延迟与顺序带宽，
// AdjustProfile 据此把目标参数里"比 backing 还快"的部分钳制为 0 并告警——这正是
// "用更强的 tmpfs 模拟更弱的系统；当预设值超出 tmpfs 性能时提示并改用 tmpfs 模拟"。

const (
	calibSeqSize = 8 << 20 // 校准用顺序文件大小：8 MiB
	calibBlock   = 4 << 10 // 随机读块大小：4 KiB
	calibRandOps = 1024    // 随机读采样次数
)

// calibDir 选择用于放置校准临时文件的目录：优先 backing 的父目录（与 backing 同
// 设备、但不在 FUSE 暴露的 backing 根下，校准期间 .faultfs-calib-* 不会透过挂载点
// 被 readdir 看到）；若父目录在别的设备或不可用，回退到 backing 本身（仍测同设备，
// 仅短暂可见）。backing 自身由 Calibrate 先 MkdirAll 确保存在。
func calibDir(backing string) string {
	parent := filepath.Dir(backing)
	if parent == backing {
		return backing
	}
	var sb, sp syscall.Stat_t
	if syscall.Stat(backing, &sb) != nil {
		return backing
	}
	if syscall.Stat(parent, &sp) != nil {
		return backing
	}
	if sb.Dev != sp.Dev {
		return backing // 父目录在不同设备：只能在 backing 内测才准
	}
	return parent
}

// Calibrate 测量 backing 目录所在设备的随机寻址延迟（单次 4KiB 随机读均摊）与顺序
// 读带宽（字节/秒），作为可模拟的性能上限。在 calibDir（同设备的 backing 父目录，
// 不可用时回退 backing）下创建临时文件做实测，结束即删。用于把用户/预设的目标性能
// 钳制到 backing 实际可达范围内（见 [AdjustProfile]）。
func Calibrate(backing string) (randLatency time.Duration, seqBw float64, err error) {
	if err := os.MkdirAll(backing, 0o755); err != nil {
		return 0, 0, err
	}
	f, err := os.CreateTemp(calibDir(backing), ".faultfs-calib-*")
	if err != nil {
		return 0, 0, err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	buf := make([]byte, calibBlock)
	// 预分配 calibSeqSize（Truncate 建稀疏文件），再用各不相同的块填充，避免稀疏/
	// 透明压缩影响读取实测。
	if err := f.Truncate(int64(calibSeqSize)); err != nil {
		return 0, 0, err
	}
	for off := 0; off < calibSeqSize; off += calibBlock {
		for i := range buf {
			buf[i] = byte(off>>10) ^ byte(i)
		}
		if _, err := f.WriteAt(buf, int64(off)); err != nil {
			return 0, 0, err
		}
	}

	big := make([]byte, calibSeqSize)
	// 顺序读：跑两遍，取较快（已预热 page cache）的一遍，贴近 faultfs 实际看到的缓存读。
	// 用 io.ReadFull 保证读到完整 buffer（或捕获短读），并按实际字节数算带宽，避免
	// 把没读到的字节计入而高估带宽。
	var bestSeq = time.Duration(0)
	var bestN = 0
	for pass := 0; pass < 2; pass++ {
		if _, err := f.Seek(0, 0); err != nil {
			return 0, 0, err
		}
		t0 := time.Now()
		n, rerr := io.ReadFull(f, big)
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			return 0, 0, rerr
		}
		if d := time.Since(t0); n > 0 && d > 0 && (bestSeq == 0 || d < bestSeq) {
			bestSeq, bestN = d, n
		}
	}
	if bestN > 0 {
		seqBw = float64(bestN) / bestSeq.Seconds()
	}

	// 随机读：calibRandOps 次散布的 4KiB 读，均摊得单次寻址延迟。
	offsets := pseudoRandomOffsets(calibSeqSize, calibBlock, calibRandOps)
	if _, err := f.Seek(0, 0); err != nil {
		return 0, 0, err
	}
	t0 := time.Now()
	for _, off := range offsets {
		if _, err := f.ReadAt(buf, off); err != nil {
			return 0, 0, err
		}
	}
	randLatency = time.Since(t0) / time.Duration(len(offsets))
	return randLatency, seqBw, nil
}

// AdjustProfile 把目标 profile 钳制到 backing 实测上限（measuredRand / measuredBw）之内：
// faultfs 只能叠加延迟，故 effectiveRand = max(0, targetRand - measuredRand)、
// effectiveByte = max(0, targetByte - measuredByte)。当目标比 backing 更快（被钳到 0）
// 时产出告警。measuredRand<=0 或 measuredBw<=0 视为该维度未校准，对应字段透传不钳制。
// 各元数据 op（open/getattr/…）也由 applyRandLatency 按一次随机寻址写入，故一并按
// measuredRand 钳制（静默，告警已由随机读/写覆盖，避免重复刷屏）。
func AdjustProfile(p LatencyProfile, measuredRand time.Duration, measuredBw float64) (LatencyProfile, []string) {
	out := p
	var warns []string
	// 随机寻址延迟钳制（per-request）。
	if measuredRand > 0 {
		out.ReadRand, _ = subClampDur(p.ReadRand, measuredRand, &warns, "随机读")
		out.WriteRand, _ = subClampDur(p.WriteRand, measuredRand, &warns, "随机写")
		// 元数据 op 同样按一次随机寻址计（见 applyRandLatency），一并钳制；
		// 传 nil warns 静默钳制（随机读/写的告警已足以提示"目标快于 backing"）。
		for _, f := range []*time.Duration{
			&out.Open, &out.Getattr, &out.Statfs, &out.Setattr,
			&out.Getxattr, &out.Setxattr, &out.Removexattr, &out.Listxattr,
			&out.Create, &out.Mkdir, &out.Unlink, &out.Rename,
			&out.Fsync, &out.Flush,
		} {
			*f, _ = subClampDur(*f, measuredRand, nil, "")
		}
	}
	// 顺序带宽钳制（per-byte → 带宽）。
	if measuredBw > 0 {
		measuredByte := bwToByteDur(measuredBw)
		eb, _ := subClampDur(p.ReadByte, measuredByte, &warns, "顺序读")
		out.ReadByte = eb
		eb, _ = subClampDur(p.WriteByte, measuredByte, &warns, "顺序写")
		out.WriteByte = eb
	}
	return out, warns
}

// subClampDur 计算"叠加延迟" = max(0, target - measured)。target<=0 表示无延迟需求，
// 直接返回 0 不告警；target<measured（目标快于 backing）则钳到 0 并在 warns 追加一条
// （warns 为 nil 时静默，用于批量钳制派生字段而不重复告警）。
func subClampDur(target, measured time.Duration, warns *[]string, label string) (time.Duration, bool) {
	if target <= 0 {
		return 0, false
	}
	if measured <= 0 {
		return target, false
	}
	if target > measured {
		return target - measured, false
	}
	if warns != nil {
		*warns = append(*warns, label+"目标("+target.String()+")快于 backing("+measured.String()+")，已钳制到 backing（tmpfs）性能")
	}
	return 0, true
}

// pseudoRandomOffsets 用确定性的 xorshift 生成 [0, size-block) 内的伪随机块对齐 offset，
// 避免引入 math/rand 的全局状态依赖。
func pseudoRandomOffsets(size, block, n int) []int64 {
	out := make([]int64, n)
	state := uint64(0x9e3779b97f4a7c15)
	nblocks := size / block
	for i := 0; i < n; i++ {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		b := int(state % uint64(nblocks))
		out[i] = int64(b * block)
	}
	return out
}
