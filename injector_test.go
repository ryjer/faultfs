package faultfs

import (
	"sync"
	"syscall"
	"testing"
)

// TestHealOnWrite_ReadEIOThenWriteHeals:坏扇区首读 EIO；write 该区触发备用扇区
// 重映射（治愈）；再读放行，读到重映射后数据。
func TestHealOnWrite_ReadEIOThenWriteHeals(t *testing.T) {
	inj := NewInjector()
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
	inj.Add(Rule{Op: OpRead, Path: "f", Errno: syscall.EIO, HealOnWrite: true})
	inj.Check(OpRead, "f", 0)
	inj.Check(OpWrite, "f", 0) // 治愈
	if e := inj.Check(OpRead, "f", 0); e != 0 {
		t.Fatal("should be healed before refresh")
	}
	inj.Refresh()
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
	inj.Refresh()
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
