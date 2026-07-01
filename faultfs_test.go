package faultfs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// writeFile creates name under mp with data, via the FUSE mount (passthrough,
// no rules set yet at call sites that use it).
func writeFile(t *testing.T, mp, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(mp, name), data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestPassthrough verifies that with no rules, faultfs is a transparent
// loopback: read/write/stat/statfs/xattr/mkdir all behave normally. This is
// the safety baseline — if passthrough is broken, every injection test is
// meaningless.
func TestPassthrough(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)

	p := filepath.Join(mp, "a.bin")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("read = %q, want hello", got)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := syscall.Statfs(mp, &syscall.Statfs_t{}); err != nil {
		t.Fatalf("statfs: %v", err)
	}
	if err := os.Mkdir(filepath.Join(mp, "d"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
}

// TestInjectReadEIO: a read-rule surfaces EIO to the reader.
func TestInjectReadEIO(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	writeFile(t, mp, "a.bin", bytes.Repeat([]byte("x"), 8192))
	inj.Add(Rule{Op: OpRead, Path: "a.bin", Errno: syscall.EIO})

	f, err := os.Open(filepath.Join(mp, "a.bin"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	_, err = f.ReadAt(make([]byte, 4096), 0)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("read = %v, want EIO", err)
	}
}

// TestInjectWriteENOSPC: a write-rule surfaces ENOSPC.
func TestInjectWriteENOSPC(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	writeFile(t, mp, "a.bin", []byte("init"))
	inj.Add(Rule{Op: OpWrite, Errno: syscall.ENOSPC})

	f, err := os.OpenFile(filepath.Join(mp, "a.bin"), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte("x"), 0); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("write = %v, want ENOSPC", err)
	}
}

// TestInjectOpenEIO: an open-rule fails the open itself with EIO.
func TestInjectOpenEIO(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	writeFile(t, mp, "a.bin", []byte("data"))
	inj.Add(Rule{Op: OpOpen, Path: "a.bin", Errno: syscall.EIO})

	if _, err := os.Open(filepath.Join(mp, "a.bin")); !errors.Is(err, syscall.EIO) {
		t.Fatalf("open = %v, want EIO", err)
	}
}

// TestInjectMetadata: getattr/statfs/getxattr/setxattr rules surface their
// respective errnos. xattr needs user.* support on the backing FS.
func TestInjectMetadata(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	p := filepath.Join(mp, "a.bin")
	writeFile(t, mp, "a.bin", []byte("data"))
	// Seed an xattr via passthrough (no rule yet); skip if backing FS lacks
	// user.* xattr support (rare, but tmpfs-old etc.).
	if err := unix.Lsetxattr(p, "user.foo", []byte("v"), 0); err != nil {
		t.Skipf("backing FS cannot set user.* xattr: %v", err)
	}

	// getattr -> ESTALE.
	inj.Add(Rule{Op: OpGetattr, Path: "a.bin", Errno: syscall.ESTALE})
	if _, err := os.Stat(p); !errors.Is(err, syscall.ESTALE) {
		t.Fatalf("stat = %v, want ESTALE", err)
	}
	inj.Reset()

	// statfs -> EIO (on the mount root).
	inj.Add(Rule{Op: OpStatfs, Errno: syscall.EIO})
	if err := syscall.Statfs(mp, &syscall.Statfs_t{}); !errors.Is(err, syscall.EIO) {
		t.Fatalf("statfs = %v, want EIO", err)
	}
	inj.Reset()

	// getxattr -> EACCES.
	inj.Add(Rule{Op: OpGetxattr, Path: "a.bin", Errno: syscall.EACCES})
	if _, err := unix.Lgetxattr(p, "user.foo", make([]byte, 1)); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("getxattr = %v, want EACCES", err)
	}
	inj.Reset()

	// setxattr -> ENOSPC.
	inj.Add(Rule{Op: OpSetxattr, Path: "a.bin", Errno: syscall.ENOSPC})
	if err := unix.Lsetxattr(p, "user.foo", []byte("w"), 0); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("setxattr = %v, want ENOSPC", err)
	}
}

// TestInjectThenRecover: Rule{N:1} injects once then self-heals — the same
// read fails then succeeds on retry.
func TestInjectThenRecover(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	writeFile(t, mp, "a.bin", bytes.Repeat([]byte("x"), 4096))
	inj.Add(Rule{Op: OpRead, Path: "a.bin", Errno: syscall.EIO, N: 1})

	f, err := os.Open(filepath.Join(mp, "a.bin"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if _, err := f.ReadAt(make([]byte, 4096), 0); !errors.Is(err, syscall.EIO) {
		t.Fatalf("first read = %v, want EIO", err)
	}
	// EIO is not cached as success data, so the retry re-enters FUSE and the
	// exhausted rule now passes through.
	if _, err := f.ReadAt(make([]byte, 4096), 0); err != nil {
		t.Fatalf("second read = %v, want nil (self-heal)", err)
	}
}

// TestPathFilter: a Path-scoped rule only hits the named file.
func TestPathFilter(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	writeFile(t, mp, "a.bin", bytes.Repeat([]byte("a"), 4096))
	writeFile(t, mp, "b.bin", bytes.Repeat([]byte("b"), 4096))
	inj.Add(Rule{Op: OpRead, Path: "a.bin", Errno: syscall.EIO})

	// Verify open is fine (rule is read-only); the read below must EIO.
	fa, err := os.Open(filepath.Join(mp, "a.bin"))
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	defer fa.Close()
	if _, err := fa.ReadAt(make([]byte, 4096), 0); !errors.Is(err, syscall.EIO) {
		t.Fatalf("read a = %v, want EIO", err)
	}

	fb, err := os.Open(filepath.Join(mp, "b.bin"))
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	defer fb.Close()
	if _, err := fb.ReadAt(make([]byte, 4096), 0); err != nil {
		t.Fatalf("read b = %v, want nil (path filter excludes b)", err)
	}
}

// TestOffsetFilter: an exact-offset rule (OffLen:1) hits only that offset.
func TestOffsetFilter(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	writeFile(t, mp, "big.bin", bytes.Repeat([]byte("x"), 1<<16))
	inj.Add(Rule{Op: OpRead, Path: "big.bin", Off: 4096, OffLen: 1, Errno: syscall.EIO})

	f, err := os.Open(filepath.Join(mp, "big.bin"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if _, err := f.ReadAt(make([]byte, 4096), 0); err != nil {
		t.Fatalf("read@0 = %v, want nil", err)
	}
	if _, err := f.ReadAt(make([]byte, 4096), 4096); !errors.Is(err, syscall.EIO) {
		t.Fatalf("read@4096 = %v, want EIO", err)
	}
	if _, err := f.ReadAt(make([]byte, 4096), 8192); err != nil {
		t.Fatalf("read@8192 = %v, want nil", err)
	}
}

// TestOffsetRange: a range rule [Off, Off+OffLen) hits any start offset inside.
func TestOffsetRange(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	writeFile(t, mp, "big.bin", bytes.Repeat([]byte("x"), 1<<16))
	inj.Add(Rule{Op: OpRead, Path: "big.bin", Off: 4096, OffLen: 4096, Errno: syscall.EIO})

	f, err := os.Open(filepath.Join(mp, "big.bin"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if _, err := f.ReadAt(make([]byte, 4096), 4096); !errors.Is(err, syscall.EIO) {
		t.Fatalf("read@4096 = %v, want EIO (inside range)", err)
	}
	if _, err := f.ReadAt(make([]byte, 5120-4096), 5120); !errors.Is(err, syscall.EIO) {
		t.Fatalf("read@5120 = %v, want EIO (inside range)", err)
	}
	if _, err := f.ReadAt(make([]byte, 4096), 8192); err != nil {
		t.Fatalf("read@8192 = %v, want nil (outside range)", err)
	}
	if _, err := f.ReadAt(make([]byte, 4096), 0); err != nil {
		t.Fatalf("read@0 = %v, want nil (outside range)", err)
	}
}

// TestMultipleRules: one Injector carries several rules at once — different
// files/ops get different errors simultaneously.
func TestMultipleRules(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	writeFile(t, mp, "a.bin", bytes.Repeat([]byte("a"), 4096))
	writeFile(t, mp, "b.bin", []byte("b"))
	writeFile(t, mp, "c.bin", []byte("c"))
	inj.Add(Rule{Op: OpRead, Path: "a.bin", Errno: syscall.EIO})
	inj.Add(Rule{Op: OpWrite, Path: "b.bin", Errno: syscall.ENOSPC})
	inj.Add(Rule{Op: OpOpen, Path: "c.bin", Errno: syscall.EROFS, N: 1})

	// a.bin read -> EIO.
	fa, err := os.Open(filepath.Join(mp, "a.bin"))
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	defer fa.Close()
	if _, err := fa.ReadAt(make([]byte, 4096), 0); !errors.Is(err, syscall.EIO) {
		t.Fatalf("read a = %v, want EIO", err)
	}

	// b.bin write -> ENOSPC.
	fb, err := os.OpenFile(filepath.Join(mp, "b.bin"), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	defer fb.Close()
	if _, err := fb.WriteAt([]byte("x"), 0); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("write b = %v, want ENOSPC", err)
	}

	// c.bin open -> EROFS (first time only).
	if _, err := os.Open(filepath.Join(mp, "c.bin")); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("open c = %v, want EROFS", err)
	}
}

// TestMultipleMounts: two independent faultfs mounts with separate injectors
// do not interfere — isolation between "disks".
func TestMultipleMounts(t *testing.T) {
	inj1 := NewInjector()
	mp1 := MountT(t, inj1)
	inj2 := NewInjector()
	mp2 := MountT(t, inj2)
	writeFile(t, mp1, "a.bin", bytes.Repeat([]byte("x"), 4096))
	writeFile(t, mp2, "a.bin", bytes.Repeat([]byte("x"), 4096))
	inj1.Add(Rule{Op: OpRead, Errno: syscall.EIO}) // only mp1

	f1, err := os.Open(filepath.Join(mp1, "a.bin"))
	if err != nil {
		t.Fatalf("open mp1: %v", err)
	}
	defer f1.Close()
	if _, err := f1.ReadAt(make([]byte, 4096), 0); !errors.Is(err, syscall.EIO) {
		t.Fatalf("mp1 read = %v, want EIO", err)
	}

	f2, err := os.Open(filepath.Join(mp2, "a.bin"))
	if err != nil {
		t.Fatalf("open mp2: %v", err)
	}
	defer f2.Close()
	if _, err := f2.ReadAt(make([]byte, 4096), 0); err != nil {
		t.Fatalf("mp2 read = %v, want nil (isolated)", err)
	}
}
