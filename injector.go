package faultfs

import (
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// 故障注入点：Rule.Op 取这些值之一，或 "" 表示任意 op。
const (
	OpOpen        = "open"
	OpCreate      = "create"
	OpRead        = "read"
	OpWrite       = "write"
	OpLookup      = "lookup"
	OpMkdir       = "mkdir"
	OpRmdir       = "rmdir"
	OpUnlink      = "unlink"
	OpRename      = "rename"
	OpGetattr     = "getattr"
	OpStatfs      = "statfs"
	OpSetattr     = "setattr"
	OpGetxattr    = "getxattr"
	OpSetxattr    = "setxattr"
	OpRemovexattr = "removexattr"
	OpListxattr   = "listxattr"
	OpFsync       = "fsync"
	OpFlush       = "flush"
)

// maxHealedBlocks 限制 HealOnWrite 按块模式分配的 healed 标志数，防 OffLen 极大（如
// MaxInt64）时 make([]bool, N) OOM。超此阈值的规则回退整段模式（healedBlocks 留 nil，
// write 整段治愈、按 blocksNeeded 整体扣 spare）——语义不丢，仅放弃逐块治愈粒度。
// 1<<20 块 × 4KiB = 4GiB 坏区，远超实际坏扇区规模。
const maxHealedBlocks = 1 << 20

// usedCacheTTL 是 backing 已用字节数（statfs）缓存的有效期。容量判定在每个 Write 上触发，
// 若每次都 statfs 会在高 IOPS 下成为热点（statfs 取 superblock 全局锁）。缓存把 statfs 频率
// 限制到 ~100/秒；10ms 级陈旧对"磁盘满"这种粗粒度模拟可忽略。
const usedCacheTTL = 10 * time.Millisecond

// addOK 返回 a+b，溢出时钳到 MaxInt64（用于 off+length、rOff+rLen 等"非负端点求和"，
// 防回绕成负值导致区间相交判定错乱）。a、b 预期为非负（offset/length/OffLen 均非负）。
func addOK(a, b int64) int64 {
	if b < 0 {
		return a
	}
	if a < 0 || a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// Rule 描述一条故障注入规则。一个 [Injector] 可同时持有任意多条 Rule，
// 同一时刻多种错误可在不同文件/位置/op 上并存。Check 按 Add 顺序遍历，
// 首条命中即返回（多条同时命中同一请求时，Add 顺序决定优先级）。
type Rule struct {
	// Op 限定的操作类型，取上述 Op* 常量；"" 表示任意 op。
	Op string

	// Path 限定的挂载内相对路径子串；"" 表示任意路径。匹配是子串包含，
	// 例如 "blob.bin" 命中 "data/blob.bin"。路径是挂载内相对路径（不含
	// 挂载点前缀），与 FUSE 请求里文件的逻辑位置一致。
	Path string

	// Off 仅对 read/write 生效，是请求的起始 offset（条带对齐边界）。
	// OffLen<=0：不限制 offset，任意 offset 都命中——这是零值默认，适合
	//            “整文件该 op 全报错”这类最常见的需求。
	// OffLen>0 ：仅当 off 落入 [Off, Off+OffLen) 才命中。想精确命中某个
	//            offset X，写 Off:X, OffLen:1；想命中一段条带，写区间。
	Off    int64
	OffLen int64

	// Errno 命中时返回的 errno（不可为 0）。取 syscall.EIO/ENOSPC/EROFS 等。
	Errno syscall.Errno

	// N 仅对普通规则生效：前 N 次命中才注入，之后该规则失效（“注入几次后
	// 自愈”）；0 表示永久生效。对 HealOnWrite 规则无意义（它由 write 治愈）。
	N int

	// HealOnWrite 把这条 read 规则变成“可修复的坏扇区”模型：read 命中（未
	// 修复时）返回 Errno（如 EIO）；write 命中同区域则触发备用块重映射、
	// 标记该规则为已修复——之后的 read 不再注入、读到重映射后的数据。备用
	// 块预算（Injector.spareCount/spareBlockSize）耗尽时 write 也返回 Errno。
	// 仅对 Op==OpRead 的规则有意义；规则持久保留（healed 是运行时状态），
	// 可用 Refresh 重置。参见 [Injector.Refresh]。
	HealOnWrite bool

	// ID 由 Add 自动分配（从 1 递增），供 Delete/控制协议引用；0=未分配。
	ID int
}

// ruleState 是一条规则的运行时状态，与 Rule 配置分离，以便 Refresh 还原。
type ruleState struct {
	r            Rule
	remaining    int    // 当前剩余命中次数；-1 表示永久
	initialRem   int    // 初始 remaining（Refresh 还原用）
	healed       bool   // 整段模式（healedBlocks==nil）下是否已被 write 治愈
	healedBlocks []bool // 按块模式：每块是否治愈；nil = 整段模式（用 healed）
	blockSize    int64  // Add 时快照的 spareBlockSize，固化块划分（避免后续 SetSpareBlocks 改值致 healed 标记错位）
}

// RuleView 是 [Injector.List] 返回的规则视图，含运行时状态（只读快照）。
type RuleView struct {
	Rule
	Healed       bool
	HealedBlocks int // HealOnWrite 按块模式：已治愈块数；整段模式 = healed?1:0；非 HealOnWrite = 0
	TotalBlocks  int // HealOnWrite 按块模式：总块数；整段模式 = 1；非 HealOnWrite = 0
	Remaining    int // -1 表示永久
}

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

// RefreshOptions 控制 [Injector.Refresh] 的复位范围。零值（SkipLatency=false）= 完整复位
// （规则状态 + spare + 性能参数）。设 SkipLatency=true 则保留当前 profile/speed 不动。
type RefreshOptions struct {
	SkipLatency bool // 跳过性能参数（profile/speed）的复位
}

// ResetEntry 描述 Refresh 过程中发生的一次复位/变动，供调用方（如 CLI）日志告知。What 取
// "rule"（含规则 ID）/ "spare" / "latency"；Before/After 为人类可读的变动前后状态。仅记录
// 实际发生变化的条目（未变的规则/字段不产生 entry），避免静默聚合编号。
type ResetEntry struct {
	What   string // "rule" | "spare" | "latency"
	ID     int    // 规则 ID（仅 What=="rule"）
	Before string
	After  string
}

// RefreshResult 汇总一次 Refresh 的全部变动条目。
type RefreshResult struct {
	Entries []ResetEntry
}

// Refresh 把所有规则状态还原到 Add 时的初始态（healed=false、remaining=初始 N）、spare
// 还原到最近一次 set 的初始值；默认同时把 profile/speed 复位到初始值（opts.SkipLatency=true
// 时跳过）。返回所有发生变动的条目列表（规则按 ID、spare、latency），供调用方显式日志，
// 不留静默聚合编号。规则配置不变。用于反复重放同一组故障（治愈→刷新→再次故障）。
func (in *Injector) Refresh(opts RefreshOptions) RefreshResult {
	in.mu.Lock()
	defer in.mu.Unlock()
	var entries []ResetEntry
	for i := range in.rules {
		s := &in.rules[i]
		before := ruleStateText(s.healed, s.remaining, s.healedBlocks)
		s.remaining = s.initialRem
		s.healed = false
		for j := range s.healedBlocks {
			s.healedBlocks[j] = false
		}
		if after := ruleStateText(s.healed, s.remaining, s.healedBlocks); before != after {
			entries = append(entries, ResetEntry{What: "rule", ID: s.r.ID, Before: before, After: after})
		}
	}
	// spare 复位到初始块预算（count + blockSize）。外层按字段比较决定是否复位与记条目：
	// 字段变化即记（哪怕 FormatSpare 文本碰巧相同，如 count=-1 时 blockSize 的变化——复位
	// 确实发生了，应如实记录，不靠文本比较吞掉条目）。
	sBefore := FormatSpare(in.spareCount, in.spareBlockSize)
	if in.spareCount != in.initialSpareCount || in.spareBlockSize != in.initialSpareBlockSize {
		in.spareCount = in.initialSpareCount
		in.spareBlockSize = in.initialSpareBlockSize
		entries = append(entries, ResetEntry{What: "spare", Before: sBefore, After: FormatSpare(in.spareCount, in.spareBlockSize)})
	}
	// 性能参数复位（默认；--keep-latency 跳过）。latency 无消耗路径，current 通常已等于
	// initial，故这里多为 no-op；保留复位以兑现"重置回初始值"语义并提供显式安全开关。
	if !opts.SkipLatency {
		lBefore := latencyStateText(in.profile, in.speed)
		in.profile = in.initialProfile
		in.speed = in.initialSpeed
		if after := latencyStateText(in.profile, in.speed); after != lBefore {
			entries = append(entries, ResetEntry{What: "latency", Before: lBefore, After: after})
		}
	}
	return RefreshResult{Entries: entries}
}

// ruleStateText 把规则运行时状态格式化为紧凑串。按块模式输出 "healed=N/M"；整段模式
// 输出 "healed=%v"。含 remaining。供 Refresh 的 Before/After 比较。
func ruleStateText(healed bool, remaining int, healedBlocks []bool) string {
	if healedBlocks != nil {
		n := 0
		for _, b := range healedBlocks {
			if b {
				n++
			}
		}
		return "healed=" + strconv.Itoa(n) + "/" + strconv.Itoa(len(healedBlocks)) + " rem=" + strconv.Itoa(remaining)
	}
	return "healed=" + strconv.FormatBool(healed) + " rem=" + strconv.Itoa(remaining)
}

// latencyStateText 把 profile/speed 格式化为紧凑串（profile=<name> speed=<v>）。
func latencyStateText(p LatencyProfile, speed float64) string {
	return "profile=" + profileName(p) + " speed=" + trimFloat(speed)
}

// SetSpare 设备用预算为 n 个默认块（blockSize=1，即每治愈消耗 1 块，等价于旧的纯次数语义）；
// 同步更新初始快照，故 Refresh 会还原到该值。需要按真实块大小计费时用 [Injector.SetSpareBlocks]。
func (in *Injector) SetSpare(n int64) { in.SetSpareBlocks(n, 1) }

// SetSpareBlocks 设备用块预算：count 个 blockSize 字节的块（count=-1 无限、>=0 有效；
// count<-1 无定义，钳到 0；blockSize<1 钳到 1）。同步更新初始快照，故 Refresh 会还原到
// 该值。治愈一段坏区时按 ceil(坏区长度/blockSize) 整块消耗（见 [blocksNeeded]）。
func (in *Injector) SetSpareBlocks(count, blockSize int64) {
	if blockSize < 1 {
		blockSize = 1
	}
	// count 合法值：-1（无限）或 >=0；< -1 无定义（与 ParseSpareSpec 的拒绝一致），
	// 钳到 0（无备用，fail-safe）——与 SetSpeed<=0、blockSize<1 的静默钳制风格一致。
	if count < -1 {
		count = 0
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.spareCount = count
	in.spareBlockSize = blockSize
	in.initialSpareCount = count
	in.initialSpareBlockSize = blockSize
}

// Spare 返回剩余备用块数（-1 无限）。块大小另见 [Injector.SpareBlockSize]。
func (in *Injector) Spare() int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.spareCount
}

// SpareBlockSize 返回每块字节数（默认 1；>1 表示按真实块大小整块计费）。
func (in *Injector) SpareBlockSize() int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.spareBlockSize
}

// SetCapacity 设模拟容量上限（字节）；<0 钳到 0（=未启用）。capacity 是 mount 时固化的
// 设备属性，不被消耗、不进 Refresh（改值需重新挂载）。挂载时由 [Mount] 按 backing statfs
// 校验 capacity∈(backing已用, backing总量)；运行时 write 超容量返 ENOSPC、statfs 反映
// 模拟容量（见 spec/capacity.md）。0=不限制（直透 backing 真实容量）。
func (in *Injector) SetCapacity(capacity int64) {
	if capacity < 0 {
		capacity = 0
	}
	in.capacity.Store(capacity)
}

// Capacity 返回模拟容量上限（字节）；0=未启用。atomic 读，可在 Write 热路径无锁调用。
func (in *Injector) Capacity() int64 {
	return in.capacity.Load()
}

// backingStatfsUsed/Total 从 syscall.Statfs_t 推算 backing 已用/总量（字节）。按 f_frsize
// （基本块大小）换算——statfs 的 Blocks/Bfree 以 f_frsize 为单位，而非 f_bsize（"优选传输
// 块"）；二者在 tmpfs/ext4 等通常相等，但部分 fs/配置不同时只有 frsize 正确。负值钳 0
// （理论上 Bfree<=Blocks，钳制为防御）。三处容量判定共用此口径，避免漂移。
func backingStatfsUsed(sf *syscall.Statfs_t) int64 {
	used := (int64(sf.Blocks) - int64(sf.Bfree)) * int64(sf.Frsize)
	if used < 0 {
		used = 0
	}
	return used
}

func backingStatfsTotal(sf *syscall.Statfs_t) int64 {
	total := int64(sf.Blocks) * int64(sf.Frsize)
	if total < 0 {
		total = 0
	}
	return total
}

// capacityUsed 返回 backing 已用字节数的运行估值，供容量判定读。TTL 内随每条放行的 write
// 乐观累加（见 checkWriteCapacity），使一个在 TTL 窗口（~10ms）内完成的突发写仍会逼近 capacity
// 而被拦下——纯 TTL 缓存会让突发写一直读到旧 used 而全部放行、超额。TTL 到期时用真实 statfs
// 重新对齐，修正乐观累加对覆盖写/短写/外部写入的漂移。statfs 失败沿用上次估值（fail-open）。
func (in *Injector) capacityUsed(backing string) int64 {
	now := time.Now().UnixNano()
	if now-in.usedCachedAt.Load() < int64(usedCacheTTL) {
		return in.usedCached.Load()
	}
	var sf syscall.Statfs_t
	if err := syscall.Statfs(backing, &sf); err != nil {
		return in.usedCached.Load() // fail-open：沿用上次估值（或 0）
	}
	used := backingStatfsUsed(&sf)
	in.usedCached.Store(used) // 重新对齐到真实值
	in.usedCachedAt.Store(now)
	return used
}

// checkWriteCapacity 按模拟容量上限判定本次写是否放行：cap<=0（未启用）或 n<=0 →放行；
// 否则取估值（capacityUsed），n > cap-used → ENOSPC；放行则把 n 乐观计入 usedCached，使后续
// 写看到递增的已用、突发写能逼近 capacity 被拦下（TTL 到期由 statfs 重对齐修正）。保守近似：
// 覆盖写也按请求字节计，仅在接近满时触发。原子读 capacity + 估值，热路径无锁、不每 write statfs。
// 设备级判定，与规则注入独立——见 [FaultFile.Write] 在 Check 之前调用，保证规则治愈等副作用
// 不会在随后判定 ENOSPC 时已落盘（heal-then-ENOSPC 原子性）。
func (in *Injector) checkWriteCapacity(backing string, n int64) syscall.Errno {
	cap := in.capacity.Load()
	if cap <= 0 || n <= 0 {
		return 0
	}
	if n > cap-in.capacityUsed(backing) {
		return syscall.ENOSPC
	}
	in.usedCached.Add(n) // 乐观累计：本次 write 将落地 ~n 字节
	return 0
}

// blocksNeeded 计算治愈一段长 offLen 字节的坏区需要消耗多少个 blockSize 字节的备用块：
// blockSize<=1（纯次数模式）或 offLen<=0（未限定区间）时算 1 块；否则向上取整
// （ceil(offLen/blockSize)）。真实硬盘语义：备用块按整块重映射。
func blocksNeeded(offLen, blockSize int64) int64 {
	if blockSize <= 1 || offLen <= 0 {
		return 1
	}
	// 用 div+mod 算 ceil，避免 (offLen+blockSize-1) 在 offLen 极大（接近 MaxInt64）时
	// 溢出回绕成负——负 need 会让 Check 误判放行治愈、并让 spareCount -= need 反向增加。
	// 除法/取模结果 <= 被除数，q <= offLen/blockSize，q++ 后仍远小于 MaxInt64，无溢出。
	q := offLen / blockSize
	if offLen%blockSize != 0 {
		q++
	}
	return q
}

// gridBlocksToSpare 把"治愈了 count 个 gridBs 字节的网格块"换算为 liveBs 字节的备用块数
// （ceil(count*gridBs/liveBs)）。spareCount 以 live blockSize 为单位，而按块治愈的网格用
// Add 时快照的 s.blockSize——二者不同时（Add 后 SetSpareBlocks 改了 blockSize），直接用
// 网格块数扣 spareCount 会单位错配。本函数经字节数中转统一口径；gridBs==liveBs 时退化为 count。
// count 受 maxHealedBlocks 上界、gridBs 有界，乘积不溢出。
func gridBlocksToSpare(count, gridBs, liveBs int64) int64 {
	if count <= 0 {
		return 0
	}
	if liveBs < 1 {
		liveBs = 1
	}
	bytes := count * gridBs
	q := bytes / liveBs
	if bytes%liveBs != 0 {
		q++
	}
	return q
}

// Check 返回命中的 errno（0 表示放行，不注入）。op 是操作类型（取 Op* 常量），
// path 是挂载内相对路径，off/length 是 read/write 的请求起始 offset 与字节数（其他 op
// 传 0；length 仅 HealOnWrite 按块治愈判定用，整段模式与普通规则忽略）。普通规则命中
// 消耗一次 N 配额；HealOnWrite 规则的治愈由 write 触发（见 [Rule.HealOnWrite]）。
func (in *Injector) Check(op, path string, off, length int64) syscall.Errno {
	in.mu.Lock()
	defer in.mu.Unlock()
	for i := range in.rules {
		s := &in.rules[i]
		r := s.r

		// op 匹配。HealOnWrite 坏扇区规则的注入点虽是 read，但 write 也要能
		// 命中它来触发治愈，故放宽到 {read, write}（Add 已把 HealOnWrite 规则的 Op 归一为 OpRead）。
		if r.Op != "" {
			if r.HealOnWrite {
				if op != OpRead && op != OpWrite {
					continue
				}
			} else if r.Op != op {
				continue
			}
		}
		if r.Path != "" && !strings.Contains(path, r.Path) {
			continue
		}
		// off 匹配：read/write 且显式给了区间（OffLen>0）时，按"请求区间 [off,off+length) 与
		// 坏区 [r.Off, r.Off+OffLen) 有交集"判定——而非只看请求起点 off。这样起点在坏区前但
		// 延伸进坏区的覆盖写/读也能命中（HealOnWrite 治愈 / 坏块 EIO），与 coverRange/allHealed
		// 的区间相交语义一致。端点求和用 addOK 防 off+length / r.Off+OffLen 溢出回绕。
		if (op == OpRead || op == OpWrite) && r.OffLen > 0 {
			rEnd := addOK(r.Off, r.OffLen)
			reqEnd := addOK(off, length)
			if off >= rEnd || reqEnd <= r.Off {
				continue // 区间不相交
			}
		}

		// HealOnWrite 坏扇区语义。整段模式（healedBlocks==nil）下 write 命中即整段治愈；
		// 按块模式下 read/write 按"请求覆盖的块"判定——部分覆盖 write 只治愈其实际写入
		// 的块，未覆盖块 read 仍 EIO（真硬盘 UNC 语义，避免 backing 旧数据被误读为已修复）。
		if r.HealOnWrite {
			switch op {
			case OpRead:
				if s.healedBlocks == nil {
					if s.healed {
						continue // 整段已修复：放行
					}
					return r.Errno // 未修复：坏扇区读 → EIO
				}
				// 按块：请求覆盖的块全治愈才放行，否则 EIO（保守：跨好坏块整体 EIO）。
				if allHealed(s.healedBlocks, r.Off, r.OffLen, off, length, s.blockSize) {
					continue
				}
				return r.Errno
			case OpWrite:
				if s.healedBlocks == nil {
					// 整段模式：write 命中即整段治愈，按 blocksNeeded(OffLen, liveBlockSize) 扣 spare。
					// 用 live in.spareBlockSize（非 snapshot s.blockSize）是因为 spareCount 以 live
					// blockSize 为单位计量——Add 后若 SetSpareBlocks 改了 blockSize，charge 仍与预算同口径。
					// blockSize<=1 或 OffLen<=0 时 blocksNeeded 返回 1（纯次数语义）。
					if s.healed {
						continue
					}
					need := blocksNeeded(r.OffLen, in.spareBlockSize)
					if in.spareCount != -1 && in.spareCount < need {
						return r.Errno // 备用块不足：write 也 EIO
					}
					s.healed = true
					if in.spareCount > 0 {
						in.spareCount -= need
					}
					return 0
				}
				// 按块模式：只治愈 write 实际覆盖的块。need = 新治愈的网格块数（每块 s.blockSize 字节）；
				// charge = 把这些块换算到 live blockSize 的备用块单位（gridBlocksToSpare），使扣费与
				// spareCount 口径一致——Add 后 SetSpareBlocks 改 blockSize 也不致单位错配。
				start, end, ok := coverRange(r.Off, r.OffLen, off, length, s.blockSize, len(s.healedBlocks))
				if !ok {
					continue // 无交集，放行
				}
				var need int64
				for j := start; j <= end; j++ {
					if !s.healedBlocks[j] {
						need++
					}
				}
				if need == 0 {
					return 0 // 覆盖块已全治愈，放行
				}
				charge := gridBlocksToSpare(need, s.blockSize, in.spareBlockSize)
				if in.spareCount != -1 && in.spareCount < charge {
					return r.Errno // 备用块不足：不治愈任何块（原子），write 也 EIO
				}
				for j := start; j <= end; j++ {
					s.healedBlocks[j] = true
				}
				if in.spareCount > 0 {
					in.spareCount -= charge // 按换算后的 live 块数整块消耗（-1 无限时不计）
				}
				return 0
			}
		}

		// 普通规则：N 配额。
		if s.remaining == 0 {
			continue
		}
		if s.remaining > 0 {
			s.remaining--
		}
		return r.Errno
	}
	return 0
}

// coverRange 计算请求 [off,off+length) 与坏区 [rOff, rOff+rLen) 的交集，映射到坏区内的
// 块下标 [start, end]（块 i 覆盖 [rOff+i*bs, rOff+(i+1)*bs)）。ok=false 表示无交集
// （length<=0、bs<=1 或区间不相交）。供 HealOnWrite 按块治愈判定。
func coverRange(rOff, rLen, off, length, bs int64, nBlocks int) (start, end int64, ok bool) {
	if bs <= 1 || length <= 0 {
		return 0, 0, false
	}
	lo := off
	if rOff > lo {
		lo = rOff
	}
	// 端点用 addOK 求和，防 off+length / rOff+rLen 在 off、rOff 接近 MaxInt64 时溢出回绕
	// 成负值导致 hi<=lo 误判无交集（坏区 high-offset 写入漏治愈 / 读漏 EIO）。
	hi := addOK(off, length)
	regionEnd := addOK(rOff, rLen)
	if regionEnd < hi {
		hi = regionEnd
	}
	if hi <= lo {
		return 0, 0, false
	}
	start = (lo - rOff) / bs
	end = (hi - 1 - rOff) / bs
	if start < 0 {
		start = 0
	}
	if max := int64(nBlocks) - 1; end > max {
		end = max
	}
	if start > end {
		return 0, 0, false
	}
	return start, end, true
}

// allHealed 报告请求 [off,off+length) 覆盖的坏区块是否全部治愈。无交集（请求未触碰
// 坏区）返回 true（放行）。
func allHealed(blocks []bool, rOff, rLen, off, length, bs int64) bool {
	start, end, ok := coverRange(rOff, rLen, off, length, bs, len(blocks))
	if !ok {
		return true
	}
	for i := start; i <= end; i++ {
		if !blocks[i] {
			return false
		}
	}
	return true
}
