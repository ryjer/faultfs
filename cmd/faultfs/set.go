package main

import (
	"fmt"
	"os"

	"github.com/ryjer/faultfs"
	"github.com/ryjer/faultfs/control"
	"github.com/spf13/cobra"
)

// ---- 设备固有属性（latency / spare）----

// newSetCmd 是"设备固有属性"分组的父命令：latency（延迟档/倍速/性能参数）与
// spare（备用扇区预算）。这些是设备的属性而非可增删的规则，故用 set 子命令设置。
func newSetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "set",
		Short: biHelp("Set inherent device attributes (latency/perf params, spare-block budget)", "设置设备固有属性（延迟/性能参数、备用扇区预算）"),
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
		Short: biHelp("Set device latency profile (--profile), global speed factor (--speed), or manual perf params (--rand/--seq)", "设设备延迟档（--profile）、倍速（--speed）或手动性能参数（--rand/--seq）"),
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
	c.Flags().StringVar(&profile, "profile", "", biHelp("Preset profile: none|memory|ssd|hdd (empty = no change)", "预设档：none|memory|ssd|hdd（空=不改）"))
	c.Flags().Float64Var(&speed, "speed", 1.0, biHelp("Global speed factor (1.0 = normal; >1 = slow down; <1 = speed up)", "全局倍速（1.0 正常；>1 慢放；<1 快放）"))
	c.Flags().StringVar(&randStr, "rand", "", biHelp("Random-seek latency (unit ns/us/ms, e.g. 8ms; empty = no change; cannot be negative)", "随机寻址延迟（单位 ns/us/ms，如 8ms；空=不改；不可为负）"))
	c.Flags().StringVar(&seqStr, "seq", "", biHelp("Sequential read/write speed (unit M=MiB/s, G=GiB/s, e.g. 100M; empty = no change; min 1 B/s, 0 = uncapped)", "顺序读写速度（单位 M=MiB/s、G=GiB/s，如 100M；空=不改；最小 1 B/s，0=不限速）"))
	return c
}

func newSetSpareCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "spare <mp> <spec>",
		Short: biHelp("Set the spare-block budget (<count>*<size> e.g. 8*4KiB, or a plain count e.g. 8; -1 = unlimited); refresh restores this initial value", "设备用块预算（<count>*<size> 如 8*4KiB，或纯数量如 8；-1 无限）；refresh 会还原到该初始值"),
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
	// 关闭 interspersed：spec 是位置参数且合法值含负数（-1 无限、-1*4KiB），否则 pflag 会把
	// 起首的 "-1" 当 shorthand flag 拦截（unknown shorthand flag: '1'）。本命令无其他 flag，
	// 故 interspersed=false 让首个位置参数之后的内容全部按位置参数处理，零副作用。
	c.Flags().SetInterspersed(false)
	return c
}
