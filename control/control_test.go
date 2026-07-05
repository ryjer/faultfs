package control

import (
	"path/filepath"
	"sync"
	"testing"
)

// TestServerClientRoundTrip:server 接受连接 → handler → client 收到 Resp。
// 覆盖 add-rule（返回 ID）、list-rules（返回规则）、delete-rule。
func TestServerClientRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var lastID int
	handler := func(r Req) Resp {
		switch r.Cmd {
		case CmdAddRule:
			mu.Lock()
			lastID++
			id := lastID
			mu.Unlock()
			return Resp{OK: true, ID: id}
		case CmdListRules:
			return Resp{OK: true, Rules: []RuleView{{ID: 1, Op: "read", Errno: 5}}}
		case CmdDeleteRule:
			return Resp{OK: true}
		case CmdSetLatency:
			return Resp{OK: true}
		default:
			return Resp{OK: false, Err: "unknown"}
		}
	}

	sock := filepath.Join(t.TempDir(), "ctl.sock")
	s := NewServer(sock, handler)
	if err := s.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go s.Serve()
	defer func() { _ = s.Close() }()

	// add → ID 1
	resp, err := Send(sock, Req{Cmd: CmdAddRule, Op: "read", Errno: 5})
	if err != nil {
		t.Fatalf("send add: %v", err)
	}
	if !resp.OK || resp.ID != 1 {
		t.Fatalf("add resp = %+v", resp)
	}
	// list
	resp, err = Send(sock, Req{Cmd: CmdListRules})
	if err != nil {
		t.Fatalf("send list: %v", err)
	}
	if len(resp.Rules) != 1 || resp.Rules[0].Op != "read" {
		t.Fatalf("list resp = %+v", resp)
	}
	// delete
	resp, err = Send(sock, Req{Cmd: CmdDeleteRule, ID: 1})
	if err != nil || !resp.OK {
		t.Fatalf("delete resp = %+v err=%v", resp, err)
	}
	// set-latency
	resp, err = Send(sock, Req{Cmd: CmdSetLatency, Profile: "ssd", HasSpeed: true, Speed: 2})
	if err != nil || !resp.OK {
		t.Fatalf("set-latency resp = %+v err=%v", resp, err)
	}
}

// TestServerClientDump:CmdDump 返回全量 DumpView（挂载元信息 + 规则 + profile 字段）。
func TestServerClientDump(t *testing.T) {
	handler := func(r Req) Resp {
		if r.Cmd != CmdDump {
			return Resp{OK: false, Err: "expected dump, got " + string(r.Cmd)}
		}
		return Resp{OK: true, Dump: &DumpView{
			MountPID: 1234, Backing: "/tmp/b", Socket: "/tmp/s.sock", MountTime: "2026-01-02T03:04:05Z",
			ProfileName: "hdd", Speed: 1.5, Spare: 3,
			Rules:         []RuleView{{ID: 1, Op: "read", Errno: 5, HealOnWrite: true}},
			ProfileFields: map[string]string{"read_rand": "8ms"},
		}}
	}
	sock := filepath.Join(t.TempDir(), "d.sock")
	s := NewServer(sock, handler)
	if err := s.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go s.Serve()
	defer func() { _ = s.Close() }()

	resp, err := Send(sock, Req{Cmd: CmdDump})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !resp.OK || resp.Dump == nil {
		t.Fatalf("resp = %+v", resp)
	}
	d := resp.Dump
	if d.MountPID != 1234 || d.Backing != "/tmp/b" || d.ProfileName != "hdd" || d.Speed != 1.5 || d.Spare != 3 {
		t.Fatalf("dump fields = %+v", d)
	}
	if len(d.Rules) != 1 || !d.Rules[0].HealOnWrite {
		t.Fatalf("rules = %+v", d.Rules)
	}
	if d.ProfileFields["read_rand"] != "8ms" {
		t.Fatalf("profile fields = %+v", d.ProfileFields)
	}
}

// TestSocketPathStable:同一 mp 映射到同一路径，不同 mp 不同。
func TestSocketPathStable(t *testing.T) {
	a := SocketPath("/tmp/aa")
	b := SocketPath("/tmp/aa")
	c := SocketPath("/tmp/bb")
	if a != b {
		t.Fatalf("same mp should map to same path: %q != %q", a, b)
	}
	if a == c {
		t.Fatalf("different mp should map to different paths")
	}
}
