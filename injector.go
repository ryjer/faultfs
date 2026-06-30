package faultfs

import (
	"strings"
	"sync"
	"syscall"
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
	OpGetxattr    = "getxattr"
	OpSetxattr    = "setxattr"
	OpRemovexattr = "removexattr"
	OpListxattr   = "listxattr"
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
	// 修复时）返回 Errno（如 EIO）；write 命中同区域则触发备用扇区重映射、
	// 标记该规则为已修复——之后的 read 不再注入、读到重映射后的数据。备用
	// 预算（Injector.spare）耗尽时 write 也返回 Errno。仅对 Op==OpRead 的规则
	// 有意义；规则持久保留（healed 是运行时状态），可用 Refresh 重置。参见
	// [Injector.Refresh]。
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
	mu           sync.Mutex
	rules        []ruleState
	nextID       int
	spare        int64 // 当前备用扇区预算；-1 无限
	initialSpare int64 // 初始 spare（Refresh 还原用）
	profile      LatencyProfile
	speed        float64 // 倍率；<=0 视为 1
}

// NewInjector 建一个空规则集：spare 无限、speed 1.0、ProfileNone。
func NewInjector() *Injector { return &Injector{spare: -1, initialSpare: -1, speed: 1} }

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
	in.rules = nil
	in.mu.Unlock()
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

// Refresh 把所有规则状态还原到 Add 时的初始态：healed=false、remaining=
// 初始 N、spare=初始值。规则配置不变。用于反复重放同一组故障。
func (in *Injector) Refresh() {
	in.mu.Lock()
	defer in.mu.Unlock()
	for i := range in.rules {
		in.rules[i].remaining = in.rules[i].initialRem
		in.rules[i].healed = false
	}
	in.spare = in.initialSpare
}

// SetSpare 设备用扇区预算（-1 无限）；同步更新初始快照，故 Refresh 会还原到该值。
func (in *Injector) SetSpare(n int64) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.spare = n
	in.initialSpare = n
}

// Spare 返回当前剩余备用扇区预算（-1 无限）。
func (in *Injector) Spare() int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.spare
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
				if in.spare == 0 {
					return r.Errno // 备用耗尽：write 也 EIO
				}
				s.healed = true // 备用扇区重映射，治愈
				if in.spare > 0 {
					in.spare--
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
