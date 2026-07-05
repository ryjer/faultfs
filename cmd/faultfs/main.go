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
	"os"

	"github.com/spf13/cobra"
)

// detachStatusFDEnv 是父进程传给 --detach 子进程的环境变量，值为 status pipe 的 fd 号
// （子进程经 ExtraFiles 继承为 fd 3）。子进程 setup 完成（就绪/失败）即向该 fd 写一字节
// 状态，父进程据此立即返回——失败时拿到的就是真实 setup 错误，无需空等 5s socket 轮询。
const detachStatusFDEnv = "FAULTFS_DETACH_STATUS_FD"

func main() {
	root := &cobra.Command{
		Use:   "faultfs",
		Short: biHelp("Programmable fault-injection FUSE filesystem (for testing)", "可编程故障注入 FUSE 文件系统（测试用）"),
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
	// --detach fork 出的子进程经 status pipe 把 setup 错误回报给父进程（见 detachSelf）；
	// 此时静默子进程的 cobra "Error:" 打印，避免同一错误既走 pipe 又打到继承的 stderr 重复。
	if os.Getenv(detachStatusFDEnv) != "" {
		root.SilenceErrors = true
	}
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
