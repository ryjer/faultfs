package faultfs

import (
	"strconv"
	"strings"
	"sync"
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
	// 先于 backing 真满触发。不被消耗、不进 Refresh（改值需重新挂载）。
	capacity int64

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
	if r.HealOnWrite && r.Op == OpRead && bs > 1 && r.OffLen > 0 {
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
	in.mu.Lock()
	defer in.mu.Unlock()
	in.capacity = capacity
}

// Capacity 返回模拟容量上限（字节）；0=未启用。
func (in *Injector) Capacity() int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.capacity
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
		// 命中它来触发治愈，故放宽到 {read, write}。
		if r.Op != "" {
			if r.HealOnWrite && r.Op == OpRead {
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
		// off 匹配仅对 read/write 且显式给了区间（OffLen>0）时启用。
		if (op == OpRead || op == OpWrite) && r.OffLen > 0 {
			if off < r.Off || off >= r.Off+r.OffLen {
				continue
			}
		}

		// HealOnWrite 坏扇区语义。整段模式（healedBlocks==nil）下 write 命中即整段治愈；
		// 按块模式下 read/write 按"请求覆盖的块"判定——部分覆盖 write 只治愈其实际写入
		// 的块，未覆盖块 read 仍 EIO（真硬盘 UNC 语义，避免 backing 旧数据被误读为已修复）。
		if r.HealOnWrite && r.Op == OpRead {
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
					// 整段模式：write 命中即整段治愈，按 blocksNeeded(OffLen,blockSize) 扣 spare。
					// blockSize<=1 或 OffLen<=0 时 blocksNeeded 返回 1（纯次数语义）；按块模式因
					// 块数超 maxHealedBlocks 回退到此者，则按真实块数整体扣（保留"整段消耗"语义）。
					if s.healed {
						continue
					}
					need := blocksNeeded(r.OffLen, s.blockSize)
					if in.spareCount != -1 && in.spareCount < need {
						return r.Errno // 备用块不足：write 也 EIO
					}
					s.healed = true
					if in.spareCount > 0 {
						in.spareCount -= need
					}
					return 0
				}
				// 按块模式：只治愈 write 实际覆盖的块，按新治愈块数扣 spare。
				start, end, ok := coverRange(r.Off, r.OffLen, off, length, s.blockSize, len(s.healedBlocks))
				if !ok {
					continue // 无交集（off 已在坏区内，理论上不触发），放行
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
				if in.spareCount != -1 && in.spareCount < need {
					return r.Errno // 备用块不足：不治愈任何块（原子），write 也 EIO
				}
				for j := start; j <= end; j++ {
					s.healedBlocks[j] = true
				}
				if in.spareCount > 0 {
					in.spareCount -= need // 按新治愈块数整块消耗（-1 无限时不计）
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
	hi := off + length
	if regionEnd := rOff + rLen; regionEnd < hi {
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
