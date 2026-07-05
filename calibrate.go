package faultfs

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

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
//
// rand（随机寻址延迟）是叠加增量，不需 backing 校准；仅当目标含顺序带宽（ReadByte/WriteByte
// >0，即"限制"语义需 backing 上限来判定是否告警）时才触发校准。rand-only 配置跳过几十 ms
// 校准直接写入。
func (in *Injector) SetProfileCalibrated(backing string, target LatencyProfile) []string {
	if target.ReadByte <= 0 && target.WriteByte <= 0 {
		in.SetProfile(target) // rand-only 或全 0：无带宽需钳制，跳过校准
		return nil
	}
	_, measBw, calibErr := in.calibrateCached(backing)
	switch {
	case calibErr != nil:
		in.SetProfile(target)
		return []string{"backing 性能校准失败(" + calibErr.Error() + ")，已跳过顺序带宽钳制"}
	default:
		adj, warns := AdjustProfile(target, measBw) // rand 不钳（增量），仅钳 seq 带宽
		in.SetProfile(adj)
		return warns
	}
}

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
	defer func() { _ = os.Remove(f.Name()) }()
	defer func() { _ = f.Close() }()

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

// AdjustProfile 把目标 profile 的顺序带宽钳制到 backing 实测上限（measuredBw）之内。
// rand（随机寻址延迟 + 由 applyRandLatency 派生的各元数据 op）语义为"在 backing 上叠加的
// 增量"——永远让设备更慢，故不钳制、原样透传。
// 顺序带宽语义为"限制上限"：faultfs 通过 per-byte sleep 把 host 速度降到目标，当目标带宽 >
// backing（想限到的速度比 backing 还快）时实际取 backing 并告警。measuredBw<=0 视为未校准，
// 带宽字段透传不钳。
func AdjustProfile(p LatencyProfile, measuredBw float64) (LatencyProfile, []string) {
	out := p
	var warns []string
	// 顺序带宽钳制（per-byte → 带宽）：限制语义，目标快于 backing 时取 backing 并告警。
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
