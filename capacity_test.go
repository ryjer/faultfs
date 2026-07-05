package faultfs

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

// TestCheckCapacityAtMount:capacity<=0 跳过；capacity<=backing已用 拒绝；capacity>=总量 拒绝。
// capacity=1 必 <= backing 已用（任何真实 FS 上至少有 1 块已用）；MaxInt64 必 >= 总量。
func TestCheckCapacityAtMount(t *testing.T) {
	dir := t.TempDir()
	if err := checkCapacityAtMount(dir, 0); err != nil {
		t.Errorf("capacity=0 应跳过，得 %v", err)
	}
	if err := checkCapacityAtMount(dir, -5); err != nil {
		t.Errorf("capacity<0 应跳过，得 %v", err)
	}
	if err := checkCapacityAtMount(dir, 1); err == nil {
		t.Error("capacity=1 ≤ backing 已用，应拒绝挂载")
	}
	if err := checkCapacityAtMount(dir, math.MaxInt64); err == nil {
		t.Error("capacity=MaxInt64 ≥ backing 总量，应拒绝挂载")
	}
}

// TestReflectCapacity:statfs 输出按模拟容量改写——total=capacity/frsize、avail=total-used。
func TestReflectCapacity(t *testing.T) {
	var out fuse.StatfsOut
	out.Bsize = 4096
	out.Frsize = 4096
	out.Blocks = 1000
	out.Bfree = 900 // used = 100 块 = 400KiB
	reflectCapacity(&out, 500*4096)
	// total = 500*4096/4096 = 500；avail = 500 - 100(used) = 400。
	if out.Blocks != 500 || out.Bfree != 400 || out.Bavail != 400 {
		t.Errorf("reflectCapacity: Blocks=%d Bfree=%d Bavail=%d, want 500/400/400", out.Blocks, out.Bfree, out.Bavail)
	}
}

// TestCheckWriteCapacityMethod:容量判定数学 + 估值 + cap<=0 放行。用 primed 缓存隔离
// backing FS 整体已用（capacityUsed 返回整盘 used，直接 statfs 不可控）。
func TestCheckWriteCapacityMethod(t *testing.T) {
	inj := NewInjector()
	inj.usedCached.Store(0) // 模拟空 backing（used=0）
	inj.usedCachedAt.Store(time.Now().UnixNano())

	inj.SetCapacity(0)
	if e := inj.checkWriteCapacity("", 1<<30); e != 0 {
		t.Errorf("cap=0 应放行，得 %v", e)
	}
	inj.SetCapacity(100)
	if e := inj.checkWriteCapacity("", 200); e != syscall.ENOSPC {
		t.Errorf("n=200 > cap=100-used(0) → want ENOSPC，得 %v", e)
	}
	if e := inj.checkWriteCapacity("", 50); e != 0 {
		t.Errorf("n=50 < cap=100 → want 0，得 %v", e)
	}
	// n<=0 放行（即便 cap 启用）。
	if e := inj.checkWriteCapacity("", 0); e != 0 {
		t.Errorf("n=0 应放行，得 %v", e)
	}
}

// TestCheckWriteCapacityOptimisticIncrement:放行的 write 按 n 乐观累计到 usedCached，使连续写
// 逼近 capacity 被拦下——纯 TTL 缓存会让 TTL 窗口内的连续写读到旧 used 而全部放行、超额。
// 用 primed 缓存（不 statfs）隔离，纯验证乐观累计逻辑。
func TestCheckWriteCapacityOptimisticIncrement(t *testing.T) {
	inj := NewInjector()
	inj.usedCached.Store(0)
	inj.usedCachedAt.Store(time.Now().UnixNano()) // fresh → 不 statfs 重对齐
	inj.SetCapacity(1000)
	for i := 0; i < 10; i++ {
		if e := inj.checkWriteCapacity("", 100); e != 0 {
			t.Fatalf("write #%d (100B) = %v, want 0（未到 cap）", i+1, e)
		}
	}
	if got := inj.usedCached.Load(); got != 1000 {
		t.Fatalf("usedCached = %d after 10×100B writes, want 1000（乐观累计）", got)
	}
	// 第 11 次：cap-used 已为 0 → ENOSPC，无需任何 statfs。
	if e := inj.checkWriteCapacity("", 100); e != syscall.ENOSPC {
		t.Fatalf("11th write = %v, want ENOSPC（乐观累计已填满 cap）", e)
	}
}

// mountWithCapacity 挂一个带模拟容量的 faultfs：capacity = backing 已用 + headroom，
// 保证 mount 校验通过（capacity∈(used,total)）。无 /dev/fuse 或 headroom 不足则 t.Skip。
func mountWithCapacity(t *testing.T, headroom int64) (mp string, inj *Injector) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("/dev/fuse unavailable: %v", err)
	}
	backing := t.TempDir()
	var sf syscall.Statfs_t
	if err := syscall.Statfs(backing, &sf); err != nil {
		t.Skipf("statfs backing: %v", err)
	}
	used := backingStatfsUsed(&sf)
	total := backingStatfsTotal(&sf)
	if total-used < headroom+4096 {
		t.Skipf("backing FS 剩余空间不足 %dB，跳过容量集成测试", headroom)
	}
	inj = NewInjector()
	inj.SetCapacity(used + headroom) // cap-used ≈ headroom，首条大写即超
	// 预填 used 缓存为挂载时刻值，使运行时 checkWriteCapacity 的 cap-used 确定为 headroom，
	// 不受其它测试并发清理 TempDir 致 /tmp 整盘 used 波动的影响（测试在 TTL 内完成，不重 statfs）。
	inj.usedCached.Store(used)
	inj.usedCachedAt.Store(time.Now().UnixNano())
	mp = t.TempDir()
	cleanup, err := Mount(mp, backing, inj)
	if err != nil {
		t.Skipf("faultfs mount unavailable: %v", err)
	}
	t.Cleanup(cleanup)
	return mp, inj
}

// TestCapacityWriteENOSPC:capacity 启用时，超过 cap-used 的 write 返 ENOSPC（设备级）。
// headroom=100B 使首个 4KiB 写块即超（cap-used≈100）→ 确定性触发，不受 10ms TTL 缓存影响
// （首条 write 触发新鲜 statfs；后续若分段每块仍 4KiB > 100）。
func TestCapacityWriteENOSPC(t *testing.T) {
	mp, _ := mountWithCapacity(t, 100)
	f, err := os.OpenFile(filepath.Join(mp, "f"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteAt(make([]byte, 4096), 0); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("write 4KiB over cap(used+100B) = %v, want ENOSPC", err)
	}
}

// TestCapacityFallocateENOSPC:fallocate 现经 FaultFile.Allocate 走容量判定（修旧实现透传绕过）。
func TestCapacityFallocateENOSPC(t *testing.T) {
	mp, _ := mountWithCapacity(t, 100)
	f, err := os.OpenFile(filepath.Join(mp, "f"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()
	err = syscall.Fallocate(int(f.Fd()), 0, 0, 1<<20)
	if errors.Is(err, syscall.ENOSPC) {
		return // 预期：容量门拦截 fallocate
	}
	// 若 fallocate 未被 FUSE 路由（内核/go-fuse 不支持），无法验证——skip。
	if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) {
		t.Skipf("fallocate 未被 FUSE 路由：%v", err)
	}
	t.Fatalf("fallocate 1MiB over cap = %v, want ENOSPC (Allocate now gated)", err)
}

// TestCapacityBurstWriteNoOvershoot:在 TTL 窗口内完成的突发写不会超额——放行的 write 乐观
// 累计 usedCached，使后续写逼近 cap 被拦下。headroom=128KiB，4×64KiB 写应在 ~128KiB 处 ENOSPC，
// 而非旧 TTL 缓存那样全部放行落 256KiB。
func TestCapacityBurstWriteNoOvershoot(t *testing.T) {
	mp, _ := mountWithCapacity(t, 128*1024)
	f, err := os.OpenFile(filepath.Join(mp, "f"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()
	// 4 块 64KiB = 256KiB；headroom 128KiB → 应在第 3 块 ENOSPC，落盘 ~128KiB。
	var landed int64
	for i := 0; i < 4; i++ {
		n, werr := f.WriteAt(make([]byte, 64*1024), int64(i)*64*1024)
		landed += int64(n)
		if errors.Is(werr, syscall.ENOSPC) {
			break // 预期中途 ENOSPC
		}
		if werr != nil {
			t.Fatalf("write chunk %d: %v", i, werr)
		}
	}
	if landed > 128*1024 {
		t.Errorf("突发写落盘 %dB，超过 headroom 128KiB（乐观累计应使其 ≤128KiB）", landed)
	}
	if landed >= 4*64*1024 {
		t.Errorf("突发写全部成功（%dB），未触发 ENOSPC——TTL 缓存超额", landed)
	}
}

// TestHealOnWriteThenENOSPCAtomicity:capacity 不足时 write 返 ENOSPC，且 HealOnWrite 规则
// 不被治愈（容量判定在 Check 之前，治愈副作用未发生）：read 仍 EIO、spare 不变。修旧实现
// 先 Check（治愈、扣 spare）后判容量，致 write 失败却已治愈、后续 read 放行读旧数据。
// 文件无需预写内容——HealOnWrite 规则在 read op 上注入 EIO，与文件实际内容无关。
func TestHealOnWriteThenENOSPCAtomicity(t *testing.T) {
	mp, inj := mountWithCapacity(t, 100)
	p := filepath.Join(mp, "f")
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644) // 空文件，不触发容量判定
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = f.Close() }()

	inj.SetSpare(-1)
	inj.Add(Rule{Op: OpRead, Path: "f", Off: 0, OffLen: 4096, Errno: syscall.EIO, HealOnWrite: true})

	// read → EIO（规则命中，未治愈）。
	if _, err := f.ReadAt(make([]byte, 4096), 0); !errors.Is(err, syscall.EIO) {
		t.Fatalf("read = %v, want EIO", err)
	}
	// write 坏区（4KiB >> cap-used≈100）→ ENOSPC，且不治愈（Check 未执行）。
	if _, err := f.WriteAt(make([]byte, 4096), 0); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("write over capacity = %v, want ENOSPC", err)
	}
	// 再 read → 仍 EIO（未治愈，验证治愈副作用未在 ENOSPC 时落盘）。
	if _, err := f.ReadAt(make([]byte, 4096), 0); !errors.Is(err, syscall.EIO) {
		t.Fatalf("read after failed write = %v, want EIO (heal must not have committed)", err)
	}
}
