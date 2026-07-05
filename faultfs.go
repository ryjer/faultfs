package faultfs

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/ryjer/faultfs/control"
)

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
