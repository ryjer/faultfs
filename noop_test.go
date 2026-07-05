package faultfs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// mountTmp 挂一个 faultfs 到自建的 backing/mp 临时目录，返回 (mp, backing, cleanup)。
// 比 [MountT] 多返回 backing 路径，便于直接核对落盘（不经 FUSE）。无 /dev/fuse 时 t.Skip。
func mountTmp(t *testing.T, inj *Injector) (mp, backing string, cleanup func()) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("/dev/fuse unavailable: %v", err)
	}
	backing = t.TempDir()
	mp = t.TempDir()
	cl, err := Mount(mp, backing, inj)
	if err != nil {
		t.Skipf("faultfs mount unavailable: %v", err)
	}
	return mp, backing, cl
}

// TestNoOpOpsReachFaultfs 验证：faultfs 与 Linux VFS/FUSE 内核都**不**短路"无实际变更"
// 的操作——设置与当前值相同的 chmod / utimes / truncate、或写入与现状逐字节相同的数据，
// 只要系统调用真的发出，内核就把它转发给 faultfs，注入规则照常触发（返回 EIO）。这是对
// "内核短路同值 setattr"误判的回归保护。
//
// 注意：本测试用 [os.Chmod]，它不经"无变化"判断、总是调用 chmod(2)，故能覆盖内核路径。
// coreutils/uutils 的 shell `chmod` 命令在目标值等于当前值时会**在用户态跳过系统调用**，
// 那种情况请求根本不到内核（也就到不了 faultfs）——那是用户态工具的优化，不属于 faultfs
// 行为，本测试不覆盖、也不应覆盖。
func TestNoOpOpsReachFaultfs(t *testing.T) {
	inj := NewInjector()
	mp, backing, cleanup := mountTmp(t, inj)
	defer cleanup()

	p := filepath.Join(mp, "a.bin")
	const orig = "hello world!!!!!"
	if err := os.WriteFile(p, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)

	// 辅助：装一条 op 规则（EIO），执行 no-op，断言命中（返回 EIO）。
	expectReached := func(name, op string, act func() error) {
		t.Helper()
		inj.Clear()
		inj.Add(Rule{Op: op, Path: "a.bin", Errno: syscall.EIO})
		err := act()
		if err == nil {
			t.Fatalf("%s：内核短路了该 no-op，faultfs 未收到（规则未触发）", name)
		}
		if !isErrno(err, syscall.EIO) {
			t.Fatalf("%s：期望 EIO（命中规则），得到 %v", name, err)
		}
	}

	// chmod 到与当前完全相同的 mode。
	expectReached("chmod 同值", OpSetattr, func() error {
		return os.Chmod(p, st.Mode().Perm())
	})
	// utimes 到与当前完全相同的时刻。
	expectReached("utimes 同值", OpSetattr, func() error {
		return os.Chtimes(p, st.ModTime(), st.ModTime())
	})
	// truncate 到与当前完全相同的 size。
	expectReached("truncate 同尺寸", OpSetattr, func() error {
		return os.Truncate(p, st.Size())
	})
	// 写入与现状逐字节相同的数据。
	expectReached("write 同字节", OpWrite, func() error {
		f, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = f.WriteAt([]byte(orig), 0)
		return err
	})

	_ = backing
}

// TestNoOpWritePersistsToBacking 验证另一面：无注入规则时，写入与现状逐字节相同的数据，
// faultfs 仍会真实落盘到 backing（mtime 推进）——既不自行短路、也不被内核吞掉。这保证
// "写同值也要真实执行落盘"的需求成立。
func TestNoOpWritePersistsToBacking(t *testing.T) {
	inj := NewInjector()
	mp, backing, cleanup := mountTmp(t, inj)
	defer cleanup()

	p := filepath.Join(mp, "a.bin")
	bp := filepath.Join(backing, "a.bin")
	const orig = "hello world!!!!!"
	if err := os.WriteFile(p, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(bp)
	mtimeBefore := before.ModTime()

	// 等一个时钟分辨率，确保即便 mtime 精度为秒也能观察到推进。
	time.Sleep(1100 * time.Millisecond)

	// 无规则：写入与现状逐字节相同的数据。
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteAt([]byte(orig), 0)
	_ = f.Close()
	if err != nil {
		t.Fatalf("write 同字节（无规则）：%v", err)
	}

	// 直接看 backing（不经 FUSE）：mtime 应已推进 → 确实落盘。
	after, _ := os.Stat(bp)
	if !after.ModTime().After(mtimeBefore) {
		t.Fatalf("backing mtime 未推进（%v → %v）：写同值未被真实落盘", mtimeBefore, after.ModTime())
	}
}

// isErrno 判断 err 是否为期望的 syscall errno（errors.Is 会解开 *os.PathError 等包装）。
func isErrno(err error, e syscall.Errno) bool {
	return err != nil && errors.Is(err, e)
}
