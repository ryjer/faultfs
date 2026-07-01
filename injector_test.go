package faultfs

import (
	"strings"
	"sync"
	"syscall"
	"testing"
)

// TestHealOnWrite_ReadEIOThenWriteHeals:坏扇区首读 EIO；write 该区触发备用扇区
// 重映射（治愈）；再读放行，读到重映射后数据。
func TestHealOnWrite_ReadEIOThenWriteHeals(t *testing.T) {
	inj := NewInjector()
	inj.SetSpare(-1) // 默认 spare=0（不可治愈）；这里需无限预算以验证治愈语义
	inj.Add(Rule{Op: OpRead, Path: "f", Errno: syscall.EIO, HealOnWrite: true})

	if e := inj.Check(OpRead, "f", 0); e != syscall.EIO {
		t.Fatalf("first read = %v, want EIO", e)
	}
	if e := inj.Check(OpWrite, "f", 0); e != 0 {
		t.Fatalf("write after bad sector = %v, want 0 (healed)", e)
	}
	if e := inj.Check(OpRead, "f", 0); e != 0 {
		t.Fatalf("read after heal = %v, want 0", e)
	}
}

// TestHealOnWrite_RefreshResets:治愈后 Refresh 把规则还原到初始（再读又 EIO）。
func TestHealOnWrite_RefreshResets(t *testing.T) {
	inj := NewInjector()
	inj.SetSpare(-1) // 默认 spare=0；这里需无限预算以验证治愈→重置语义
	inj.Add(Rule{Op: OpRead, Path: "f", Errno: syscall.EIO, HealOnWrite: true})
	inj.Check(OpRead, "f", 0)
	inj.Check(OpWrite, "f", 0) // 治愈
	if e := inj.Check(OpRead, "f", 0); e != 0 {
		t.Fatal("should be healed before refresh")
	}
	inj.Refresh(RefreshOptions{})
	if e := inj.Check(OpRead, "f", 0); e != syscall.EIO {
		t.Fatalf("after Refresh read = %v, want EIO again", e)
	}
}

// TestHealOnWrite_SpareExhausted:备用扇区耗尽（spare=0）时，write 也返回 EIO；
// 补充 spare 后再次 write 可治愈。
func TestHealOnWrite_SpareExhausted(t *testing.T) {
	inj := NewInjector()
	inj.SetSpare(0)
	inj.Add(Rule{Op: OpRead, Path: "f", Errno: syscall.EIO, HealOnWrite: true})
	if e := inj.Check(OpWrite, "f", 0); e != syscall.EIO {
		t.Fatalf("write with spare=0 = %v, want EIO", e)
	}
	inj.SetSpare(1)
	if e := inj.Check(OpWrite, "f", 0); e != 0 {
		t.Fatalf("write with spare=1 = %v, want 0 (healed)", e)
	}
}

// TestSpareDecrements:每治愈一个坏扇区消耗一格 spare，耗尽后新坏扇区 write EIO。
func TestSpareDecrements(t *testing.T) {
	inj := NewInjector()
	inj.SetSpare(2)
	inj.Add(Rule{Op: OpRead, Path: "a", Errno: syscall.EIO, HealOnWrite: true})
	inj.Add(Rule{Op: OpRead, Path: "b", Errno: syscall.EIO, HealOnWrite: true})
	inj.Check(OpWrite, "a", 0)
	inj.Check(OpWrite, "b", 0)
	if got := inj.Spare(); got != 0 {
		t.Fatalf("spare = %d, want 0 after 2 heals", got)
	}
	inj.Add(Rule{Op: OpRead, Path: "c", Errno: syscall.EIO, HealOnWrite: true})
	if e := inj.Check(OpWrite, "c", 0); e != syscall.EIO {
		t.Fatalf("write c with spare=0 = %v, want EIO", e)
	}
	// Refresh 还原 spare 到初始 2，c 规则也回到未治愈 → 又能治愈。
	inj.Refresh(RefreshOptions{})
	if got := inj.Spare(); got != 2 {
		t.Fatalf("spare after Refresh = %d, want 2", got)
	}
	if e := inj.Check(OpWrite, "c", 0); e != 0 {
		t.Fatalf("write c after Refresh = %v, want 0 (healed)", e)
	}
}

// TestAddDeleteListClear:Add 返回递增 ID；Delete 按 ID；List 反映状态；Clear 清空。
func TestAddDeleteListClear(t *testing.T) {
	inj := NewInjector()
	id1 := inj.Add(Rule{Op: OpRead, Path: "a", Errno: syscall.EIO})
	id2 := inj.Add(Rule{Op: OpWrite, Path: "b", Errno: syscall.ENOSPC})
	if id1 != 1 || id2 != 2 {
		t.Fatalf("ids = %d,%d want 1,2", id1, id2)
	}
	if len(inj.List()) != 2 {
		t.Fatalf("list len = %d want 2", len(inj.List()))
	}
	if !inj.Delete(id1) {
		t.Fatal("delete id1 returned false")
	}
	if len(inj.List()) != 1 {
		t.Fatalf("after delete len = %d want 1", len(inj.List()))
	}
	if e := inj.Check(OpWrite, "b", 0); e != syscall.ENOSPC {
		t.Fatalf("id2 should still fire, got %v", e)
	}
	if inj.Delete(999) {
		t.Fatal("delete unknown id should return false")
	}
	inj.Clear()
	if len(inj.List()) != 0 {
		t.Fatal("clear left rules")
	}
}

// TestCheckConcurrent:-race 下并发 Add/Check/Delete 不出竞态。
func TestCheckConcurrent(t *testing.T) {
	inj := NewInjector()
	inj.Add(Rule{Op: OpRead, Path: "f", Errno: syscall.EIO, N: 1000})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inj.Check(OpRead, "f", int64(i))
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		inj.Add(Rule{Op: OpWrite, Errno: syscall.ENOSPC})
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		inj.List()
		inj.Delete(1)
	}()
	wg.Wait()
}

// TestSpareBlocks:spare 按「块数量 + 块大小」整块消耗。坏区长度向上取整到 blockSize
// 的倍数即为本次治愈消耗的块数；预算不足时 write 也 EIO。-1 无限不计。
func TestSpareBlocks(t *testing.T) {
	inj := NewInjector()
	inj.SetSpareBlocks(8, 4096) // 8 个 4KiB 块
	if got := inj.Spare(); got != 8 {
		t.Fatalf("Spare() = %d, want 8", got)
	}
	if got := inj.SpareBlockSize(); got != 4096 {
		t.Fatalf("SpareBlockSize() = %d, want 4096", got)
	}
	// 坏区 8192 → ceil(8192/4096)=2 块。
	inj.Add(Rule{Op: OpRead, Path: "big", Off: 0, OffLen: 8192, Errno: syscall.EIO, HealOnWrite: true})
	if e := inj.Check(OpWrite, "big", 0); e != 0 {
		t.Fatalf("heal big = %v, want 0", e)
	}
	if got := inj.Spare(); got != 6 {
		t.Fatalf("after healing 8192B region, spare = %d, want 6 (consumed 2 blocks)", got)
	}
	// 坏区 100 → ceil(100/4096)=1 块（向上取整，最小 1）。
	inj.Add(Rule{Op: OpRead, Path: "tiny", Off: 0, OffLen: 100, Errno: syscall.EIO, HealOnWrite: true})
	if e := inj.Check(OpWrite, "tiny", 0); e != 0 {
		t.Fatalf("heal tiny = %v, want 0", e)
	}
	if got := inj.Spare(); got != 5 {
		t.Fatalf("after healing 100B region, spare = %d, want 5 (consumed 1 block)", got)
	}
	// 无限预算（-1）：治愈不计、不耗尽。
	inj2 := NewInjector()
	inj2.SetSpareBlocks(-1, 4096)
	inj2.Add(Rule{Op: OpRead, Path: "x", Off: 0, OffLen: 1 << 20, Errno: syscall.EIO, HealOnWrite: true})
	if e := inj2.Check(OpWrite, "x", 0); e != 0 {
		t.Fatalf("unlimited spare heal = %v, want 0", e)
	}
	if got := inj2.Spare(); got != -1 {
		t.Fatalf("unlimited spare changed to %d", got)
	}
}

// TestSpareBlocksExhausted:剩余块数 < 本次治愈所需块数时 write 返 EIO、不治愈。
func TestSpareBlocksExhausted(t *testing.T) {
	inj := NewInjector()
	inj.SetSpareBlocks(1, 4096) // 仅 1 块
	// 坏区 8192 需 2 块 > 1 → write EIO、spare 不变。
	inj.Add(Rule{Op: OpRead, Path: "f", Off: 0, OffLen: 8192, Errno: syscall.EIO, HealOnWrite: true})
	if e := inj.Check(OpWrite, "f", 0); e != syscall.EIO {
		t.Fatalf("heal needing 2 blocks with 1 left = %v, want EIO", e)
	}
	if got := inj.Spare(); got != 1 {
		t.Fatalf("exhausted heal should not consume spare, got %d", got)
	}
	// 坏区 4096 需 1 块 == 1 → 可治愈，spare→0。
	inj.Add(Rule{Op: OpRead, Path: "g", Off: 0, OffLen: 4096, Errno: syscall.EIO, HealOnWrite: true})
	if e := inj.Check(OpWrite, "g", 0); e != 0 {
		t.Fatalf("heal needing 1 block with 1 left = %v, want 0", e)
	}
	if got := inj.Spare(); got != 0 {
		t.Fatalf("spare = %d, want 0", got)
	}
}

// TestRefreshReturnsEntries:Refresh 返回所有发生变动的条目（规则按 ID、spare），未变的
// 规则不产生条目（无静默聚合编号）。latency 因不被消耗通常无条目（no-op）。
func TestRefreshReturnsEntries(t *testing.T) {
	inj := NewInjector()
	inj.SetSpare(-1)
	idHeal := inj.Add(Rule{Op: OpRead, Path: "h", Errno: syscall.EIO, HealOnWrite: true})
	idN := inj.Add(Rule{Op: OpRead, Path: "n", Errno: syscall.EIO, N: 3})
	idUntouched := inj.Add(Rule{Op: OpRead, Path: "u", Errno: syscall.ENOSPC}) // 从不命中，状态不变

	// 触发变动：治愈 h、消耗 n 两次。
	inj.Check(OpWrite, "h", 0) // 治愈 h
	inj.Check(OpRead, "n", 0)
	inj.Check(OpRead, "n", 0)

	res := inj.Refresh(RefreshOptions{})
	gotRule := map[int]string{} // rule id -> "Before->After"
	var spareEntry, latencyEntry *ResetEntry
	for i := range res.Entries {
		e := &res.Entries[i]
		switch e.What {
		case "rule":
			gotRule[e.ID] = e.Before + "->" + e.After
		case "spare":
			spareEntry = e
		case "latency":
			latencyEntry = e
		}
	}
	if _, ok := gotRule[idHeal]; !ok {
		t.Errorf("缺少已治愈规则 %d 的 reset 条目", idHeal)
	}
	if _, ok := gotRule[idN]; !ok {
		t.Errorf("缺少消耗过 N 的规则 %d 的 reset 条目", idN)
	}
	if _, ok := gotRule[idUntouched]; ok {
		t.Errorf("未变动的规则 %d 不应产生 reset 条目（得到 %q）", idUntouched, gotRule[idUntouched])
	}
	if s := gotRule[idHeal]; !strings.Contains(s, "healed=true") || !strings.Contains(s, "healed=false") {
		t.Errorf("规则 %d 的 reset 条目应反映 healed true→false，得 %q", idHeal, s)
	}
	if spareEntry != nil {
		t.Errorf("未被消耗的 spare(-1) 不应产生 reset 条目：%+v", spareEntry)
	}
	if latencyEntry != nil {
		t.Errorf("未被改动的 latency 不应产生 reset 条目：%+v", latencyEntry)
	}

	// 让 spare 被消耗：有限 spare + 治愈，再 Refresh 应报 spare 条目（before≠after）。
	inj.SetSpare(2)
	inj.Add(Rule{Op: OpRead, Path: "x", Errno: syscall.EIO, HealOnWrite: true})
	inj.Check(OpWrite, "x", 0) // 消耗 1，spare 2→1
	res2 := inj.Refresh(RefreshOptions{})
	var sawSpare bool
	for _, e := range res2.Entries {
		if e.What == "spare" && strings.Contains(e.Before, "1") && strings.Contains(e.After, "2") {
			sawSpare = true
		}
	}
	if !sawSpare {
		t.Errorf("Refresh 后应有 spare 1->2 的条目，得 %+v", res2.Entries)
	}
}

// TestRefreshSkipLatency:SkipLatency=true 只跳过 latency 复位，不应抑制 rule/spare 复位条目。
// 注：所有 latency setter 同步 initial 快照、且 latency 无消耗路径，故 latency 复位恒为 no-op
// （current==initial），latency 条目在两种 opts 下都不产生——这是设计不变量，非可观测差异；
// 因此本测试用 rule/spare 的正向条目来证明 SkipLatency 分支确被执行且不影响其余复位。
func TestRefreshSkipLatency(t *testing.T) {
	inj := NewInjector()
	inj.SetProfile(ProfileSSD) // current=initial=ssd（setter 同步 initial）
	inj.SetSpare(2)
	inj.Add(Rule{Op: OpRead, Path: "h", Errno: syscall.EIO, HealOnWrite: true})
	inj.Check(OpWrite, "h", 0) // 治愈：消耗 1 块，spare 2→1

	// SkipLatency=true：rule（治愈复位）与 spare（1→2）条目必须照常出现，
	// 证明 SkipLatency 只跳过 latency、不抑制 rule/spare 复位。
	res := inj.Refresh(RefreshOptions{SkipLatency: true})
	var sawRule, sawSpare bool
	for _, e := range res.Entries {
		switch e.What {
		case "rule":
			sawRule = true
		case "spare":
			sawSpare = true
		case "latency":
			t.Errorf("SkipLatency=true 时不应产生 latency 条目：%+v", e)
		}
	}
	if !sawRule {
		t.Errorf("SkipLatency=true 不应抑制 rule 复位条目，得 %+v", res.Entries)
	}
	if !sawSpare {
		t.Errorf("SkipLatency=true 不应抑制 spare 复位条目，得 %+v", res.Entries)
	}

	// 默认（false）：latency 复位为 no-op，不产生 latency 条目（设计不变量）。
	for _, e := range inj.Refresh(RefreshOptions{}).Entries {
		if e.What == "latency" {
			t.Errorf("latency 未变动不应产生条目：%+v", e)
		}
	}
}

// TestDefaultSpareZeroBlocksHeal:NewInjector 默认 spare=0（破坏性变更 -1→0），故 HealOnWrite
// 规则的 write 直接返 EIO（无备用可消耗）。锁定该默认语义，防止默认值回退成 -1（无限）而治愈
// 静默通过、无回归保护。与显式 SetSpare(0) 等价，但更直接地约束"未显式分配即不可治愈"。
func TestDefaultSpareZeroBlocksHeal(t *testing.T) {
	inj := NewInjector() // 不调 SetSpare：默认 spare=0
	if got := inj.Spare(); got != 0 {
		t.Fatalf("NewInjector 默认 Spare = %d, want 0", got)
	}
	inj.Add(Rule{Op: OpRead, Path: "f", Errno: syscall.EIO, HealOnWrite: true})
	if e := inj.Check(OpRead, "f", 0); e != syscall.EIO {
		t.Fatalf("read = %v, want EIO", e)
	}
	if e := inj.Check(OpWrite, "f", 0); e != syscall.EIO {
		t.Fatalf("默认 spare=0 时 write = %v, want EIO（无备用可治愈）", e)
	}
	if e := inj.Check(OpRead, "f", 0); e != syscall.EIO {
		t.Fatalf("未治愈时再读 = %v, want EIO", e)
	}
}

// TestBlocksNeededNoOverflow:blocksNeeded 在 offLen 接近 MaxInt64 时不得溢出回绕成负
// （旧实现 (offLen+blockSize-1)/blockSize 会溢出，导致 Check 误判放行治愈并让 spareCount
// 反向暴增）。回归保护本次的 div+mod 重写。
func TestBlocksNeededNoOverflow(t *testing.T) {
	max := int64(1<<63 - 1)
	cases := []struct {
		offLen, blockSize, want int64
	}{
		{8192, 4096, 2},           // 常规 ceil
		{100, 4096, 1},            // 向上取整到 1
		{4096, 4096, 1},           // 恰好整除
		{max, 4096, max/4096 + 1}, // 极大 offLen：精确 ceil（max%4096=4095≠0），不溢出
		{max, 2, max/2 + 1},       // blockSize=2：max 为奇数 → +1
	}
	for _, c := range cases {
		got := blocksNeeded(c.offLen, c.blockSize)
		if got != c.want || got < 0 {
			t.Errorf("blocksNeeded(offLen=%d, bs=%d) = %d, want %d（非负）", c.offLen, c.blockSize, got, c.want)
		}
	}

	// 端到端：极大坏区 + 仅 2 块 spare → write 应 EIO；spareCount 不被污染。
	inj := NewInjector()
	inj.SetSpareBlocks(2, 4096)
	inj.Add(Rule{Op: OpRead, Path: "big", Off: 0, OffLen: max, Errno: syscall.EIO, HealOnWrite: true})
	if e := inj.Check(OpWrite, "big", 0); e != syscall.EIO {
		t.Fatalf("极大坏区 + 仅 2 块 spare，write = %v, want EIO", e)
	}
	if got := inj.Spare(); got != 2 {
		t.Fatalf("spareCount 被污染 = %d, want 2（未治愈不应消耗）", got)
	}
}

// TestSetSpareBlocksClampsInvalidCount:count<-1 无定义，SetSpareBlocks 钳到 0（fail-safe），
// 与 ParseSpareSpec 拒绝 n<-1 的语义一致。钳后表现为"无备用"（治愈 EIO），不残留负值。
func TestSetSpareBlocksClampsInvalidCount(t *testing.T) {
	inj := NewInjector()
	inj.SetSpareBlocks(-5, 4096) // count<-1 → 钳到 0
	if got := inj.Spare(); got != 0 {
		t.Fatalf("SetSpareBlocks(-5,4096) 后 Spare = %d, want 0（钳到 0）", got)
	}
	inj.Add(Rule{Op: OpRead, Path: "f", Errno: syscall.EIO, HealOnWrite: true})
	if e := inj.Check(OpWrite, "f", 0); e != syscall.EIO {
		t.Fatalf("钳到 0 后 write = %v, want EIO", e)
	}
}
