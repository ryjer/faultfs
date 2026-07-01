package faultfs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/ryjer/fss/faultfs/control"
)

// FaultNode 按指针嵌入 *fs.LoopbackNode（与 FSS 的 fssNode 同一范式），继承
// 全部 loopback 操作，仅覆写被测数据通路会用到的那些：命中注入规则时返回
// [Rule.Errno]，否则调嵌入类型透传，并在透传后按 [Injector] 的延迟模型 sleep
// 模拟设备响应时间。
type FaultNode struct {
	*fs.LoopbackNode
	inj *Injector
}

// 静态断言：确保覆写的方法签名与 go-fuse fs 接口严格一致——签名写错不会
// 编译报错，而是让 go-fuse 静默回落到嵌入类型的实现（注入/延迟失效）。
var (
	_ fs.NodeOpener        = (*FaultNode)(nil)
	_ fs.NodeCreater       = (*FaultNode)(nil)
	_ fs.NodeGetattrer     = (*FaultNode)(nil)
	_ fs.NodeStatfser      = (*FaultNode)(nil)
	_ fs.NodeSetattrer     = (*FaultNode)(nil)
	_ fs.NodeGetxattrer    = (*FaultNode)(nil)
	_ fs.NodeSetxattrer    = (*FaultNode)(nil)
	_ fs.NodeRemovexattrer = (*FaultNode)(nil)
	_ fs.NodeListxattrer   = (*FaultNode)(nil)
	_ fs.NodeLookuper      = (*FaultNode)(nil)
	_ fs.NodeMkdirer       = (*FaultNode)(nil)
	_ fs.NodeRmdirer       = (*FaultNode)(nil)
	_ fs.NodeUnlinker      = (*FaultNode)(nil)
	_ fs.NodeRenamer       = (*FaultNode)(nil)
	_ fs.NodeFsyncer       = (*FaultNode)(nil)
	_ fs.NodeFlusher       = (*FaultNode)(nil)
)

// WrapChild 由 go-fuse 为每个经 Lookup/Create/Mkdir 等创建的子节点调用，把
// 原始 *fs.LoopbackNode 重新包成 *FaultNode 并注入同一个 *Injector。
func (n *FaultNode) WrapChild(ctx context.Context, ops fs.InodeEmbedder) fs.InodeEmbedder {
	switch v := ops.(type) {
	case *FaultNode:
		return v
	case *fs.LoopbackNode:
		return &FaultNode{LoopbackNode: v, inj: n.inj}
	}
	return ops
}

// rel 返回挂载内相对路径（如 "dir/a.txt"，根返回 "."）。
func (n *FaultNode) rel() string {
	return n.Path(n.Root())
}

// ---- 文件句柄 / 属性 ----

// Open 命中 open 规则时返回注入的 errno；否则透传 loopback，并把返回的
// *fs.LoopbackFile 包成 *FaultFile，使其读/写也走注入/延迟。fuseFlags 强制带
// FOPEN_DIRECT_IO：禁用内核 page cache，确保每次 read/write 都进入 faultfs。
func (n *FaultNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	n.inj.DelayOp(OpOpen)
	if e := n.inj.Check(OpOpen, n.rel(), -1); e != 0 {
		return nil, 0, e
	}
	fh, fuseFlags, errno := n.LoopbackNode.Open(ctx, flags)
	fuseFlags |= fuse.FOPEN_DIRECT_IO
	if errno != 0 {
		return nil, 0, errno
	}
	if lf, ok := fh.(*fs.LoopbackFile); ok {
		return &FaultFile{LoopbackFile: lf, inj: n.inj, path: n.rel()}, fuseFlags, 0
	}
	return fh, fuseFlags, 0
}

// Create 同理：命中 create 规则返注入 errno；成功时包 FaultFile。
func (n *FaultNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	rel := filepath.Join(n.rel(), name)
	n.inj.DelayOp(OpCreate)
	if e := n.inj.Check(OpCreate, rel, -1); e != 0 {
		return nil, nil, 0, e
	}
	inode, fh, fuseFlags, errno := n.LoopbackNode.Create(ctx, name, flags, mode, out)
	fuseFlags |= fuse.FOPEN_DIRECT_IO
	if errno != 0 {
		return nil, nil, 0, errno
	}
	if lf, ok := fh.(*fs.LoopbackFile); ok {
		return inode, &FaultFile{LoopbackFile: lf, inj: n.inj, path: rel}, fuseFlags, 0
	}
	return inode, fh, fuseFlags, 0
}

func (n *FaultNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.inj.DelayOp(OpGetattr)
	if e := n.inj.Check(OpGetattr, n.rel(), -1); e != 0 {
		return e
	}
	return n.LoopbackNode.Getattr(ctx, f, out)
}

func (n *FaultNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	n.inj.DelayOp(OpSetattr)
	if e := n.inj.Check(OpSetattr, n.rel(), -1); e != 0 {
		return e
	}
	return n.LoopbackNode.Setattr(ctx, f, in, out)
}

func (n *FaultNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	n.inj.DelayOp(OpStatfs)
	if e := n.inj.Check(OpStatfs, n.rel(), -1); e != 0 {
		return e
	}
	return n.LoopbackNode.Statfs(ctx, out)
}

// ---- xattr ----

func (n *FaultNode) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	n.inj.DelayOp(OpGetxattr)
	if e := n.inj.Check(OpGetxattr, n.rel(), -1); e != 0 {
		return 0, e
	}
	return n.LoopbackNode.Getxattr(ctx, attr, dest)
}

func (n *FaultNode) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	n.inj.DelayOp(OpSetxattr)
	if e := n.inj.Check(OpSetxattr, n.rel(), -1); e != 0 {
		return e
	}
	return n.LoopbackNode.Setxattr(ctx, attr, data, flags)
}

func (n *FaultNode) Removexattr(ctx context.Context, attr string) syscall.Errno {
	n.inj.DelayOp(OpRemovexattr)
	if e := n.inj.Check(OpRemovexattr, n.rel(), -1); e != 0 {
		return e
	}
	return n.LoopbackNode.Removexattr(ctx, attr)
}

func (n *FaultNode) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	n.inj.DelayOp(OpListxattr)
	if e := n.inj.Check(OpListxattr, n.rel(), -1); e != 0 {
		return 0, e
	}
	return n.LoopbackNode.Listxattr(ctx, dest)
}

// ---- 目录 / 树操作 ----

func (n *FaultNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.inj.DelayOp(OpLookup)
	if e := n.inj.Check(OpLookup, filepath.Join(n.rel(), name), -1); e != 0 {
		return nil, e
	}
	return n.LoopbackNode.Lookup(ctx, name, out)
}

func (n *FaultNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.inj.DelayOp(OpMkdir)
	if e := n.inj.Check(OpMkdir, filepath.Join(n.rel(), name), -1); e != 0 {
		return nil, e
	}
	return n.LoopbackNode.Mkdir(ctx, name, mode, out)
}

func (n *FaultNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	n.inj.DelayOp(OpRmdir)
	if e := n.inj.Check(OpRmdir, filepath.Join(n.rel(), name), -1); e != 0 {
		return e
	}
	return n.LoopbackNode.Rmdir(ctx, name)
}

func (n *FaultNode) Unlink(ctx context.Context, name string) syscall.Errno {
	n.inj.DelayOp(OpUnlink)
	if e := n.inj.Check(OpUnlink, filepath.Join(n.rel(), name), -1); e != 0 {
		return e
	}
	return n.LoopbackNode.Unlink(ctx, name)
}

func (n *FaultNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	n.inj.DelayOp(OpRename)
	if e := n.inj.Check(OpRename, filepath.Join(n.rel(), name), -1); e != 0 {
		return e
	}
	return n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
}
func (n *FaultNode) Fsync(ctx context.Context, f fs.FileHandle, flags uint32) syscall.Errno {
	n.inj.DelayOp(OpFsync)
	if e := n.inj.Check(OpFsync, n.rel(), -1); e != 0 {
		return e
	}
	if fh, ok := f.(*FaultFile); ok {
		return fh.LoopbackFile.Fsync(ctx, flags)
	}
	return 0
}

func (n *FaultNode) Flush(ctx context.Context, f fs.FileHandle) syscall.Errno {
	n.inj.DelayOp(OpFlush)
	if e := n.inj.Check(OpFlush, n.rel(), -1); e != 0 {
		return e
	}
	if fh, ok := f.(*FaultFile); ok {
		return fh.LoopbackFile.Flush(ctx)
	}
	return 0
}

// ---- Mount ----

// newRoot 用 backing 目录构建一个 fault loopback 根节点。
func newRoot(backing string, inj *Injector) (fs.InodeEmbedder, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(backing, &st); err != nil {
		return nil, err
	}
	root := &fs.LoopbackRoot{
		Path: backing,
		Dev:  uint64(st.Dev),
	}
	lb := &fs.LoopbackNode{RootData: root}
	r := &FaultNode{LoopbackNode: lb, inj: inj}
	root.RootNode = r
	return r, nil
}

// Mount 在 mp 挂一个 fault loopback：backing 的内容透传到 mp，命中 inj 规则的
// 操作返回注入的 errno，透传后按延迟模型 sleep。同时启动一个 control server
// （unix socket，路径由 mp 决定），供 CLI 在线增删规则、调延迟。返回的 cleanup
// 关闭 control、卸载并处理 server 退出。
func Mount(mp, backing string, inj *Injector) (func(), error) {
	_, cleanup, err := mountInternal(mp, backing, inj)
	return cleanup, err
}

// Run 挂载 + 启 control 并阻塞，直到收到 SIGINT/SIGTERM 或 FUSE server 退出
// （如 `faultfs unmount` 用 fusermount3 -u 触发的卸载），随后清理退出。供 CLI
// 的 mount 子命令（前台守护）使用。
func Run(mp, backing string, inj *Injector) error {
	server, cleanup, err := mountInternal(mp, backing, inj)
	if err != nil {
		return err
	}
	defer cleanup()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		server.Wait()
		close(done)
	}()
	select {
	case <-sigCh:
	case <-done:
	}
	return nil
}

// mountInternal 挂载 + 启 control，返回 FUSE server 与 cleanup。Mount（库 API）
// 与 Run（CLI 守护）共用它。control 监听失败不阻断挂载（cleanup 里 ctl.Close
// 对未 Listen 的 server 是 no-op）。
func mountInternal(mp, backing string, inj *Injector) (*fuse.Server, func(), error) {
	root, err := newRoot(backing, inj)
	if err != nil {
		return nil, nil, err
	}
	server, err := fs.Mount(mp, root, &fs.Options{})
	if err != nil {
		return nil, nil, err
	}
	sock := control.SocketPath(mp)
	meta := mountMeta{
		pid:       os.Getpid(),
		backing:   backing,
		socket:    sock,
		mountTime: time.Now().Format(time.RFC3339),
	}
	ctl := control.NewServer(sock, func(req control.Req) control.Resp { return handleControl(inj, meta, req) })
	if lerr := ctl.Listen(); lerr == nil {
		go ctl.Serve()
	} else {
		_, _ = fmt.Fprintf(os.Stderr, "faultfs: control socket unavailable: %v (rules can only be managed via code API)\n", lerr)
	}
	cleanup := func() {
		_ = ctl.Close()
		// server.Unmount 内部已等待 event loops 退出（loops.Wait）。仅当它
		// 重试 5 次仍失败（WSL2 等环境下偶发的 EBUSY）时，用 lazy umount
		// (-z) 强制断开连接、让 server loop 自行退出。
		if uerr := server.Unmount(); uerr != nil {
			_ = exec.Command("fusermount3", "-u", "-z", mp).Run()
		}
	}
	return server, cleanup, nil
}

// MountT 是测试友好的挂载入口：自动创建 backing 临时目录与挂载点、把卸载注册
// 到 t.Cleanup，并在无 /dev/fuse 或挂载失败时 t.Skip。返回挂载点路径。
func MountT(t *testing.T, inj *Injector) string {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("/dev/fuse unavailable: %v", err)
	}
	backing := t.TempDir()
	mp := t.TempDir()
	cleanup, err := Mount(mp, backing, inj)
	if err != nil {
		t.Skipf("faultfs mount unavailable: %v", err)
	}
	t.Cleanup(cleanup)
	return mp
}

// handleControl 是 control server 的请求处理适配器：把 control.Req 翻译成对
// *Injector 的调用并返回 control.Resp。control 包不 import faultfs，故由本包
// 提供此闭包，避免循环依赖。
func handleControl(inj *Injector, meta mountMeta, req control.Req) control.Resp {
	switch req.Cmd {
	case control.CmdAddRule:
		r := Rule{
			Op:          req.Op,
			Path:        req.Path,
			Off:         req.Off,
			OffLen:      req.OffLen,
			Errno:       syscall.Errno(req.Errno),
			N:           req.N,
			HealOnWrite: req.HealOnWrite,
		}
		return control.Resp{OK: true, ID: inj.Add(r)}
	case control.CmdDeleteRule:
		return control.Resp{OK: inj.Delete(req.ID)}
	case control.CmdClear:
		inj.Clear()
		return control.Resp{OK: true}
	case control.CmdListRules:
		return control.Resp{OK: true, Rules: toControlViews(inj.List())}
	case control.CmdRefreshRules:
		inj.Refresh()
		return control.Resp{OK: true}
	case control.CmdSetLatency:
		if req.Profile == "" && !req.HasSpeed {
			return control.Resp{OK: false, Err: "no profile or speed specified; use --profile and/or --speed"}
		}
		if req.Profile != "" {
			p, ok := ProfileByName(req.Profile)
			if !ok {
				return control.Resp{OK: false, Err: "unknown profile: " + req.Profile}
			}
			inj.SetProfile(p)
		}
		if req.HasSpeed {
			inj.SetSpeed(req.Speed)
		}
		return control.Resp{OK: true}
	case control.CmdSetSpare:
		if !req.HasSpare {
			return control.Resp{OK: false, Err: "no spare value specified"}
		}
		inj.SetSpare(req.Spare)
		return control.Resp{OK: true}
	case control.CmdStatus:
		return control.Resp{OK: true, Rules: toControlViews(inj.List()), Profile: profileName(inj.Profile()), Spare: inj.Spare(), Speed: inj.Speed()}
	case control.CmdDump:
		return control.Resp{OK: true, Dump: buildDump(inj, meta)}
	}
	return control.Resp{OK: false, Err: "unknown cmd: " + string(req.Cmd)}
}

// mountMeta 记录一次挂载的元信息，供 dump/status 回传给 CLI。
type mountMeta struct {
	pid       int
	backing   string
	socket    string
	mountTime string // RFC3339
}

// buildDump 构造一份全量快照：规则 + 挂载元信息 + 完整延迟 profile。
func buildDump(inj *Injector, meta mountMeta) *control.DumpView {
	p := inj.Profile()
	return &control.DumpView{
		Rules:         toControlViews(inj.List()),
		MountPID:      meta.pid,
		Backing:       meta.backing,
		Socket:        meta.socket,
		MountTime:     meta.mountTime,
		ProfileName:   profileName(p),
		Speed:         inj.Speed(),
		Spare:         inj.Spare(),
		ProfileFields: profileFields(p),
	}
}

// toControlViews 把 faultfs.RuleView 列表转成 control 协议的 RuleView。
func toControlViews(vs []RuleView) []control.RuleView {
	out := make([]control.RuleView, len(vs))
	for i, v := range vs {
		out[i] = control.RuleView{
			ID:          v.ID,
			Op:          v.Op,
			Path:        v.Path,
			Off:         v.Off,
			OffLen:      v.OffLen,
			Errno:       int(v.Errno),
			N:           v.N,
			HealOnWrite: v.HealOnWrite,
			Healed:      v.Healed,
			Remaining:   v.Remaining,
		}
	}
	return out
}
