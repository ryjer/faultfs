package faultfs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/ryjer/fss/faultfs/control"
)

// TestControlOnline_AddAndInject:端到端——经 control socket 在线 Add 一条 read
// EIO 规则，随后从挂载点读该文件，验证 raif 侧（这里是直接 os 读）经内核收到
// faultfs 注入的 EIO。证明 Mount 启动的 control server + handleControl 闭环可用。
func TestControlOnline_AddAndInject(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	sock := control.SocketPath(mp)

	// 在线加规则（不走 inj.Add，而走 control socket，模拟 CLI/外部进程）。
	resp, err := control.Send(sock, control.Req{
		Cmd: control.CmdAddRule, Op: OpRead, Path: "a.bin", Errno: int(syscall.EIO),
	})
	if err != nil {
		t.Fatalf("control send: %v", err)
	}
	if !resp.OK {
		t.Fatalf("add-rule failed: %s", resp.Err)
	}

	// 写文件（无规则覆盖 write，透传），再读触发注入的 EIO。
	p := filepath.Join(mp, "a.bin")
	if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if _, err := f.ReadAt(make([]byte, 4), 0); !errors.Is(err, syscall.EIO) {
		t.Fatalf("read = %v, want EIO (injected via control socket)", err)
	}

	// 在线 Refresh 前规则仍生效；在线 clear 后读恢复正常。
	if _, err := control.Send(sock, control.Req{Cmd: control.CmdClear}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	// 重新 open 一个 fd（FOPEN_DIRECT_IO 保证不复用缓存）以观察 clear 后行为。
	f2, _ := os.Open(p)
	defer f2.Close()
	if _, err := f2.ReadAt(make([]byte, 4), 0); err != nil {
		t.Fatalf("read after clear = %v, want nil", err)
	}
}

// TestControlOnline_DumpAndStatus:端到端——经 control socket 拉 dump 与 status，
// 验证挂载元信息（pid/backing/socket）、档名、speed、spare、规则与 profile 字段
// 都正确回传。
func TestControlOnline_DumpAndStatus(t *testing.T) {
	inj := NewInjector()
	mp := MountT(t, inj)
	sock := control.SocketPath(mp)

	inj.Add(Rule{Op: OpRead, Path: "a", Errno: syscall.EIO, HealOnWrite: true})
	inj.SetProfile(ProfileSSD)
	inj.SetSpeed(2.0)
	inj.SetSpare(4)

	// dump
	resp, err := control.Send(sock, control.Req{Cmd: control.CmdDump})
	if err != nil {
		t.Fatalf("dump send: %v", err)
	}
	if !resp.OK || resp.Dump == nil {
		t.Fatalf("dump resp = %+v", resp)
	}
	d := resp.Dump
	if d.MountPID <= 0 {
		t.Fatalf("mount pid = %d, want >0", d.MountPID)
	}
	if d.Backing == "" {
		t.Fatal("backing empty")
	}
	if d.Socket != sock {
		t.Fatalf("socket = %q, want %q", d.Socket, sock)
	}
	if d.ProfileName != "ssd" {
		t.Fatalf("profile name = %q, want ssd", d.ProfileName)
	}
	if d.Speed != 2.0 {
		t.Fatalf("speed = %v, want 2", d.Speed)
	}
	if d.Spare != 4 {
		t.Fatalf("spare = %d, want 4", d.Spare)
	}
	if len(d.Rules) != 1 || !d.Rules[0].HealOnWrite {
		t.Fatalf("rules = %+v", d.Rules)
	}
	if len(d.ProfileFields) == 0 || d.ProfileFields["read_rand"] == "" {
		t.Fatalf("profile fields missing: %+v", d.ProfileFields)
	}

	// status（精简）
	st, err := control.Send(sock, control.Req{Cmd: control.CmdStatus})
	if err != nil {
		t.Fatalf("status send: %v", err)
	}
	if st.Profile != "ssd" {
		t.Fatalf("status profile = %q, want ssd", st.Profile)
	}
	if st.Spare != 4 || st.Speed != 2.0 {
		t.Fatalf("status spare/speed = %d/%v, want 4/2", st.Spare, st.Speed)
	}
	if len(st.Rules) != 1 {
		t.Fatalf("status rules len = %d, want 1", len(st.Rules))
	}
}
