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

// TestRefreshSkipLatency:SkipLatency=true 时不产生 latency 条目；默认（false）在 latency
// 未变动时也无条目（no-op）。验证两条分支结构正确、不 panic。
func TestRefreshSkipLatency(t *testing.T) {
	inj := NewInjector()
	inj.SetProfile(ProfileSSD) // current=initial=ssd（setter 同步 initial）
	for _, e := range inj.Refresh(RefreshOptions{SkipLatency: true}).Entries {
		if e.What == "latency" {
			t.Errorf("SkipLatency=true 时不应产生 latency 条目：%+v", e)
		}
	}
	for _, e := range inj.Refresh(RefreshOptions{}).Entries {
		if e.What == "latency" {
			t.Errorf("latency 未变动不应产生条目：%+v", e)
		}
	}
}
