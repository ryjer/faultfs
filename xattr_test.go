package faultfs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestXattrInjection_FileAndDir:对文件与目录的 xattr 操作注入错误（覆盖新扩充的 errno
// ENODATA/EOPNOTSUPP/E2BIG/ENOSPC），验证 xattr 注入对文件与目录都生效。xattr op
// （getxattr/setxattr/removexattr/listxattr）在 FaultNode（node 级）覆写，文件与目录
// 共用同一套注入路径——本测试端到端确认目录 xattr 也能命中。
func TestXattrInjection_FileAndDir(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)

	// 建文件 f 与目录 d。
	pf := filepath.Join(mp, "f")
	if err := os.WriteFile(pf, []byte("x"), 0o644); err != nil {
		t.Fatalf("write f: %v", err)
	}
	pd := filepath.Join(mp, "d")
	if err := os.Mkdir(pd, 0o755); err != nil {
		t.Fatalf("mkdir d: %v", err)
	}

	// 文件 setxattr 注入 E2BIG（属性名/值过大）。
	inj.Add(Rule{Op: OpSetxattr, Path: "f", Errno: syscall.E2BIG})
	if err := unix.Setxattr(pf, "user.k", []byte("v"), 0); !errors.Is(err, syscall.E2BIG) {
		t.Fatalf("setxattr f = %v, want E2BIG (injected)", err)
	}

	// 目录 getxattr 注入 ENODATA（属性不存在）。
	inj.Add(Rule{Op: OpGetxattr, Path: "d", Errno: syscall.ENODATA})
	if _, err := unix.Getxattr(pd, "user.k", make([]byte, 64)); !errors.Is(err, syscall.ENODATA) {
		t.Fatalf("getxattr d = %v, want ENODATA (injected)", err)
	}

	// 文件 removexattr 注入 EOPNOTSUPP（不支持）。
	inj.Add(Rule{Op: OpRemovexattr, Path: "f", Errno: syscall.EOPNOTSUPP})
	if err := unix.Removexattr(pf, "user.k"); !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Fatalf("removexattr f = %v, want EOPNOTSUPP (injected)", err)
	}

	// 目录 setxattr 注入 ENOSPC（无空间）。
	inj.Add(Rule{Op: OpSetxattr, Path: "d", Errno: syscall.ENOSPC})
	if err := unix.Setxattr(pd, "user.k", []byte("v"), 0); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("setxattr d = %v, want ENOSPC (injected)", err)
	}

	// clear 后注入应消失（setxattr 不再返回注入的 E2BIG；可能成功或返回 backing 自身的
	// 错误如 EOPNOTSUPP——只要不是 E2BIG 即说明注入确已移除）。
	inj.Clear()
	if err := unix.Setxattr(pf, "user.ok", []byte("v"), 0); errors.Is(err, syscall.E2BIG) {
		t.Fatalf("setxattr f after clear = %v；注入应已随 clear 移除", err)
	}
}
