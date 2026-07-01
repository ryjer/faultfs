// Command faultfs 挂载并管理一个可编程故障注入 FUSE 文件系统（测试用）。
//
// `faultfs mount` 启动一个 faultfs 守护进程（backing 透传 + 在线 control socket），
// 其余子命令作为客户端通过 control socket 在线操控规则引擎与设备属性：
//
//	add <mp> […]           加注入规则；add badsector <mp> […] 加可修复坏扇区
//	rm/clear/refresh/list  管理规则（refresh 同时重置 spare 到初始值）
//	set latency <mp> […]   设备延迟档/倍速/性能参数（设备固有属性，非规则）
//	set spare <mp> <spec>  备用块预算（设备固有属性；如 8*4KiB）
//	status/dump            只读快照
//
// 设备延迟与备用扇区属于设备固有属性（不能像规则那样增删），故用 set 子命令设置。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ryjer/fss/faultfs"
	"github.com/ryjer/fss/faultfs/control"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "faultfs",
		Short: "可编程故障注入 FUSE 文件系统（测试用）",
	}
	root.AddCommand(
		newMountCmd(), newUnmountCmd(), newAddCmd(), newRmCmd(),
		newClearCmd(), newRefreshCmd(), newListCmd(), newStatusCmd(),
		newDumpCmd(), newSetCmd(),
	)
	// SilenceUsage：cobra 默认在每条 RunE 错误后把整段 Usage:+Flags: 打到 stderr（很噪）。
	// 置于根命令即全局静默 Usage（cobra ExecuteC 的 !c.SilenceUsage && !cmd.SilenceUsage
	// 逻辑下，二者其一为真即不打印），仍保留 "Error: <msg>" 一行。
	root.SilenceUsage = true
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// ---- mount / unmount ----

func newMountCmd() *cobra.Command {
	var detach bool
	var randStr, seqStr, spareStr string
	c := &cobra.Command{
		Use:   "mount <backing> <mp>",
		Short: "挂载一个 faultfs（backing 透传），前台守护；--detach 后台运行",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			backing, mp := args[0], args[1]
			if detach {
				// detach：把挂载参数透传给 fork 出的守护子进程，由其完成 setup。
				return detachSelf(backing, mp, mountExtraArgs(cmd))
			}
			inj, warns, err := buildInjector(backing, randStr, seqStr, spareStr)
			if err != nil {
				return err
			}
			for _, w := range warns {
				fmt.Fprintf(os.Stderr, "warning: %s\n", w)
			}
			return faultfs.Run(mp, backing, inj)
		},
	}
	c.Flags().BoolVar(&detach, "detach", false, "后台守护，立即返回")
	c.Flags().StringVar(&randStr, "rand", "", "初始随机寻址延迟（ns/us/ms，如 8ms；空=不启用性能模拟，直透 backing）")
	c.Flags().StringVar(&seqStr, "seq", "", "初始顺序读写速度（M=MiB/s、G=GiB/s，如 100M；空=不启用）")
	c.Flags().StringVar(&spareStr, "spare", "", "初始备用块预算（如 8*4KiB = 8 个 4KiB 块；空=0，挂载后用 set spare 设）")
	return c
}

// mountExtraArgs 收集 mount 上被显式设置的性能/备用参数，作为透传给 --detach 子进程的
// 额外命令行参数（保持前台/后台两条路径用同一份参数完成 setup）。
func mountExtraArgs(cmd *cobra.Command) []string {
	var args []string
	for _, name := range []string{"rand", "seq", "spare"} {
		if cmd.Flags().Changed(name) {
			v, _ := cmd.Flags().GetString(name)
			args = append(args, "--"+name, v)
		}
	}
	return args
}

// buildInjector 按挂载参数构造 *Injector 并设初始 profile/spare（同步 initial 快照，供
// refresh 复位）。--rand/--seq 至少其一给定则启用性能模拟（按 backing 钳制并返回告警）；
// --spare 给定则设块预算，否则保持 NewInjector 默认 0。返回 (inj, warns, err)。
func buildInjector(backing, randStr, seqStr, spareStr string) (*faultfs.Injector, []string, error) {
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
	return inj, warns, nil
}

// detachSelf 重新以非 detach 模式 fork 自身，新会话脱离终端；父进程等待 control
// socket 就绪后返回，从而 `mount --detach` 一返回即可接收 add/set 等子命令。
// extraArgs 是透传给子进程的挂载参数（--rand/--seq/--spare），保证后台路径同样完成
// setup（含按 backing 钳制）。子进程 stderr 继承父进程 stderr：setup（解析参数、
// 校准、钳制）先于 control socket Listen，故启动期告警会先于 socket 就绪输出，
// 父进程据此把 warning: 暴露给用户（子进程退出码同样反映失败）。
func detachSelf(backing, mp string, extraArgs []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("detach: cannot find executable: %w", err)
	}
	args := append([]string{"mount", backing, mp}, extraArgs...)
	c := exec.Command(exe, args...)
	c.Stdin = nil
	c.Stdout = nil
	c.Stderr = os.Stderr
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return fmt.Errorf("detach: %w", err)
	}
	pid := c.Process.Pid
	sock := control.SocketPath(mp)
	// 等待守护进程绑定 control socket（轮询一次 status round-trip）。就绪即返回；
	// 超时通常意味着子进程已退出（如 /dev/fuse 不可用、backing 无效）——返回错误让
	// mount 以非零退出，避免脚本误以为挂载成功、却在下一条命令（add/set…）上才失败。
	if rerr := waitReady(sock, 5*time.Second); rerr != nil {
		return fmt.Errorf("mount: control socket %s 未就绪（%v）；守护进程可能未成功挂载（请检查 /dev/fuse 与 backing）", sock, rerr)
	}
	fmt.Fprintf(os.Stderr, "faultfs mounted at %s (pid %d, socket %s)\n", mp, pid, sock)
	return nil
}

// waitReady 反复向 socket 发一次 status 请求，直到成功 round-trip（守护进程已监听并
// 服务）或超时。control socket 的 Listen 先于 Serve，故一次成功的响应即代表就绪。
func waitReady(socket string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if _, err := control.Send(socket, control.Req{Cmd: control.CmdStatus}); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newUnmountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unmount <mp>",
		Short: "卸载 faultfs（fusermount3 -u；挂载进程随后自动退出）",
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

// ---- 在线规则/延迟管理（走 control socket）----

// sendCtl 发请求到 mp 的 control socket；返回响应或在失败/!OK 时返回错误。
func sendCtl(mp string, req control.Req) (*control.Resp, error) {
	resp, err := control.Send(control.SocketPath(mp), req)
	if err != nil {
		return nil, fmt.Errorf("control socket %s: %w（mount 未运行或未就绪?）", control.SocketPath(mp), err)
	}
	if !resp.OK {
		return &resp, fmt.Errorf("%s", resp.Err)
	}
	return &resp, nil
}

func newAddCmd() *cobra.Command {
	var op, path, errnoStr string
	var off, offLen int64
	var n int
	var heal bool
	c := &cobra.Command{
		Use: "add <mp>", Short: "添加一条注入规则，打印分配的 ID", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			errnoVal, err := parseErrno(errnoStr)
			if err != nil {
				return err
			}
			req := control.Req{
				Cmd: control.CmdAddRule, Op: op, Path: path, Off: off, OffLen: offLen,
				Errno: int(errnoVal), N: n, HealOnWrite: heal,
			}
			resp, err := sendCtl(args[0], req)
			if err != nil {
				return err
			}
			fmt.Println(resp.ID)
			return nil
		},
	}
	c.Flags().StringVar(&op, "op", "read", "open|read|write|create|getattr|statfs|getxattr|setxattr|...")
	c.Flags().StringVar(&path, "path", "", "挂载内相对路径子串（空=任意）")
	c.Flags().Int64Var(&off, "off", 0, "起始 offset（仅 read/write）")
	c.Flags().Int64Var(&offLen, "off-len", 0, "offset 区间长度（0=任意 offset；>0=区间[off,off+len)，精确点用 1）")
	c.Flags().StringVar(&errnoStr, "errno", "EIO", "errno 名（EIO/ENOSPC/EROFS/ESTALE/...）或数字")
	c.Flags().IntVar(&n, "n", 0, "前 N 次注入（0=永久）")
	c.Flags().BoolVar(&heal, "heal-on-write", false, "可修复坏扇区（read EIO，write 治愈）")
	// badsector 作为 add 的子命令：坏扇区本质是"封装为 heal-on-write read 的注入规则"，
	// 属于规则的范畴，故挂在 add 下而非 set（set 留给设备固有属性）。
	c.AddCommand(newBadsectorCmd())
	return c
}

func newRmCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rm <mp> <id>", Short: "按 ID 删除一条规则", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("id: %w", err)
			}
			_, err = sendCtl(args[0], control.Req{Cmd: control.CmdDeleteRule, ID: id})
			return err
		},
	}
}

func newClearCmd() *cobra.Command {
	return &cobra.Command{
		Use: "clear <mp>", Short: "清空所有规则", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := sendCtl(args[0], control.Req{Cmd: control.CmdClear})
			return err
		},
	}
}

func newRefreshCmd() *cobra.Command {
	var keepLatency bool
	c := &cobra.Command{
		Use: "refresh <mp>", Short: "重置所有规则到初始态（healed/remaining/spare，默认含性能参数）", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := sendCtl(args[0], control.Req{Cmd: control.CmdRefreshRules, SkipLatency: keepLatency})
			if err != nil {
				return err
			}
			// 把发生的每条复位/变动打到 stderr（诊断日志，保持 stdout 纯净）：
			// 不留静默聚合编号，逐条说明哪个规则/spare/latency 变了、前后值。
			for _, e := range resp.Resets {
				if e.What == "rule" {
					fmt.Fprintf(os.Stderr, "reset rule %d: %s -> %s\n", e.ID, e.Before, e.After)
				} else {
					fmt.Fprintf(os.Stderr, "reset %s: %s -> %s\n", e.What, e.Before, e.After)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&keepLatency, "keep-latency", false, "保留当前性能参数（profile/speed）不动")
	return c
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list <mp>", Short: "列出规则（含运行时状态）", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := sendCtl(args[0], control.Req{Cmd: control.CmdListRules})
			if err != nil {
				return err
			}
			if len(resp.Rules) == 0 {
				fmt.Println("(no rules)")
			}
			for _, r := range resp.Rules {
				fmt.Printf("id=%d op=%s path=%q off=%d off-len=%d errno=%d n=%d heal=%v healed=%v rem=%d\n",
					r.ID, r.Op, r.Path, r.Off, r.OffLen, r.Errno, r.N, r.HealOnWrite, r.Healed, r.Remaining)
			}
			return nil
		},
	}
}

// newSetCmd 是"设备固有属性"分组的父命令：latency（延迟档/倍速/性能参数）与
// spare（备用扇区预算）。这些是设备的属性而非可增删的规则，故用 set 子命令设置。
func newSetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "set",
		Short: "设置设备固有属性（延迟/性能参数、备用扇区预算）",
	}
	c.AddCommand(newSetLatencyCmd(), newSetSpareCmd())
	return c
}

// newSetLatencyCmd 对应 `faultfs set latency <mp>`：设备延迟档（--profile）、全局倍速
// （--speed），以及手动性能参数（--rand 随机寻址延迟、--seq 顺序读写速度）。详见
// spec/latency.md。设备性能受 backing（通常 tmpfs）实际上限约束，超出会告警并钳制。
// --profile 与 --rand/--seq 互斥（叠加会产生难解释的混合 profile）；--speed 可与任一组合。
func newSetLatencyCmd() *cobra.Command {
	var profile string
	var speed float64
	var randStr, seqStr string
	c := &cobra.Command{
		Use:   "latency <mp>",
		Short: "设设备延迟档（--profile）、倍速（--speed）或手动性能参数（--rand/--seq）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := control.Req{Cmd: control.CmdSetLatency, Profile: profile}
			if cmd.Flags().Changed("speed") {
				req.HasSpeed = true
				req.Speed = speed
			}
			if cmd.Flags().Changed("rand") {
				d, err := faultfs.ParseLatency(randStr)
				if err != nil {
					return err
				}
				req.HasRand = true
				req.RandNs = int64(d)
			}
			if cmd.Flags().Changed("seq") {
				bw, err := faultfs.ParseSpeed(seqStr)
				if err != nil {
					return err
				}
				req.HasSeq = true
				req.SeqBw = bw
			}
			resp, err := sendCtl(args[0], req)
			if err != nil {
				return err
			}
			for _, w := range resp.Warns {
				fmt.Fprintf(os.Stderr, "warning: %s\n", w)
			}
			return nil
		},
	}
	c.Flags().StringVar(&profile, "profile", "", "预设档：none|memory|ssd|hdd（空=不改）")
	c.Flags().Float64Var(&speed, "speed", 1.0, "全局倍速（1.0 正常；>1 慢放；<1 快放）")
	c.Flags().StringVar(&randStr, "rand", "", "随机寻址延迟（单位 ns/us/ms，如 8ms；空=不改；不可为负）")
	c.Flags().StringVar(&seqStr, "seq", "", "顺序读写速度（单位 M=MiB/s、G=GiB/s，如 100M；空=不改；最小 1 B/s，0=不限速）")
	return c
}

func newSetSpareCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "spare <mp> <spec>",
		Short: "设备用块预算（<count>*<size> 如 8*4KiB，或纯数量如 8；-1 无限）；refresh 会还原到该初始值",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			count, bs, err := faultfs.ParseSpareSpec(args[1])
			if err != nil {
				return err
			}
			_, err = sendCtl(args[0], control.Req{Cmd: control.CmdSetSpare, Spare: count, SpareBlockSize: bs, HasSpare: true})
			return err
		},
	}
}

func newBadsectorCmd() *cobra.Command {
	var path string
	var off, length int64
	var spare string
	c := &cobra.Command{
		Use: "badsector <mp>", Short: "标记坏扇区（read EIO，write 治愈）：封装为 --heal-on-write read 规则", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mp := args[0]
			// 先设 spare（如指定）：坏扇区规则由 write 触发治愈并消耗 spare，故 spare 必须在
			// 规则生效前就位。若先加规则、后设 spare 且后者失败，规则会带着非预期的 spare
			// 留存，治愈静默不消耗预算。故先设 spare：规则添加失败时最多留下用户显式指定的
			// spare（无害的设备属性），不会留下"坏扇区规则 + 错误 spare"。不带 --spare 时
			// 不改 spare（保持挂载的默认 0——需先 set spare 才能治愈）。
			if cmd.Flags().Changed("spare") {
				count, bs, err := faultfs.ParseSpareSpec(spare)
				if err != nil {
					return err
				}
				if _, err := sendCtl(mp, control.Req{Cmd: control.CmdSetSpare, Spare: count, SpareBlockSize: bs, HasSpare: true}); err != nil {
					return err
				}
			}
			req := control.Req{
				Cmd: control.CmdAddRule, Op: faultfs.OpRead, Path: path,
				Off: off, OffLen: length, Errno: int(syscall.EIO), HealOnWrite: true,
			}
			resp, err := sendCtl(mp, req)
			if err != nil {
				return err
			}
			fmt.Println(resp.ID)
			return nil
		},
	}
	c.Flags().StringVar(&path, "path", "", "挂载内相对路径子串（必填）")
	c.Flags().Int64Var(&off, "off", 0, "坏区起始 offset")
	c.Flags().Int64Var(&length, "len", 4096, "坏区长度（=OffLen；治愈时按 ceil(len/blockSize) 整块消耗备用）")
	c.Flags().StringVar(&spare, "spare", "", "备用块预算（如 8*4KiB 或 8；-1 无限；不设则不改当前预算）")
	// --path 必填：空 path 的规则会子串匹配任意文件，对坏扇区这种高危便捷命令而言，
	// 忘带 --path 而静默生成"全局坏扇区"是不可接受的脚枪，故强制要求显式指定。
	_ = c.MarkFlagRequired("path")
	return c
}

// errnoNames 是 syscall.Errno → 名称的映射，可作为 parseErrno 和 errnoName 的
// 单一真实来源。添加新 errno 时只需更新此 map。
var errnoNames = map[syscall.Errno]string{
	syscall.EIO:        "EIO",
	syscall.ENOSPC:     "ENOSPC",
	syscall.EROFS:      "EROFS",
	syscall.ESTALE:     "ESTALE",
	syscall.ENODEV:     "ENODEV",
	syscall.EUCLEAN:    "EUCLEAN",
	syscall.EACCES:     "EACCES",
	syscall.EPERM:      "EPERM",
	syscall.ENOSYS:     "ENOSYS",
	syscall.EFBIG:      "EFBIG",
	syscall.EDQUOT:     "EDQUOT",
	syscall.ENODATA:    "ENODATA",    // xattr：属性不存在（getxattr/removexattr）
	syscall.EOPNOTSUPP: "EOPNOTSUPP", // xattr：不支持（filesystem/namespce）
	syscall.ERANGE:     "ERANGE",     // xattr：缓冲过小（getxattr/listxattr）
	syscall.E2BIG:      "E2BIG",      // xattr：属性名/值过大
}

// nameToErrno 在 init 中由 errnoNames 自动构建。
var nameToErrno map[string]syscall.Errno

func init() {
	nameToErrno = make(map[string]syscall.Errno, len(errnoNames))
	for e, n := range errnoNames {
		nameToErrno[n] = e
	}
	// ENOTSUP 与 EOPNOTSUPP 在 Linux 同值；errnoNames 只保留 EOPNOTSUPP（显示用），
	// 这里补 ENOTSUP 作为解析别名，让 xattr "not supported" 场景两种写法都被接受。
	nameToErrno["ENOTSUP"] = syscall.EOPNOTSUPP
}

// parseErrno 把 errno 名（EIO/ENOSPC/...）或数字字符串转 syscall.Errno。无法解析时返回错误。
func parseErrno(s string) (syscall.Errno, error) {
	trimmed := strings.TrimSpace(s)
	if n, err := strconv.Atoi(trimmed); err == nil {
		return syscall.Errno(n), nil
	}
	if e, ok := nameToErrno[strings.ToUpper(trimmed)]; ok {
		return e, nil
	}
	return 0, fmt.Errorf("unknown errno: %q", s)
}

// ---- status / dump（只读快照）----

// newStatusCmd 输出精简概览：规则数 / spare / speed / profile，每条规则一行
// （id/op/healed/remaining/errno）。--json 输出结构化 Resp。
func newStatusCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use: "status <mp>", Short: "概览：规则/spare/speed/profile（精简）", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := sendCtl(args[0], control.Req{Cmd: control.CmdStatus})
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(resp)
			}
			fmt.Printf("rules=%d  spare=%s  speed=%v  profile=%s\n",
				len(resp.Rules), faultfs.FormatSpare(resp.Spare, resp.SpareBlockSize), resp.Speed, resp.Profile)
			for _, r := range resp.Rules {
				fmt.Printf("  [%d] op=%s path=%q healed=%v rem=%d errno=%d(%s)\n",
					r.ID, r.Op, r.Path, r.Healed, r.Remaining, r.Errno, errnoName(r.Errno))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "结构化 JSON 输出")
	return c
}

// newDumpCmd 输出全量诊断快照：挂载元信息（pid/backing/socket/mount_time）+
// 完整规则配置 + 完整 LatencyProfile 各字段。默认人类可读 key=value 块；
// --json 输出结构化 DumpView，适合 `> /tmp/dump.json` 沉淀。
func newDumpCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use: "dump <mp>", Short: "全量诊断快照（规则+挂载元信息+完整延迟 profile）", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := sendCtl(args[0], control.Req{Cmd: control.CmdDump})
			if err != nil {
				return err
			}
			if resp.Dump == nil {
				return fmt.Errorf("daemon 未返回 dump（版本过旧?）")
			}
			if asJSON {
				return writeJSON(resp.Dump)
			}
			d := resp.Dump
			fmt.Printf("mount_pid=%d\nbacking=%s\nsocket=%s\nmount_time=%s\n",
				d.MountPID, d.Backing, d.Socket, d.MountTime)
			fmt.Printf("profile=%s speed=%v spare=%s rules=%d\n",
				d.ProfileName, d.Speed, faultfs.FormatSpare(d.Spare, d.SpareBlockSize), len(d.Rules))
			for _, r := range d.Rules {
				fmt.Printf("  [%d] op=%s path=%q off=%d off-len=%d errno=%d(%s) n=%d heal=%v healed=%v rem=%d\n",
					r.ID, r.Op, r.Path, r.Off, r.OffLen, r.Errno, errnoName(r.Errno),
					r.N, r.HealOnWrite, r.Healed, r.Remaining)
			}
			fmt.Println("profile_fields:")
			for _, k := range sortedKeys(d.ProfileFields) {
				fmt.Printf("  %s=%s\n", k, d.ProfileFields[k])
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "结构化 JSON 输出")
	return c
}

// writeJSON 以 2 空格缩进把 v 编码到 stdout。
func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// errnoName 反查常见 errno 数字对应的名称；未知返回 "?"。数据来源为 [errnoNames] map。
func errnoName(n int) string {
	if name, ok := errnoNames[syscall.Errno(n)]; ok {
		return name
	}
	return "?"
}

// sortedKeys 返回 map 的键排序后的切片，便于确定性输出。
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
