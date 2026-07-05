package faultfs

import (
	"math"
	"strings"
	"syscall"
)

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
