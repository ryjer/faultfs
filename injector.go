package faultfs

import (
	"sync"
	"sync/atomic"
	"time"
)

// Injector 是线程安全的故障注入规则集 + 设备性能模型。多个 FUSE 回调与
// control server 可并发查询/修改它。
type Injector struct {
	mu     sync.Mutex
	rules  []ruleState
	nextID int
	// 备用块预算：spareCount 个 spareBlockSize 字节的块。spareCount=-1 无限，默认 0
	// （无备用，需显式 SetSpare/SetSpareBlocks 分配）。治愈一段坏区时按
	// ceil(坏区长度/spareBlockSize) 整块消耗。initial* 为 Refresh 还原用的初始快照
	// （由各 setter 同步更新，故 Refresh 复位到最近一次 set 的值）。
	spareCount            int64
	spareBlockSize        int64
	initialSpareCount     int64
	initialSpareBlockSize int64
	profile               LatencyProfile
	initialProfile        LatencyProfile // 初始 profile（Refresh 还原用）
	speed                 float64        // 倍率；<=0 视为 1
	initialSpeed          float64        // 初始 speed（Refresh 还原用）

	// 模拟容量上限（字节）；0=未启用（默认，直透 backing 真实容量）。mount 时按 backing
	// statfs 校验并固化：保证 capacity∈(backing已用, backing总量)，使 faultfs 模拟的"满"
	// 先于 backing 真满触发。不被消耗、不进 Refresh（改值需重新挂载）。用 atomic 存取：
	// Write 热路径 lock-free 读 capacity，避免每 write 额外一次 in.mu。
	capacity atomic.Int64

	// capacity 已用字节数的运行估值，供容量判定（checkWriteCapacity）读、避免每 write 一次 statfs：
	// TTL 到期时由真实 statfs 重新对齐（usedCachedAt 为对齐时刻，UnixNano，0=未对齐）；TTL 内则随
	// 每条放行的 write/fallocate 按 n 乐观累加——否则一个在 TTL 窗口（~10ms）内完成的突发写会一直
	// 读到旧 used 而全部放行、超额写入（见 usedCacheTTL）。statfs 失败时沿用上次估值（fail-open，
	// 与 mount 时 statfs 失败拒绝挂载的 fail-closed 互补）。两个原子量无锁读。
	usedCached   atomic.Int64
	usedCachedAt atomic.Int64

	// backing（通常 tmpfs）实测性能上限，缓存以便重复 set-latency 复用。calibOnce
	// 保证首次校准独占执行（并发请求阻塞等待，不重跑 Calibrate）；calibDone 标记是否
	// 已校准，calibErr 非 nil 表示校准失败（此时跳过钳制）。Injector 始终以指针使用，
	// 故 sync.Once 不会被复制。
	calibRand time.Duration
	calibBw   float64
	calibErr  error
	calibDone bool
	calibOnce sync.Once
}

// NewInjector 建一个空规则集：spare=0（无备用，需显式分配）、blockSize=1、speed 1.0、
// ProfileNone（不模拟延迟，直透 backing）。
func NewInjector() *Injector {
	return &Injector{
		spareCount:            0,
		spareBlockSize:        1,
		initialSpareCount:     0,
		initialSpareBlockSize: 1,
		speed:                 1,
		initialSpeed:          1,
	}
}

// Add 追加一条规则并返回分配的 ID。
func (in *Injector) Add(r Rule) int {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.nextID++
	r.ID = in.nextID
	// HealOnWrite 语义仅对 read 规则有意义（read 命中 EIO、write 触发治愈）。强制 Op=OpRead，
	// 使 HealOnWrite 规则无论调用方写 Op=""（任意 op）还是误填其它 op，都不会因下游 op 匹配
	// 的 `r.Op != ""` / `r.Op == OpRead` 守卫而静默退化为"永久 EIO、永不治愈"。
	if r.HealOnWrite {
		r.Op = OpRead
	}
	rem := r.N
	if rem == 0 {
		rem = -1 // 永久
	}
	bs := in.spareBlockSize
	s := ruleState{r: r, remaining: rem, initialRem: rem, blockSize: bs}
	// HealOnWrite 坏扇区且按块模式（blockSize>1 且 OffLen>0）：按 ceil(OffLen/blockSize)
	// 为每块分配独立治愈标志，使部分覆盖 write 只治愈其实际写入的块（真硬盘语义）。
	// 否则整段模式（healedBlocks==nil），write 命中即整段治愈——兼容 blockSize=1 纯次数
	// 模式与 OffLen<=0 任意 offset 规则。
	if r.HealOnWrite && bs > 1 && r.OffLen > 0 {
		// 按块模式：为每块分配独立治愈标志。但块数过大（OffLen 接近 MaxInt64）时 make 会 OOM，
		// 故超 maxHealedBlocks 阈值回退整段模式（healedBlocks 留 nil，write 整段治愈、按
		// blocksNeeded 整体扣 spare）——语义不丢，仅放弃逐块治愈粒度。
		if n := blocksNeeded(r.OffLen, bs); n <= maxHealedBlocks {
			s.healedBlocks = make([]bool, n)
		}
	}
	in.rules = append(in.rules, s)
	return r.ID
}

// Delete 删除指定 ID 的规则，返回是否找到并删除。
func (in *Injector) Delete(id int) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	for i := range in.rules {
		if in.rules[i].r.ID == id {
			in.rules = append(in.rules[:i], in.rules[i+1:]...)
			return true
		}
	}
	return false
}

// Clear 清空所有规则。
func (in *Injector) Clear() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.rules = nil
}

// Reset 是 Clear 的别名（v1 兼容）。
func (in *Injector) Reset() { in.Clear() }

// List 返回所有规则的当前视图快照（含 healed/remaining 等运行时状态）。
func (in *Injector) List() []RuleView {
	in.mu.Lock()
	defer in.mu.Unlock()
	out := make([]RuleView, len(in.rules))
	for i, s := range in.rules {
		rv := RuleView{Rule: s.r, Healed: s.healed, Remaining: s.remaining}
		if s.healedBlocks != nil {
			rv.TotalBlocks = len(s.healedBlocks)
			for _, b := range s.healedBlocks {
				if b {
					rv.HealedBlocks++
				}
			}
			rv.Healed = rv.HealedBlocks == rv.TotalBlocks
		} else if s.r.HealOnWrite {
			rv.TotalBlocks = 1
			if s.healed {
				rv.HealedBlocks = 1
			}
		}
		out[i] = rv
	}
	return out
}
