package faultfs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestInjectOpendirEIO (AC-1, P0): an opendir-rule makes the directory open
// fail with the injected errno; os.ReadDir propagates it.
func TestInjectOpendirEIO(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	if err := os.MkdirAll(filepath.Join(mp, "dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, mp, "dir/a", []byte("x"))

	inj.Add(Rule{Op: OpOpendir, Path: "dir", Errno: syscall.EIO})
	_, err := os.ReadDir(filepath.Join(mp, "dir"))
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("readdir = %v, want EIO", err)
	}
}

// TestInjectOpendirHealsByN (AC-1 自愈): N:1 makes the first opendir fail and
// subsequent ones succeed (rule self-disables after one hit).
func TestInjectOpendirHealsByN(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	if err := os.MkdirAll(filepath.Join(mp, "dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, mp, "dir/a", []byte("x"))

	inj.Add(Rule{Op: OpOpendir, Path: "dir", Errno: syscall.EIO, N: 1})
	if _, err := os.ReadDir(filepath.Join(mp, "dir")); !errors.Is(err, syscall.EIO) {
		t.Fatalf("first readdir = %v, want EIO", err)
	}
	// N 耗尽后规则失效：第二次 opendir 成功、列到 "a"。
	entries, err := os.ReadDir(filepath.Join(mp, "dir"))
	if err != nil {
		t.Fatalf("second readdir = %v, want nil", err)
	}
	if !hasEntry(entries, "a") {
		t.Fatalf("entries = %v, want \"a\" present", names(entries))
	}
}

// TestReaddirPassthrough (AC-2, P0): with no rule, directory read is transparent
// and lists backing contents (matching bare loopback; os.ReadDir filters "."/"..").
func TestReaddirPassthrough(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	if err := os.MkdirAll(filepath.Join(mp, "dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, mp, "dir/a", []byte("x"))
	writeFile(t, mp, "dir/b", []byte("y"))

	entries, err := os.ReadDir(filepath.Join(mp, "dir"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if !hasEntry(entries, "a") || !hasEntry(entries, "b") {
		t.Fatalf("entries = %v, want \"a\" and \"b\" present", names(entries))
	}

	// 多次读取确认透传稳定（排除内核一次性缓存等假象）。
	entries2, err := os.ReadDir(filepath.Join(mp, "dir"))
	if err != nil {
		t.Fatalf("second readdir: %v", err)
	}
	if !hasEntry(entries2, "a") {
		t.Fatalf("second entries = %v, want \"a\" present", names(entries2))
	}
}

// TestInjectReaddirEIO (AC-3, P1): opendir succeeds (no OpOpendir rule) but the
// first getdents (Readdirent) is injected to EIO. Uses N:0 (permanent) so the
// first Readdirent of the round returns EIO with the bridge's first==true gate →
// error stably propagates. Repeats the read to prove no kernel caching masks it.
func TestInjectReaddirEIO(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	if err := os.MkdirAll(filepath.Join(mp, "dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, mp, "dir/a", []byte("x"))

	inj.Add(Rule{Op: OpReaddir, Path: "dir", Errno: syscall.EIO})
	for i := 0; i < 3; i++ {
		_, err := os.ReadDir(filepath.Join(mp, "dir"))
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("readdir[%d] = %v, want EIO", i, err)
		}
	}
}

// TestInjectReaddirHealsByN (AC-3 自愈): N:1 lets the first readdir fail, then
// subsequent reads succeed (entries listed).
func TestInjectReaddirHealsByN(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	if err := os.MkdirAll(filepath.Join(mp, "dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, mp, "dir/a", []byte("x"))

	inj.Add(Rule{Op: OpReaddir, Path: "dir", Errno: syscall.EIO, N: 1})
	if _, err := os.ReadDir(filepath.Join(mp, "dir")); !errors.Is(err, syscall.EIO) {
		t.Fatalf("first readdir = %v, want EIO", err)
	}
	entries, err := os.ReadDir(filepath.Join(mp, "dir"))
	if err != nil {
		t.Fatalf("second readdir = %v, want nil", err)
	}
	if !hasEntry(entries, "a") {
		t.Fatalf("entries = %v, want \"a\" present", names(entries))
	}
}

// TestInjectOpendirNestedPath verifies Path substring matching works for nested
// directories (mirrors FSS #7 /hash/<algo>/<2hex>/<2hex> shape).
func TestInjectOpendirNestedPath(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	sub := filepath.Join(mp, "hash", "md5", "ab", "cd")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, sub, "deadbeef.bin", []byte("x"))

	// 仅命中最深一层目录名 "cd"。
	inj.Add(Rule{Op: OpOpendir, Path: "cd", Errno: syscall.EIO})
	_, err := os.ReadDir(sub)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("readdir nested = %v, want EIO", err)
	}
}

func hasEntry(es []os.DirEntry, name string) bool {
	for _, e := range es {
		if e.Name() == name {
			return true
		}
	}
	return false
}

func names(es []os.DirEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Name()
	}
	return out
}
