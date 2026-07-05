package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ryjer/faultfs"
	"github.com/ryjer/faultfs/control"
	"github.com/spf13/cobra"
)

// ---- mount / unmount ----

func newMountCmd() *cobra.Command {
	var detach bool
	var randStr, seqStr, spareStr, capacityStr string
	c := &cobra.Command{
		Use:   "mount <backing> <mp>",
		Short: biHelp("Mount a faultfs (backing passthrough), foreground daemon; --detach to run in background", "挂载一个 faultfs（backing 透传），前台守护；--detach 后台运行"),
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			backing, mp := args[0], args[1]
			if detach {
				// detach：把挂载参数透传给 fork 出的守护子进程，由其完成 setup。
				return detachSelf(backing, mp, mountExtraArgs(cmd))
			}
			// 子进程路径：--detach fork 出来的子进程带着 FAULTFS_DETACH_STATUS_FD，setup 完成
			// 后向该 fd 回报状态；前台进程无此 env，report 为 no-op，行为不变。
			statusW := openStatusWriter(os.Getenv(detachStatusFDEnv))
			report := func(ok bool, msg string) {
				if statusW == nil {
					return
				}
				if ok {
					_, _ = statusW.WriteString("1")
				} else {
					_, _ = statusW.WriteString("0" + msg)
				}
				_ = statusW.Close()
			}
			inj, warns, err := buildInjector(backing, randStr, seqStr, spareStr, capacityStr)
			if err != nil {
				report(false, err.Error()) // 参数/容量解析错误经 pipe 给父进程
				return err
			}
			for _, w := range warns {
				fmt.Fprintf(os.Stderr, "warning: %s\n", w)
			}
			if statusW != nil {
				// 子进程：Run 在 goroutine 里跑（成功则阻塞服务），与 control socket 就绪竞速，
				// 任一先到即回报——setup 失败（如 capacity 校验拒绝）立即反馈，不等 socket 轮询超时。
				errCh := make(chan error, 1)
				go func() { errCh <- faultfs.Run(mp, backing, inj) }()
				if rerr := waitReadyOrError(control.SocketPath(mp), errCh); rerr != nil {
					report(false, rerr.Error())
					return rerr
				}
				report(true, "")
				return <-errCh // 阻塞至卸载信号；正常退出返 nil
			}
			return faultfs.Run(mp, backing, inj)
		},
	}
	c.Flags().BoolVar(&detach, "detach", false, biHelp("Run as background daemon and return immediately", "后台守护，立即返回"))
	c.Flags().StringVar(&randStr, "rand", "", biHelp("Initial random-seek latency increment (ns/us/ms, e.g. 8ms; added on top of backing; empty = no perf simulation)", "初始随机寻址延迟增量（ns/us/ms，如 8ms；叠加在 backing 上；空=不启用性能模拟）"))
	c.Flags().StringVar(&seqStr, "seq", "", biHelp("Initial sequential read/write speed cap (M=MiB/s, G=GiB/s, e.g. 100M; empty = disabled)", "初始顺序读写速度上限（M=MiB/s、G=GiB/s，如 100M；空=不启用）"))
	c.Flags().StringVar(&spareStr, "spare", "", biHelp("Initial spare-block budget (e.g. 8*4KiB = 8 blocks of 4KiB; empty = 0, set later via 'set spare')", "初始备用块预算（如 8*4KiB = 8 个 4KiB 块；空=0，挂载后用 set spare 设）"))
	c.Flags().StringVar(&capacityStr, "capacity", "", biHelp("Simulated capacity cap (e.g. 100M/1G; empty = unlimited. Must be > backing used and < total; for simulating a full disk)", "模拟容量上限（如 100M/1G；空=不限制。须 > backing 已用且 < 总量，用于模拟磁盘满）"))
	return c
}

// mountExtraArgs 收集 mount 上被显式设置的性能/备用参数，作为透传给 --detach 子进程的
// 额外命令行参数（保持前台/后台两条路径用同一份参数完成 setup）。
func mountExtraArgs(cmd *cobra.Command) []string {
	var args []string
	for _, name := range []string{"rand", "seq", "spare", "capacity"} {
		if cmd.Flags().Changed(name) {
			v, _ := cmd.Flags().GetString(name)
			args = append(args, "--"+name, v)
		}
	}
	return args
}

// buildInjector 按挂载参数构造 *Injector 并设初始 profile/spare/capacity（profile/spare 同步
// initial 快照供 refresh 复位；capacity 不进 refresh，mount 固化）。--rand/--seq 至少其一给定则
// 启用性能模拟（rand 为叠加增量、seq 为限制上限；仅 seq 触发 backing 校准+告警）；--spare 给定
// 则设块预算；--capacity 给定则设模拟容量上限（mount 时由 checkCapacityAtMount 校验区间）。
// 返回 (inj, warns, err)。
func buildInjector(backing, randStr, seqStr, spareStr, capacityStr string) (*faultfs.Injector, []string, error) {
	inj := faultfs.NewInjector()
	var warns []string
	if randStr != "" || seqStr != "" {
		var rand time.Duration
		if randStr != "" {
			d, err := faultfs.ParseLatency(randStr)
			if err != nil {
				return nil, nil, err
			}
			rand = d
		}
		var seqBw float64
		if seqStr != "" {
			bw, err := faultfs.ParseSpeed(seqStr)
			if err != nil {
				return nil, nil, err
			}
			seqBw = bw
		}
		target := faultfs.ProfileFromKnobs(rand, seqBw)
		warns = append(warns, inj.SetProfileCalibrated(backing, target)...)
	}
	if spareStr != "" {
		count, bs, err := faultfs.ParseSpareSpec(spareStr)
		if err != nil {
			return nil, nil, err
		}
		inj.SetSpareBlocks(count, bs)
	}
	if capacityStr != "" {
		capVal, err := faultfs.ParseCapacity(capacityStr)
		if err != nil {
			return nil, nil, err
		}
		inj.SetCapacity(capVal)
	}
	return inj, warns, nil
}

// detachSelf 重新以非 detach 模式 fork 自身，新会话脱离终端；父进程通过 status pipe 等待
// 子进程 setup 完成（就绪或失败）后返回，从而 `mount --detach` 一返回即可接收 add/set 等子命令，
// 且 setup 失败（如 capacity 校验拒绝）时立即拿到真实错误而非空等 socket 轮询超时。
// extraArgs 是透传给子进程的挂载参数（--rand/--seq/--spare/--capacity），保证后台路径同样
// 完成 setup。子进程 stderr 继承父进程 stderr：启动期 warning: 由子进程直接打出。
func detachSelf(backing, mp string, extraArgs []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("detach: cannot find executable: %w", err)
	}
	// status pipe：子进程 setup 完成后写 "1"(就绪) 或 "0<msg>"(失败) 到 fd 3，父进程据此返回。
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("detach: status pipe: %w", err)
	}
	args := append([]string{"mount", backing, mp}, extraArgs...)
	c := exec.Command(exe, args...)
	c.Stdin = nil
	c.Stdout = nil
	c.Stderr = os.Stderr
	c.ExtraFiles = []*os.File{pw} // 子进程作为 fd 3 继承（stdin/out/err 之后）
	c.Env = append(os.Environ(), detachStatusFDEnv+"=3")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return fmt.Errorf("detach: %w", err)
	}
	pid := c.Process.Pid
	_ = pw.Close() // 父进程只读
	data, _ := io.ReadAll(pr)
	_ = pr.Close()
	sock := control.SocketPath(mp)
	if len(data) == 0 {
		_ = c.Wait()
		return fmt.Errorf("mount: 守护进程在报告状态前退出（请检查 /dev/fuse 与 backing）")
	}
	if data[0] == '1' {
		fmt.Fprintf(os.Stderr, "faultfs mounted at %s (pid %d, socket %s)\n", mp, pid, sock)
		return nil
	}
	// "0"+msg：子进程 setup 失败的真实错误（子进程已 SilenceErrors，仅经 pipe 回传）。
	_ = c.Wait()
	return fmt.Errorf("mount: %s", strings.TrimSpace(string(data[1:])))
}

// openStatusWriter 把 detachStatusFDEnv 指定的 fd 包装为 *os.File，供子进程写状态。
// env 未设（前台 mount）返回 nil。
func openStatusWriter(fdStr string) *os.File {
	if fdStr == "" {
		return nil
	}
	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		return nil
	}
	return os.NewFile(uintptr(fd), "detach-status")
}

// waitReadyOrError 等待 control socket 就绪（成功）或 Run 返回 setup 错误（失败），任一先到即返回。
// 替代纯轮询：setup 失败（如 capacity 校验拒绝、/dev/fuse 不可用）时 Run 立即返回 err，本函数
// 下一轮 select 即捕获（≤20ms），不必空等 5s 超时。
func waitReadyOrError(socket string, errCh <-chan error) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-errCh:
			if err != nil {
				return err
			}
			return fmt.Errorf("守护进程在 control socket 就绪前退出")
		default:
		}
		if _, err := control.Send(socket, control.Req{Cmd: control.CmdStatus}); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("control socket %s 5s 内未就绪（守护进程可能未成功挂载；请检查 /dev/fuse 与 backing）", socket)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newUnmountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unmount <mp>",
		Short: biHelp("Unmount faultfs (fusermount3 -u; the mount process then exits on its own)", "卸载 faultfs（fusermount3 -u；挂载进程随后自动退出）"),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mp := args[0]
			if out, err := exec.Command("fusermount3", "-u", mp).CombinedOutput(); err != nil {
				if out2, err2 := exec.Command("fusermount", "-u", mp).CombinedOutput(); err2 != nil {
					return fmt.Errorf("unmount %s: %v (%s) / %v (%s)", mp, err, out, err2, out2)
				}
			}
			_ = os.Remove(control.SocketPath(mp))
			return nil
		},
	}
}
