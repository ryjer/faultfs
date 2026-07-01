// Command faultfs 挂载并管理一个可编程故障注入 FUSE 文件系统（测试用）。
//
// `faultfs mount` 启动一个 faultfs 守护进程（backing 透传 + 在线 control socket），
// 其余子命令（add/rm/clear/refresh/list/status/dump/latency/spare/badsector）作为客户端
// 通过 control socket 在线操控规则引擎与延迟模型，从而让非 Go 程序与 AI 也能构造并
// 驱动 faultfs。
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
		newDumpCmd(), newLatencyCmd(), newSpareCmd(), newBadsectorCmd(),
	)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// ---- mount / unmount ----

func newMountCmd() *cobra.Command {
	var detach bool
	c := &cobra.Command{
		Use:   "mount <backing> <mp>",
		Short: "挂载一个 faultfs（backing 透传），前台守护；--detach 后台运行",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			backing, mp := args[0], args[1]
			if detach {
				return detachSelf(backing, mp)
			}
			return faultfs.Run(mp, backing, faultfs.NewInjector())
		},
	}
	c.Flags().BoolVar(&detach, "detach", false, "后台守护，立即返回")
	return c
}

// detachSelf 重新以非 detach 模式 fork 自身，新会话脱离终端，父进程立即返回。
func detachSelf(backing, mp string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("detach: cannot find executable: %w", err)
	}
	c := exec.Command(exe, "mount", backing, mp)
	c.Stdin = nil
	c.Stdout = nil
	c.Stderr = nil
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return fmt.Errorf("detach: %w", err)
	}
	fmt.Fprintf(os.Stderr, "faultfs mounted at %s (pid %d, socket %s)\n", mp, c.Process.Pid, control.SocketPath(mp))
	return nil
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
	return &cobra.Command{
		Use: "refresh <mp>", Short: "重置所有规则到初始态（healed/remaining/spare）", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := sendCtl(args[0], control.Req{Cmd: control.CmdRefreshRules})
			return err
		},
	}
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

func newLatencyCmd() *cobra.Command {
	var profile string
	var speed float64
	c := &cobra.Command{
		Use: "latency <mp>", Short: "设设备延迟档（--profile）与全局倍速（--speed）", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := control.Req{Cmd: control.CmdSetLatency, Profile: profile}
			if cmd.Flags().Changed("speed") {
				req.HasSpeed = true
				req.Speed = speed
			}
			_, err := sendCtl(args[0], req)
			return err
		},
	}
	c.Flags().StringVar(&profile, "profile", "", "none|memory|ssd|hdd（空=不改）")
	c.Flags().Float64Var(&speed, "speed", 1.0, "全局倍速（1.0 正常；>1 慢放；<1 快放）")
	return c
}

func newSpareCmd() *cobra.Command {
	return &cobra.Command{
		Use: "spare <mp> <n>", Short: "设置备用扇区预算（-1 无限）", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("n: %w", err)
			}
			_, err = sendCtl(args[0], control.Req{Cmd: control.CmdSetSpare, Spare: n, HasSpare: true})
			return err
		},
	}
}

func newBadsectorCmd() *cobra.Command {
	var path string
	var off, length int64
	var spare int
	c := &cobra.Command{
		Use: "badsector <mp>", Short: "标记坏扇区（read EIO，write 治愈）：封装为 --heal-on-write read 规则", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mp := args[0]
			req := control.Req{
				Cmd: control.CmdAddRule, Op: faultfs.OpRead, Path: path,
				Off: off, OffLen: length, Errno: int(syscall.EIO), HealOnWrite: true,
			}
			resp, err := sendCtl(mp, req)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("spare") {
				if _, err := sendCtl(mp, control.Req{Cmd: control.CmdSetSpare, Spare: int64(spare), HasSpare: true}); err != nil {
					return err
				}
			}
			fmt.Println(resp.ID)
			return nil
		},
	}
	c.Flags().StringVar(&path, "path", "", "挂载内相对路径子串")
	c.Flags().Int64Var(&off, "off", 0, "坏区起始 offset")
	c.Flags().Int64Var(&length, "len", 4096, "坏区长度（=OffLen）")
	c.Flags().IntVar(&spare, "spare", -1, "备用扇区预算（-1 无限）")
	return c
}

// errnoNames 是 syscall.Errno → 名称的映射，可作为 parseErrno 和 errnoName 的
// 单一真实来源。添加新 errno 时只需更新此 map。
var errnoNames = map[syscall.Errno]string{
	syscall.EIO:     "EIO",
	syscall.ENOSPC:  "ENOSPC",
	syscall.EROFS:   "EROFS",
	syscall.ESTALE:  "ESTALE",
	syscall.ENODEV:  "ENODEV",
	syscall.EUCLEAN: "EUCLEAN",
	syscall.EACCES:  "EACCES",
	syscall.EPERM:   "EPERM",
	syscall.ENOSYS:  "ENOSYS",
	syscall.EFBIG:   "EFBIG",
	syscall.EDQUOT:  "EDQUOT",
}

// nameToErrno 在 init 中由 errnoNames 自动构建。
var nameToErrno map[string]syscall.Errno

func init() {
	nameToErrno = make(map[string]syscall.Errno, len(errnoNames))
	for e, n := range errnoNames {
		nameToErrno[n] = e
	}
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
			fmt.Printf("rules=%d  spare=%d  speed=%v  profile=%s\n",
				len(resp.Rules), resp.Spare, resp.Speed, resp.Profile)
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
			fmt.Printf("profile=%s speed=%v spare=%d rules=%d\n",
				d.ProfileName, d.Speed, d.Spare, len(d.Rules))
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
