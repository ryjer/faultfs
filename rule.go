package faultfs

import (
	"syscall"
)

// 故障注入点：Rule.Op 取这些值之一，或 "" 表示任意 op。
const (
	OpOpen        = "open"
	OpOpendir     = "opendir"
	OpCreate      = "create"
	OpRead        = "read"
	OpReaddir     = "readdir"
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
