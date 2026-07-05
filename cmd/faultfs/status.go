package main

import (
	"fmt"

	"github.com/ryjer/faultfs"
	"github.com/ryjer/faultfs/control"
	"github.com/spf13/cobra"
)

// ---- status / dump（只读快照）----

// newStatusCmd 输出精简概览：规则数 / spare / speed / profile，每条规则一行
// （id/op/healed/remaining/errno）。--json 输出结构化 Resp。
func newStatusCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use: "status <mp>", Short: biHelp("Overview: rules/spare/speed/profile (compact)", "概览：规则/spare/speed/profile（精简）"), Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := sendCtl(args[0], control.Req{Cmd: control.CmdStatus})
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(resp)
			}
			fmt.Printf("rules=%d  spare=%s  capacity=%s  speed=%v  profile=%s\n",
				len(resp.Rules), faultfs.FormatSpare(resp.Spare, resp.SpareBlockSize), formatCapacity(resp.Capacity), resp.Speed, resp.Profile)
			for _, r := range resp.Rules {
				fmt.Printf("  [%d] op=%s path=%q healed=%s rem=%d errno=%d(%s)\n",
					r.ID, r.Op, r.Path, formatHealed(r), r.Remaining, r.Errno, errnoName(r.Errno))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, biHelp("Structured JSON output", "结构化 JSON 输出"))
	return c
}

// newDumpCmd 输出全量诊断快照：挂载元信息（pid/backing/socket/mount_time）+
// 完整规则配置 + 完整 LatencyProfile 各字段。默认人类可读 key=value 块；
// --json 输出结构化 DumpView，适合 `> /tmp/dump.json` 沉淀。
func newDumpCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use: "dump <mp>", Short: biHelp("Full diagnostic snapshot (rules + mount metadata + full latency profile)", "全量诊断快照（规则+挂载元信息+完整延迟 profile）"), Args: cobra.ExactArgs(1),
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
			fmt.Printf("profile=%s speed=%v spare=%s capacity=%s rules=%d\n",
				d.ProfileName, d.Speed, faultfs.FormatSpare(d.Spare, d.SpareBlockSize), formatCapacity(d.Capacity), len(d.Rules))
			for _, r := range d.Rules {
				fmt.Printf("  [%d] op=%s path=%q off=%d off-len=%d errno=%d(%s) n=%d heal=%v healed=%s rem=%d\n",
					r.ID, r.Op, r.Path, r.Off, r.OffLen, r.Errno, errnoName(r.Errno),
					r.N, r.HealOnWrite, formatHealed(r), r.Remaining)
			}
			fmt.Println("profile_fields:")
			for _, k := range sortedKeys(d.ProfileFields) {
				fmt.Printf("  %s=%s\n", k, d.ProfileFields[k])
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, biHelp("Structured JSON output", "结构化 JSON 输出"))
	return c
}
