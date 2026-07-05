package faultfs

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// knobParseError 是手动性能旋钮（--rand/--seq）解析失败的错误，携带 kind/raw/hint
// 便于 CLI 展示。空串与无法解析的值都走这一种结构化错误（不再用裸 sentinel，
// 让空值也能给出带上下文的提示）。
type knobParseError struct {
	kind string
	raw  string
	hint string
}

func (e *knobParseError) Error() string {
	return "无法解析" + e.kind + "旋钮 " + strconv.Quote(e.raw) + "：" + e.hint
}

// parseStrictInt 解析整数字符串（不允许前后空白以外的额外字符）。失败返回 error。
func parseStrictInt(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// parseFloat 解析浮点数字符串。失败返回 error。
func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

// trimFloat 把浮点格式化为最短可往返表示（自动去掉末尾多余的 0 与孤立的小数点），
// 如 100.0→"100"、2.5→"2.5"。等价于 strconv.FormatFloat(f,'f',-1,64)（-1=最短表示），
// 比固定 2 位小数再裁 0 更准确（不会把 2.555 截成 "2.56"）。
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// ---- 手动性能参数（随机寻址延迟 + 顺序读写速度）----

// 随机寻址延迟与顺序读写速度是用户可手动调节的两个性能旋钮，对应真实设备的两个
// 核心指标：随机寻道/访问延迟、顺序传输带宽。它们与预设档（ProfileHDD/SSD/Memory）
// 等价但更直观——前者用 ns/us/ms，后者用 MiB/s、GiB/s。

const (
	// MiB / GiB 字节数，用于顺序速度单位换算。
	MiB float64 = 1 << 20
	GiB float64 = 1 << 30
)

// ParseLatency 把延迟旋钮字符串解析为 time.Duration。接受 Go duration（"8ms"/
// "200us"/"200µs"/"100ns"/"5s"，单位 ns/us/ms/s）以及裸整数（视为 ns）。
// 空串与负值均报错（负延迟会让 sleepFor 静默当作不延迟，即"要慢却变快"）。
func ParseLatency(s string) (time.Duration, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, &knobParseError{kind: "latency", raw: s, hint: "不能为空；示例：8ms / 200us / 100ns"}
	}
	if d, err := time.ParseDuration(t); err == nil {
		if d < 0 {
			return 0, &knobParseError{kind: "latency", raw: s, hint: "延迟不可为负"}
		}
		return d, nil
	}
	// 裸整数 → 纳秒（与 latency 的 SI 基本单位一致）。
	if n, err := parseStrictInt(t); err == nil {
		if n < 0 {
			return 0, &knobParseError{kind: "latency", raw: s, hint: "延迟不可为负"}
		}
		return time.Duration(n), nil
	}
	return 0, &knobParseError{kind: "latency", raw: s, hint: "示例：8ms / 200us / 100ns"}
}

// ParseSpeed 把顺序速度旋钮字符串解析为字节/秒。接受 "100M"/"100MiB/s"（=100MiB/s）、
// "2G"/"2GiB/s"（=2GiB/s）、"512K"/"512KiB/s"，以及裸数字（=字节/秒）。大小写不敏感。
// 0（含 "0"/"0M"）=不限速；正带宽必须 ≥ 1 B/s（更慢会让 per-byte 延迟溢出或挂死）。
func ParseSpeed(s string) (float64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, &knobParseError{kind: "speed", raw: s, hint: "不能为空；示例：100M / 2G / 100MiB/s"}
	}
	t = strings.TrimSuffix(strings.ToLower(t), "/s")
	bw, err := parseBytesFloat(t)
	if err != nil {
		return 0, &knobParseError{kind: "speed", raw: s, hint: "示例：100M / 2G / 100MiB/s（M=MiB/s，G=GiB/s）"}
	}
	if math.IsNaN(bw) || math.IsInf(bw, 0) {
		return 0, &knobParseError{kind: "speed", raw: s, hint: "速度不能为 NaN/Inf"}
	}
	if bw < 0 {
		return 0, &knobParseError{kind: "speed", raw: s, hint: "速度不可为负"}
	}
	// bw=0 合法（=不限速）。正带宽须 ≥ 1 B/s：per-byte 延迟 = 1s/bw 以 int64 纳秒存储，
	// bw<1 会让单字节延迟 >1s（大读挂死）乃至 1s/bw 溢出 int64 回绕成负（被 sleepFor
	// 当作不延迟，"要慢却变快"）。
	if bw > 0 && bw < 1 {
		return 0, &knobParseError{kind: "speed", raw: s, hint: "速度过小（最小 1 B/s；0=不限速）"}
	}
	return bw, nil
}

// ParseCapacity 解析容量字符串为字节数。接受 "100M"/"100MiB"（=100MiB）、"1G"/"1GiB"、
// "512K"/"512KiB"，以及裸数字（=字节）。大小写不敏感。拒绝负值、NaN/Inf 与超出 int64
// 可表示范围的值。用于 mount --capacity 与 [Injector.SetCapacity]；展示用 [FormatSize]。
func ParseCapacity(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, &knobParseError{kind: "capacity", raw: s, hint: "不能为空；示例：100M / 1G / 100MiB"}
	}
	bw, err := parseBytesFloat(t)
	if err != nil {
		return 0, &knobParseError{kind: "capacity", raw: s, hint: "示例：100M / 1G / 100MiB（M=MiB，G=GiB）"}
	}
	return finiteNonNegInt64("capacity", s, bw)
}

// finiteNonNegInt64 校验 bw 为有限、非负，Round 后安全转为 int64（防 int64(r) 静默回绕）。
// ParseCapacity 与 ParseSpareSpec 的块大小共用此"有限非负 + 安全取整"尾段，避免两处复制
// 漂移（取值校验如空串/单位/整数性由各调用方先做）。kind/raw 用于构造带上下文的 *knobParseError。
// 溢出判定依赖 math.Round 把 r 吸附到 float64 可表示值：r<float64(MaxInt64)(=2^63) 时
// r<=2^63-1024<MaxInt64，int64(r) 必然安全。
func finiteNonNegInt64(kind, raw string, bw float64) (int64, error) {
	if math.IsNaN(bw) || math.IsInf(bw, 0) {
		return 0, &knobParseError{kind: kind, raw: raw, hint: "不能为 NaN/Inf"}
	}
	if bw < 0 {
		return 0, &knobParseError{kind: kind, raw: raw, hint: "不可为负"}
	}
	r := math.Round(bw)
	if r >= float64(math.MaxInt64) {
		return 0, &knobParseError{kind: kind, raw: raw, hint: "超出可表示范围"}
	}
	return int64(r), nil
}

// parseBytesFloat 解析带可选单位（K/KiB/M/MiB/G/GiB，大小写不敏感）的字节数为 float64；
// 无单位视为字节。它是 [ParseSpeed]（速率）与 [ParseSpareSpec]（块大小）共用的单位换算核心，
// 仅做"数字 × 单位"——取值校验（速率下限、块大小整数性等）由各自调用方负责。
func parseBytesFloat(s string) (float64, error) {
	u := strings.ToLower(strings.TrimSpace(s))
	mult := 1.0
	switch {
	case strings.HasSuffix(u, "gib"):
		mult, u = GiB, strings.TrimSuffix(u, "gib")
	case strings.HasSuffix(u, "g"):
		mult, u = GiB, strings.TrimSuffix(u, "g")
	case strings.HasSuffix(u, "mib"):
		mult, u = MiB, strings.TrimSuffix(u, "mib")
	case strings.HasSuffix(u, "m"):
		mult, u = MiB, strings.TrimSuffix(u, "m")
	case strings.HasSuffix(u, "kib"):
		mult, u = 1<<10, strings.TrimSuffix(u, "kib")
	case strings.HasSuffix(u, "k"):
		mult, u = 1<<10, strings.TrimSuffix(u, "k")
	}
	f, err := parseFloat(strings.TrimSpace(u))
	if err != nil {
		return 0, err
	}
	return f * mult, nil
}

// ---- 备用块预算规格（块数量 + 块大小）----
//
// spare 用「<count>*<size>」表达一份块预算，如 8*4KiB = 8 个 4KiB 的备用块。这比单纯
// 的"次数"或"字节数"更贴近真实硬盘的备用扇区模型：治愈一段坏区时按块向上取整消耗
// （见 [Injector] 的 Check）。size 复用速率/块大小的同一套单位（K/M/G/KiB…）；省略 size
// 时块大小默认 1（= 旧的纯次数语义，向后兼容）。count=-1 表示无限。

// ParseSpareSpec 解析备用块规格字符串为（块数量, 块大小字节）。接受：
//   - "8*4KiB"/"8*4096" → (8, 4096)
//   - "8"            → (8, 1)（兼容旧的纯次数语义）
//   - "-1"           → (-1, 1)（无限）
//
// count 必须 ≥ -1（-1=无限）；块大小（若给出）必须是 ≥1 的整数字节。无法解析时返回
// [knobParseError]（kind=spare），便于 CLI 展示带上下文的提示。
func ParseSpareSpec(s string) (count, blockSize int64, err error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, 0, &knobParseError{kind: "spare", raw: s, hint: "不能为空；示例：8*4KiB / 8 / -1"}
	}
	var sizeStr string
	hasStar := false
	if idx := strings.Index(t, "*"); idx >= 0 {
		hasStar = true
		sizeStr = t[idx+1:]
		t = t[:idx]
	}
	n, e := parseStrictInt(strings.TrimSpace(t))
	if e != nil {
		return 0, 0, &knobParseError{kind: "spare", raw: s, hint: "块数量须是整数（示例：8*4KiB / 8 / -1）"}
	}
	if n < -1 {
		return 0, 0, &knobParseError{kind: "spare", raw: s, hint: "块数量须 >=0（-1=无限）"}
	}
	if !hasStar {
		return n, 1, nil // 纯数量（兼容旧 <n>），块大小默认 1
	}
	bw, perr := parseBytesFloat(sizeStr)
	if perr != nil {
		return 0, 0, &knobParseError{kind: "spare", raw: s, hint: "块大小解析失败（示例：4KiB / 4096）"}
	}
	if bw < 1 {
		return 0, 0, &knobParseError{kind: "spare", raw: s, hint: "块大小须 >=1 字节"}
	}
	if math.Abs(bw-math.Round(bw)) > 1e-9 {
		return 0, 0, &knobParseError{kind: "spare", raw: s, hint: "块大小须是整数字节"}
	}
	// 有限非负 + 安全取整（含 NaN/Inf/溢出拒绝）共用 finiteNonNegInt64，与 ParseCapacity 同口径。
	bs, err := finiteNonNegInt64("spare", s, bw)
	if err != nil {
		return 0, 0, err
	}
	return n, bs, nil
}

// ---- 格式化（字节数 / 速率 / 块预算 → 人类可读串）----

// formatScaled 把字节数 v 按最短表示选 GiB/MiB/KiB/B 单位阶梯，返回 (数值串, 单位串)。
// [FormatSize]（块大小，int64）与 [FormatSpeed]（速率，float64）共用此单位选择逻辑，
// 避免两份 GiB/MiB/KiB 阈值与后缀列表 copy-paste 漂移。
func formatScaled(v float64) (num, unit string) {
	switch {
	case v >= GiB:
		return trimFloat(v / GiB), "GiB"
	case v >= MiB:
		return trimFloat(v / MiB), "MiB"
	case v >= 1<<10:
		return trimFloat(v / (1 << 10)), "KiB"
	default:
		return trimFloat(v), "B"
	}
}

// FormatSize 把字节数格式化为人类可读的大小串（最短表示）：>=GiB→GiB、>=MiB→MiB、
// >=KiB→KiB、否则字节。用于 spare 块大小的展示。
func FormatSize(b int64) string {
	num, unit := formatScaled(float64(b))
	return num + unit
}

// FormatSpare 把备用块预算（count 个 blockSize 字节的块）格式化为展示串：
// count==-1→"unlimited"；blockSize>1→"N*<size>"（如 8*4KiB，与输入格式一致）；
// 否则"N"。0→"0"。
func FormatSpare(count, blockSize int64) string {
	if count == -1 {
		return "unlimited"
	}
	if blockSize > 1 {
		return strconv.FormatInt(count, 10) + "*" + FormatSize(blockSize)
	}
	return strconv.FormatInt(count, 10)
}

// FormatSpeed 把字节/秒格式化为人类可读的速度串（如 "100MiB/s"、"2.5GiB/s"）。
// 用最短浮点表示（strconv 精度 -1），自动省去末尾多余的 0，且不损失精度。
func FormatSpeed(bw float64) string {
	if bw <= 0 {
		return "unlimited"
	}
	num, unit := formatScaled(bw)
	return num + unit + "/s"
}
