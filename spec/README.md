# faultfs 规格（怎么实现）

这里放的是**实现规格**（字段、协议、数据流）。面向"怎么用"的用户文档在 [`../doc/`](../doc/)：
- [../doc/library.md](../doc/library.md) — Go 库用法（各类错误的示例 Go 代码）
- [../doc/cli.md](../doc/cli.md) — CLI 用法（命令 + 真实日志）

本目录：
- [architecture.md](architecture.md) — 架构图：被测系统→FUSE→faultfs 守护→Injector→loopback→backing
  的数据通路，CLI→control socket→handleControl 的控制通路，以及包布局。
- [injector.md](injector.md) — 规则引擎：Rule 字段、(op,path,off) 匹配、HealOnWrite 坏扇区
  治愈、spare 备用预算、Refresh 重置、在线管理 API（Add/Delete/Clear/List/Refresh/SetSpare/
  SetProfile/SetSpeed）。
- [latency.md](latency.md) — 设备性能模型：LatencyProfile 字段、预设档（none/memory/ssd/hdd）、
  手动旋钮（rand/seq）、speed 倍速、顺序/随机判定、per-byte 带宽、backing(tmpfs) 校准与钳制。
- [control.md](control.md) — 控制协议（Cmd/Req/Resp JSON over unix socket）、SocketPath 规则、
  server/client、`faultfs` CLI 子命令。
