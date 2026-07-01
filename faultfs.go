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
	inj     *Injector
	backing string // backing 目录绝对路径，传给 FaultFile 供容量判定、Statfs 反映容量
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
		return &FaultNode{LoopbackNode: v, inj: n.inj, backing: n.backing}
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
	if e := n.inj.Check(OpOpen, n.rel(), -1, 0); e != 0 {
		return nil, 0, e
	}
	fh, fuseFlags, errno := n.LoopbackNode.Open(ctx, flags)
	fuseFlags |= fuse.FOPEN_DIRECT_IO
	if errno != 0 {
		return nil, 0, errno
	}
	if lf, ok := fh.(*fs.LoopbackFile); ok {
		return &FaultFile{LoopbackFile: lf, inj: n.inj, path: n.rel(), backing: n.backing}, fuseFlags, 0
	}
	return fh, fuseFlags, 0
}

// Create 同理：命中 create 规则返注入 errno；成功时包 FaultFile。
func (n *FaultNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	rel := filepath.Join(n.rel(), name)
	n.inj.DelayOp(OpCreate)
	if e := n.inj.Check(OpCreate, rel, -1, 0); e != 0 {
		return nil, nil, 0, e
	}
	inode, fh, fuseFlags, errno := n.LoopbackNode.Create(ctx, name, flags, mode, out)
	fuseFlags |= fuse.FOPEN_DIRECT_IO
	if errno != 0 {
		return nil, nil, 0, errno
	}
	if lf, ok := fh.(*fs.LoopbackFile); ok {
		return inode, &FaultFile{LoopbackFile: lf, inj: n.inj, path: rel, backing: n.backing}, fuseFlags, 0
	}
	return inode, fh, fuseFlags, 0
}

func (n *FaultNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.inj.DelayOp(OpGetattr)
	if e := n.inj.Check(OpGetattr, n.rel(), -1, 0); e != 0 {
		return e
	}
	return n.LoopbackNode.Getattr(ctx, f, out)
}

func (n *FaultNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	n.inj.DelayOp(OpSetattr)
	if e := n.inj.Check(OpSetattr, n.rel(), -1, 0); e != 0 {
		return e
	}
	return n.LoopbackNode.Setattr(ctx, f, in, out)
}

func (n *FaultNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	n.inj.DelayOp(OpStatfs)
	if e := n.inj.Check(OpStatfs, n.rel(), -1, 0); e != 0 {
		return e
	}
	errno := n.LoopbackNode.Statfs(ctx, out)
	// 模拟容量反映：若设了 capacity，把 total 改为 capacity、avail 改为 capacity-backing真实used，
	// 让 df/上层 statfs 看到模拟容量。used 取 backing 真实（Statfs 已填入 out）。
	if capacity := n.inj.Capacity(); capacity > 0 && errno == 0 {
		reflectCapacity(out, capacity)
	}
	return errno
}

// ---- xattr ----

func (n *FaultNode) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	n.inj.DelayOp(OpGetxattr)
	if e := n.inj.Check(OpGetxattr, n.rel(), -1, 0); e != 0 {
		return 0, e
	}
	return n.LoopbackNode.Getxattr(ctx, attr, dest)
}

func (n *FaultNode) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	n.inj.DelayOp(OpSetxattr)
	if e := n.inj.Check(OpSetxattr, n.rel(), -1, 0); e != 0 {
		return e
	}
	return n.LoopbackNode.Setxattr(ctx, attr, data, flags)
}

func (n *FaultNode) Removexattr(ctx context.Context, attr string) syscall.Errno {
	n.inj.DelayOp(OpRemovexattr)
	if e := n.inj.Check(OpRemovexattr, n.rel(), -1, 0); e != 0 {
		return e
	}
	return n.LoopbackNode.Removexattr(ctx, attr)
}

func (n *FaultNode) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	n.inj.DelayOp(OpListxattr)
	if e := n.inj.Check(OpListxattr, n.rel(), -1, 0); e != 0 {
		return 0, e
	}
	return n.LoopbackNode.Listxattr(ctx, dest)
}

// ---- 目录 / 树操作 ----

func (n *FaultNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.inj.DelayOp(OpLookup)
	if e := n.inj.Check(OpLookup, filepath.Join(n.rel(), name), -1, 0); e != 0 {
		return nil, e
	}
	return n.LoopbackNode.Lookup(ctx, name, out)
}

func (n *FaultNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	n.inj.DelayOp(OpMkdir)
	if e := n.inj.Check(OpMkdir, filepath.Join(n.rel(), name), -1, 0); e != 0 {
		return nil, e
	}
	return n.LoopbackNode.Mkdir(ctx, name, mode, out)
}

func (n *FaultNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	n.inj.DelayOp(OpRmdir)
	if e := n.inj.Check(OpRmdir, filepath.Join(n.rel(), name), -1, 0); e != 0 {
		return e
	}
	return n.LoopbackNode.Rmdir(ctx, name)
}

func (n *FaultNode) Unlink(ctx context.Context, name string) syscall.Errno {
	n.inj.DelayOp(OpUnlink)
	if e := n.inj.Check(OpUnlink, filepath.Join(n.rel(), name), -1, 0); e != 0 {
		return e
	}
	return n.LoopbackNode.Unlink(ctx, name)
}

func (n *FaultNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	n.inj.DelayOp(OpRename)
	if e := n.inj.Check(OpRename, filepath.Join(n.rel(), name), -1, 0); e != 0 {
		return e
	}
	return n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
}
func (n *FaultNode) Fsync(ctx context.Context, f fs.FileHandle, flags uint32) syscall.Errno {
	n.inj.DelayOp(OpFsync)
	if e := n.inj.Check(OpFsync, n.rel(), -1, 0); e != 0 {
		return e
	}
	if fh, ok := f.(*FaultFile); ok {
		return fh.LoopbackFile.Fsync(ctx, flags)
	}
	return 0
}

func (n *FaultNode) Flush(ctx context.Context, f fs.FileHandle) syscall.Errno {
	n.inj.DelayOp(OpFlush)
	if e := n.inj.Check(OpFlush, n.rel(), -1, 0); e != 0 {
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
	r := &FaultNode{LoopbackNode: lb, inj: inj, backing: backing}
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
	if err := checkCapacityAtMount(backing, inj.Capacity()); err != nil {
		return nil, nil, err
	}
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

// checkCapacityAtMount 在挂载时校验模拟容量 capacity 落在合法区间：必须 > backing 已用、
// 且 < backing 总量（等价 capacity-已用 < 剩余）。前者避免挂载即满，后者保证 faultfs 模拟的
// "满"先于 backing 真满触发——这是用 capacity 模拟 ENOSPC 的前提。capacity<=0（未启用）跳过。
func checkCapacityAtMount(backing string, capacity int64) error {
	if capacity <= 0 {
		return nil
	}
	var sf syscall.Statfs_t
	if err := syscall.Statfs(backing, &sf); err != nil {
		return fmt.Errorf("capacity 校验：statfs %s: %w", backing, err)
	}
	// 用 f_frsize（基本块）换算，与 backingStatfsUsed/Total 同口径；含负值钳制（理论上
	// Bfree<=Blocks，防御性）。mount 时 statfs 失败 fail-closed（拒绝挂载），与运行时
	// checkWriteCapacity 的 fail-open（沿缓存放行）互补。
	used := backingStatfsUsed(&sf)
	total := backingStatfsTotal(&sf)
	if capacity <= used {
		return fmt.Errorf("capacity %s ≤ backing 已用 %s；挂载即满，无法模拟（capacity 须 > backing 已用）", FormatSize(capacity), FormatSize(used))
	}
	if capacity >= total {
		return fmt.Errorf("capacity %s ≥ backing 总量 %s；无法保证 faultfs 模拟的满先于 backing 真满（capacity 须 < backing 总量）", FormatSize(capacity), FormatSize(total))
	}
	return nil
}

// reflectCapacity 把 statfs 输出改写为基于模拟容量：total = capacity、avail = capacity -
// backing真实used（used 从 out 已填的 backing 真实值推算）。让 df/上层 statfs 看到模拟容量，
// 同时保留 backing 的块大小与真实已用量。供 [FaultNode.Statfs] 在设了 capacity 时调用。
func reflectCapacity(out *fuse.StatfsOut, capacity int64) {
	// 用 f_frsize 换算（Blocks/Bfree 以 frsize 为单位）；go-fuse 在 Linux 从 backing
	// statfs 忠实拷贝 Frsize。tmpfs/ext4 等通常 Bsize==Frsize，但二者不同的 fs 上只有 frsize 正确。
	frsize := int64(out.Frsize)
	if frsize <= 0 {
		frsize = 1
	}
	usedBlocks := int64(out.Blocks) - int64(out.Bfree)
	if usedBlocks < 0 {
		usedBlocks = 0
	}
	total := capacity / frsize
	avail := total - usedBlocks
	if avail < 0 {
		avail = 0
	}
	out.Blocks = uint64(total)
	out.Bfree = uint64(avail)
	out.Bavail = uint64(avail)
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
		if !inj.Delete(req.ID) {
			return control.Resp{OK: false, Err: fmt.Sprintf("rule id %d does not exist", req.ID)}
		}
		return control.Resp{OK: true}
	case control.CmdClear:
		inj.Clear()
		return control.Resp{OK: true}
	case control.CmdListRules:
		return control.Resp{OK: true, Rules: toControlViews(inj.List())}
	case control.CmdRefreshRules:
		res := inj.Refresh(RefreshOptions{SkipLatency: req.SkipLatency})
		return control.Resp{OK: true, Resets: toResetViews(res.Entries)}
	case control.CmdSetLatency:
		warns, err := setLatency(inj, meta.backing, req)
		if err != nil {
			return control.Resp{OK: false, Err: err.Error()}
		}
		return control.Resp{OK: true, Warns: warns}
	case control.CmdSetSpare:
		if !req.HasSpare {
			return control.Resp{OK: false, Err: "no spare value specified"}
		}
		inj.SetSpareBlocks(req.Spare, req.SpareBlockSize) // blockSize<1 与 count<-1 由 SetSpareBlocks 统一钳制（单一真实来源）
		return control.Resp{OK: true}
	case control.CmdStatus:
		return control.Resp{OK: true, Rules: toControlViews(inj.List()), Profile: profileName(inj.Profile()), Spare: inj.Spare(), SpareBlockSize: inj.SpareBlockSize(), Capacity: inj.Capacity(), Speed: inj.Speed()}
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

// setLatency 处理 set-latency：解析预设档/手动性能旋钮/--speed，按 backing 实测上限
// 钳制后写入 profile，倍速单独写入。返回告警列表（可能为空）与错误（参数非法时）。
// 钳制由 [Injector.SetProfileCalibrated] 统一完成（与库用户共用同一实现）。
// --profile 与 --rand/--seq 互斥（叠加会产生难解释的半覆盖混合 profile）；--speed
// 可与任一组合。
func setLatency(inj *Injector, backing string, req control.Req) ([]string, error) {
	if req.Profile == "" && !req.HasSpeed && !req.HasRand && !req.HasSeq {
		return nil, errf("未指定任何参数；用 --profile / --rand / --seq / --speed 之一")
	}
	// 预设档与手动旋钮互斥：二者叠加时旋钮只覆盖随机/带宽字段，预设的其余字段
	// （如顺序 per-request、带宽）会静默保留，形成既非预设也非旋钮意图的混合 profile。
	// 自定义组合请用库 API ProfileFromKnobs。
	if req.Profile != "" && (req.HasRand || req.HasSeq) {
		return nil, errf("--profile 与 --rand/--seq 互斥：预设档与手动旋钮请二选一")
	}

	var warns []string
	if req.Profile != "" || req.HasRand || req.HasSeq {
		var target LatencyProfile
		switch {
		case req.Profile != "":
			p, ok := ProfileByName(req.Profile)
			if !ok {
				return nil, errf("未知预设档：%q（none/memory/ssd/hdd）", req.Profile)
			}
			target = p
		default:
			// 从零开始用手动旋钮构建：走 ProfileFromKnobs（与 mount --rand/--seq 的
			// buildInjector 同一构造入口，避免两套实现漂移）。未给出的旋钮传 0 = 该维度不启用。
			var randDur time.Duration
			if req.HasRand {
				if req.RandNs < 0 {
					return nil, errf("--rand 不能为负（得到 %d ns）", req.RandNs)
				}
				randDur = time.Duration(req.RandNs)
			}
			var seqBw float64
			if req.HasSeq {
				seqBw = req.SeqBw
			}
			target = ProfileFromKnobs(randDur, seqBw)
		}
		warns = append(warns, inj.SetProfileCalibrated(backing, target)...)
	}

	// 全局倍速（可与 profile 或旋钮并存）。<=0 会被 SetSpeed 钳制为 1.0；这里明示告警，
	// 避免用户想"清零/暂停延迟"却静默得到正常速度（spec/latency.md 注明的既定钳制行为）。
	if req.HasSpeed {
		if req.Speed <= 0 {
			warns = append(warns, fmt.Sprintf("speed %s <= 0 is invalid, treating as 1.0 (normal); use a small positive value to slow down", trimFloat(req.Speed)))
		}
		inj.SetSpeed(req.Speed)
	}
	return warns, nil
}

// errf 是 fmt.Errorf 的简写，避免在本文件多处重复 fmt.Errorf。
func errf(format string, args ...any) error { return fmt.Errorf(format, args...) }

// buildDump 构造一份全量快照：规则 + 挂载元信息 + 完整延迟 profile。
func buildDump(inj *Injector, meta mountMeta) *control.DumpView {
	p := inj.Profile()
	return &control.DumpView{
		Rules:          toControlViews(inj.List()),
		MountPID:       meta.pid,
		Backing:        meta.backing,
		Socket:         meta.socket,
		MountTime:      meta.mountTime,
		ProfileName:    profileName(p),
		Speed:          inj.Speed(),
		Spare:          inj.Spare(),
		SpareBlockSize: inj.SpareBlockSize(),
		Capacity:       inj.Capacity(),
		ProfileFields:  profileFields(p),
	}
}

// toControlViews 把 faultfs.RuleView 列表转成 control 协议的 RuleView。
func toControlViews(vs []RuleView) []control.RuleView {
	out := make([]control.RuleView, len(vs))
	for i, v := range vs {
		out[i] = control.RuleView{
			ID:           v.ID,
			Op:           v.Op,
			Path:         v.Path,
			Off:          v.Off,
			OffLen:       v.OffLen,
			Errno:        int(v.Errno),
			N:            v.N,
			HealOnWrite:  v.HealOnWrite,
			Healed:       v.Healed,
			HealedBlocks: v.HealedBlocks,
			TotalBlocks:  v.TotalBlocks,
			Remaining:    v.Remaining,
		}
	}
	return out
}

// toResetViews 把 faultfs.ResetEntry 列表转成 control 协议的 ResetView。
func toResetViews(es []ResetEntry) []control.ResetView {
	out := make([]control.ResetView, len(es))
	for i, e := range es {
		out[i] = control.ResetView{What: e.What, ID: e.ID, Before: e.Before, After: e.After}
	}
	return out
}
