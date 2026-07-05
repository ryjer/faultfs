package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"

	"github.com/ryjer/faultfs"
	"github.com/ryjer/faultfs/control"
	"github.com/spf13/cobra"
)

// ---- 在线规则管理（走 control socket）----

func newAddCmd() *cobra.Command {
	var op, path, errnoStr string
	var off, offLen int64
	var n int
	var heal bool
	c := &cobra.Command{
		Use: "add <mp>", Short: biHelp("Add an injection rule and print the assigned ID", "添加一条注入规则，打印分配的 ID"), Args: cobra.ExactArgs(1),
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
	c.Flags().StringVar(&op, "op", "read", biHelp("open|opendir|read|readdir|write|create|lookup|mkdir|rmdir|unlink|rename|getattr|statfs|setattr|getxattr|setxattr|removexattr|listxattr|fsync|flush (empty = any op; see Op* constants)", "open|opendir|read|readdir|write|create|lookup|mkdir|rmdir|unlink|rename|getattr|statfs|setattr|getxattr|setxattr|removexattr|listxattr|fsync|flush（空=任意 op；见 Op* 常量）"))
	c.Flags().StringVar(&path, "path", "", biHelp("Relative-path substring within the mount (empty = any)", "挂载内相对路径子串（空=任意）"))
	c.Flags().Int64Var(&off, "off", 0, biHelp("Start offset (read/write only)", "起始 offset（仅 read/write）"))
	c.Flags().Int64Var(&offLen, "off-len", 0, biHelp("Offset range length (0 = any offset; >0 = range [off,off+len); use 1 for an exact point)", "offset 区间长度（0=任意 offset；>0=区间[off,off+len)，精确点用 1）"))
	c.Flags().StringVar(&errnoStr, "errno", "EIO", biHelp("errno name (EIO/ENOSPC/EROFS/ESTALE/...) or number", "errno 名（EIO/ENOSPC/EROFS/ESTALE/...）或数字"))
	c.Flags().IntVar(&n, "n", 0, biHelp("Inject only the first N times (0 = forever)", "前 N 次注入（0=永久）"))
	c.Flags().BoolVar(&heal, "heal-on-write", false, biHelp("Repairable bad sector (read EIO, healed by write)", "可修复坏扇区（read EIO，write 治愈）"))
	// badsector 作为 add 的子命令：坏扇区本质是"封装为 heal-on-write read 的注入规则"，
	// 属于规则的范畴，故挂在 add 下而非 set（set 留给设备固有属性）。
	c.AddCommand(newBadsectorCmd())
	return c
}

func newRmCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rm <mp> <id>", Short: biHelp("Delete one rule by ID", "按 ID 删除一条规则"), Args: cobra.ExactArgs(2),
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
		Use: "clear <mp>", Short: biHelp("Clear all rules", "清空所有规则"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := sendCtl(args[0], control.Req{Cmd: control.CmdClear})
			return err
		},
	}
}

func newRefreshCmd() *cobra.Command {
	var keepLatency bool
	c := &cobra.Command{
		Use: "refresh <mp>", Short: biHelp("Reset all rules to their initial state (healed/remaining/spare; includes perf params by default)", "重置所有规则到初始态（healed/remaining/spare，默认含性能参数）"), Args: cobra.ExactArgs(1),
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
	c.Flags().BoolVar(&keepLatency, "keep-latency", false, biHelp("Keep current perf params (profile/speed) unchanged", "保留当前性能参数（profile/speed）不动"))
	return c
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use: "list <mp>", Short: biHelp("List rules (with runtime state)", "列出规则（含运行时状态）"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := sendCtl(args[0], control.Req{Cmd: control.CmdListRules})
			if err != nil {
				return err
			}
			if len(resp.Rules) == 0 {
				fmt.Println("(no rules)")
			}
			for _, r := range resp.Rules {
				fmt.Printf("id=%d op=%s path=%q off=%d off-len=%d errno=%d n=%d heal=%v healed=%s rem=%d\n",
					r.ID, r.Op, r.Path, r.Off, r.OffLen, r.Errno, r.N, r.HealOnWrite, formatHealed(r), r.Remaining)
			}
			return nil
		},
	}
}

func newBadsectorCmd() *cobra.Command {
	var path string
	var off, length int64
	var spare string
	c := &cobra.Command{
		Use: "badsector <mp>", Short: biHelp("Mark a bad sector (read EIO, healed by write): wrapped as a --heal-on-write read rule", "标记坏扇区（read EIO，write 治愈）：封装为 --heal-on-write read 规则"), Args: cobra.ExactArgs(1),
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
	c.Flags().StringVar(&path, "path", "", biHelp("Relative-path substring within the mount (required)", "挂载内相对路径子串（必填）"))
	c.Flags().Int64Var(&off, "off", 0, biHelp("Bad-sector start offset", "坏区起始 offset"))
	c.Flags().Int64Var(&length, "len", 4096, biHelp("Bad-sector length (= OffLen; healing consumes spare in whole blocks of ceil(len/blockSize))", "坏区长度（=OffLen；治愈时按 ceil(len/blockSize) 整块消耗备用）"))
	c.Flags().StringVar(&spare, "spare", "", biHelp("Spare-block budget (e.g. 8*4KiB or 8; -1 = unlimited; unset = leave current budget unchanged)", "备用块预算（如 8*4KiB 或 8；-1 无限；不设则不改当前预算）"))
	// --path 必填：空 path 的规则会子串匹配任意文件，对坏扇区这种高危便捷命令而言，
	// 忘带 --path 而静默生成"全局坏扇区"是不可接受的脚枪，故强制要求显式指定。
	_ = c.MarkFlagRequired("path")
	return c
}
