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
	r          Rule
	remaining  int  // 当前剩余命中次数；-1 表示永久
	initialRem int  // 初始 remaining（Refresh 还原用）
	healed     bool // HealOnWrite 规则是否已被 write 治愈
}

// RuleView 是 [Injector.List] 返回的规则视图，含运行时状态（只读快照）。
type RuleView struct {
	Rule
	Healed    bool
	Remaining int // -1 表示永久
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
	in.rules = append(in.rules, ruleState{r: r, remaining: rem, initialRem: rem})
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
		out[i] = RuleView{Rule: s.r, Healed: s.healed, Remaining: s.remaining}
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
		before := ruleStateText(s.healed, s.remaining)
		s.remaining = s.initialRem
		s.healed = false
		if after := ruleStateText(s.healed, s.remaining); before != after {
			entries = append(entries, ResetEntry{What: "rule", ID: s.r.ID, Before: before, After: after})
		}
	}
	// spare 复位到初始块预算（count + blockSize）。
	sBefore := FormatSpare(in.spareCount, in.spareBlockSize)
	if in.spareCount != in.initialSpareCount || in.spareBlockSize != in.initialSpareBlockSize {
		in.spareCount = in.initialSpareCount
		in.spareBlockSize = in.initialSpareBlockSize
		if after := FormatSpare(in.spareCount, in.spareBlockSize); after != sBefore {
			entries = append(entries, ResetEntry{What: "spare", Before: sBefore, After: after})
		}
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

// ruleStateText 把规则运行时状态格式化为紧凑串（healed=%v rem=%d）。
func ruleStateText(healed bool, remaining int) string {
	return "healed=" + strconv.FormatBool(healed) + " rem=" + strconv.Itoa(remaining)
}

// latencyStateText 把 profile/speed 格式化为紧凑串（profile=<name> speed=<v>）。
func latencyStateText(p LatencyProfile, speed float64) string {
	return "profile=" + profileName(p) + " speed=" + trimFloat(speed)
}

// SetSpare 设备用预算为 n 个默认块（blockSize=1，即每治愈消耗 1 块，等价于旧的纯次数语义）；
// 同步更新初始快照，故 Refresh 会还原到该值。需要按真实块大小计费时用 [Injector.SetSpareBlocks]。
func (in *Injector) SetSpare(n int64) { in.SetSpareBlocks(n, 1) }

// SetSpareBlocks 设备用块预算：count 个 blockSize 字节的块（count=-1 无限；blockSize<1 钳到 1）。
// 同步更新初始快照，故 Refresh 会还原到该值。治愈一段坏区时按 ceil(坏区长度/blockSize) 整块
// 消耗（见 [blocksNeeded]）。
func (in *Injector) SetSpareBlocks(count, blockSize int64) {
	if blockSize < 1 {
		blockSize = 1
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

// blocksNeeded 计算治愈一段长 offLen 字节的坏区需要消耗多少个 blockSize 字节的备用块：
// blockSize<=1（纯次数模式）或 offLen<=0（未限定区间）时算 1 块；否则向上取整
// （ceil(offLen/blockSize)）。真实硬盘语义：备用块按整块重映射。
func blocksNeeded(offLen, blockSize int64) int64 {
	if blockSize <= 1 || offLen <= 0 {
		return 1
	}
	return (offLen + blockSize - 1) / blockSize
}

// Check 返回命中的 errno（0 表示放行，不注入）。op 是操作类型（取 Op* 常量），
// path 是挂载内相对路径，off 是 read/write 的请求起始 offset（其他 op 传 -1，
// 不会被匹配）。普通规则命中会消耗一次 N 配额；HealOnWrite 规则的治愈由 write
// 触发（见 [Rule.HealOnWrite]）。
func (in *Injector) Check(op, path string, off int64) syscall.Errno {
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

		// HealOnWrite 坏扇区语义。
		if r.HealOnWrite && r.Op == OpRead {
			switch op {
			case OpRead:
				if s.healed {
					continue // 已修复：放行，读到重映射后数据
				}
				return r.Errno // 未修复：坏扇区读 → EIO
			case OpWrite:
				if s.healed {
					continue // 已修复：放行 write
				}
				need := blocksNeeded(r.OffLen, in.spareBlockSize)
				if in.spareCount != -1 && in.spareCount < need {
					return r.Errno // 备用块不足：write 也 EIO
				}
				s.healed = true // 备用块重映射，治愈
				if in.spareCount > 0 {
					in.spareCount -= need // 整块消耗（-1 无限时不计）
				}
				return 0 // 放行 write
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
