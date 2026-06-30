# faultfs 规格

- [injector.md](injector.md) — 规则引擎与故障模型：Rule 字段、(op,path,off) 匹配、
  HealOnWrite 坏扇区治愈、spare 备用预算、Refresh 重置、在线管理 API（Add/Delete/
  Clear/List/Refresh/SetSpare/SetProfile/SetSpeed）。
- [latency.md](latency.md) — 设备性能模型：LatencyProfile 字段、预设档（none/memory/
  ssd/hdd）、speed 倍速、顺序/随机判定、per-byte 带宽。
- [control.md](control.md) — 控制协议（Cmd/Req/Resp JSON over unix socket）、
  SocketPath 规则、server/client、`faultfs` CLI 子命令。
