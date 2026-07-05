package faultfs

import (
	"strconv"
)

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

// ---- 备用块预算 ----

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
