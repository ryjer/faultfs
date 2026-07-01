// Package control 实现 faultfs 的控制 socket：挂载守护进程（faultfs mount）
// 在 per-mount unix socket 上监听，`faultfs add[/badsector]`、`rm/clear/refresh/
// list`、`set latency`、`set spare` 等子命令作为客户端发送 JSON 请求，在线修改
// 规则引擎与设备属性。
//
// control 是纯协议 + 传输层：它不 import 父 package faultfs，而是通过一个
// handler 回调（func(Req) Resp）把请求交回挂载方处理，从而避免循环依赖。
package control

// Cmd 是控制协议命令。
type Cmd string

const (
	CmdAddRule      Cmd = "add-rule"
	CmdDeleteRule   Cmd = "delete-rule"
	CmdClear        Cmd = "clear"
	CmdListRules    Cmd = "list-rules"
	CmdRefreshRules Cmd = "refresh-rules"
	CmdSetLatency   Cmd = "set-latency"
	CmdSetSpare     Cmd = "set-spare"
	CmdStatus       Cmd = "status"
	CmdDump         Cmd = "dump"
)

// Req 是客户端请求。Errno 用 int 表达 syscall.Errno（可序列化）。
type Req struct {
	Cmd         Cmd     `json:"cmd"`
	Op          string  `json:"op,omitempty"`
	Path        string  `json:"path,omitempty"`
	Off         int64   `json:"off,omitempty"`
	OffLen      int64   `json:"off_len,omitempty"`
	Errno       int     `json:"errno,omitempty"`
	N           int     `json:"n,omitempty"`
	HealOnWrite bool    `json:"heal_on_write,omitempty"`
	ID          int     `json:"id,omitempty"`
	Profile     string  `json:"profile,omitempty"` // set-latency: "none"/"memory"/"ssd"/"hdd"；空=不改
	Speed       float64 `json:"speed,omitempty"`
	HasSpeed    bool    `json:"has_speed,omitempty"` // 区分“未设”与 0
	RandNs      int64   `json:"rand_ns,omitempty"`   // set-latency: 随机寻址延迟（纳秒）
	HasRand     bool    `json:"has_rand,omitempty"`
	SeqBw       float64 `json:"seq_bw,omitempty"` // set-latency: 顺序读写速度（字节/秒）
	HasSeq      bool    `json:"has_seq,omitempty"`
	Spare       int64   `json:"spare,omitempty"`
	HasSpare    bool    `json:"has_spare,omitempty"`
}

// RuleView 是 list-rules / status 返回的单条规则视图（含运行时状态）。
type RuleView struct {
	ID          int    `json:"id"`
	Op          string `json:"op"`
	Path        string `json:"path,omitempty"`
	Off         int64  `json:"off,omitempty"`
	OffLen      int64  `json:"off_len,omitempty"`
	Errno       int    `json:"errno"`
	N           int    `json:"n,omitempty"`
	HealOnWrite bool   `json:"heal_on_write,omitempty"`
	Healed      bool   `json:"healed,omitempty"`
	Remaining   int    `json:"remaining"`
}

// Resp 是服务端回复。
type Resp struct {
	OK      bool       `json:"ok"`
	Err     string     `json:"err,omitempty"`
	Warns   []string   `json:"warns,omitempty"`   // 非致命告警（如性能参数被钳制到 tmpfs 上限），逐条输出
	ID      int        `json:"id,omitempty"`      // add-rule 分配的 ID
	Rules   []RuleView `json:"rules,omitempty"`   // list-rules / status / dump
	Profile string     `json:"profile,omitempty"` // status / dump：档名
	Speed   float64    `json:"speed,omitempty"`   // status / dump
	Spare   int64      `json:"spare,omitempty"`   // status / dump
	Dump    *DumpView  `json:"dump,omitempty"`    // dump：全量快照
}

// DumpView 是 dump 命令返回的全量快照：规则完整配置 + 挂载元信息 + 完整延迟
// profile 各字段。供 CLI 的 `faultfs dump`（人类可读 key=value 块或 --json）与
// 日志沉淀用。
type DumpView struct {
	Rules         []RuleView        `json:"rules"`          // 完整规则列表（含运行时状态）
	MountPID      int               `json:"mount_pid"`      // daemon 进程 PID
	Backing       string            `json:"backing"`        // 透传的 backing 目录
	Socket        string            `json:"socket"`         // control socket 路径
	MountTime     string            `json:"mount_time"`     // 挂载时刻（RFC3339）
	ProfileName   string            `json:"profile_name"`   // "none"/"memory"/"ssd"/"hdd"/"custom"
	Speed         float64           `json:"speed"`          // 全局倍速
	Spare         int64             `json:"spare"`          // 备用扇区预算（-1 无限）
	ProfileFields map[string]string `json:"profile_fields"` // 完整 LatencyProfile 各字段名→值
}
