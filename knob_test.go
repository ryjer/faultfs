package faultfs

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ryjer/faultfs/control"
)

func TestParseLatency(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"8ms", 8 * time.Millisecond},
		{"200us", 200 * time.Microsecond},
		{"200µs", 200 * time.Microsecond},
		{"100ns", 100 * time.Nanosecond},
		{"5s", 5 * time.Second},
		{"1.5ms", 1500 * time.Microsecond},
		{"5000", 5000 * time.Nanosecond}, // 裸整数 → ns
	}
	for _, c := range cases {
		got, err := ParseLatency(c.in)
		if err != nil {
			t.Errorf("ParseLatency(%q) err=%v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseLatency(%q)=%v want %v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "abc", "8xz"} {
		if _, err := ParseLatency(bad); err == nil {
			t.Errorf("ParseLatency(%q) 期望失败，却成功", bad)
		}
	}
}

func TestParseSpeed(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"100M", 100 * MiB},
		{"2G", 2 * GiB},
		{"100MiB/s", 100 * MiB},
		{"2GiB/s", 2 * GiB},
		{"512K", 512 * 1024},
		{"1000", 1000}, // 裸数字 → B/s
		{"1g", 1 * GiB},
	}
	for _, c := range cases {
		got, err := ParseSpeed(c.in)
		if err != nil {
			t.Errorf("ParseSpeed(%q) err=%v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSpeed(%q)=%v want %v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "abc", "8Z", "-100M"} {
		if _, err := ParseSpeed(bad); err == nil {
			t.Errorf("ParseSpeed(%q) 期望失败，却成功", bad)
		}
	}
}

func TestBwByteRoundTrip(t *testing.T) {
	// per-byte 延迟以整数纳秒存储（time.Duration），故只有 per-byte >= 数十 ns 的带宽
	// （HDD 档量级，带宽限制才显著）能精确往返；>~1GiB/s 的 per-byte <1ns 会被量化为 0
	// （=不限速，实际由 backing 封顶），这是带宽模型的已知精度边界。
	for _, bw := range []float64{1 * MiB, 5 * MiB, 10 * MiB} {
		d := bwToByteDur(bw)
		back := byteDurToBw(d)
		rel := (back - bw) / bw
		if rel > 0.02 || rel < -0.02 {
			t.Errorf("bw %v 往返得 %v（相对误差 %.4f，应 <2%%）", bw, back, rel)
		}
	}
	if d := bwToByteDur(0); d != 0 {
		t.Errorf("bwToByteDur(0)=%v want 0", d)
	}
	if bw := byteDurToBw(0); bw != 0 {
		t.Errorf("byteDurToBw(0)=%v want 0", bw)
	}
	// 超高带宽量化为 0（不限速）：2GiB/s 的 per-byte <1ns。
	if d := bwToByteDur(2 * GiB); d != 0 {
		t.Errorf("bwToByteDur(2GiB)=%v want 0（per-byte<1ns 量化）", d)
	}
}

func TestFormatSpeed(t *testing.T) {
	if s := FormatSpeed(0); s != "unlimited" {
		t.Errorf("FormatSpeed(0)=%q want unlimited", s)
	}
	if s := FormatSpeed(100 * MiB); s != "100MiB/s" {
		t.Errorf("FormatSpeed(100MiB)=%q want 100MiB/s", s)
	}
	if s := FormatSpeed(2 * GiB); s != "2GiB/s" {
		t.Errorf("FormatSpeed(2GiB)=%q want 2GiB/s", s)
	}
}

func TestProfileFromKnobs(t *testing.T) {
	p := ProfileFromKnobs(8*time.Millisecond, 100*MiB)
	if p.ReadRand != 8*time.Millisecond || p.WriteRand != 8*time.Millisecond {
		t.Errorf("rand 读/写未设置：%v/%v", p.ReadRand, p.WriteRand)
	}
	if p.Open != 8*time.Millisecond {
		t.Errorf("元数据 op 应=rand：%v", p.Open)
	}
	// per-byte ≈ 1s/(100MiB) ≈ 9.5ns
	want := bwToByteDur(100 * MiB)
	if p.ReadByte != want || p.WriteByte != want {
		t.Errorf("per-byte 读/写=%v/%v want≈%v", p.ReadByte, p.WriteByte, want)
	}
	if p.ReadSeq != 0 || p.WriteSeq != 0 {
		t.Errorf("顺序 per-request 应为 0（带宽主导）：%v/%v", p.ReadSeq, p.WriteSeq)
	}
}

func TestAdjustProfileClamps(t *testing.T) {
	// 新语义：rand 是叠加增量（不减 backing、不告警）；seq 是限制上限（目标带宽 > backing 时钳到 backing 并告警）。
	target := ProfileFromKnobs(8*time.Millisecond, 100*MiB)
	adj, warns := AdjustProfile(target, 5*GiB)
	if len(warns) != 0 {
		t.Errorf("rand 增量 + seq 慢于 backing 不应告警，得 %v", warns)
	}
	// rand 透传不减 backing（增量语义）。
	if adj.ReadRand != 8*time.Millisecond || adj.WriteRand != 8*time.Millisecond {
		t.Errorf("ReadRand/WriteRand 应透传不减 backing：%v/%v", adj.ReadRand, adj.WriteRand)
	}
	// 元数据 op（由 rand 派生）也不减 backing。
	if adj.Open != 8*time.Millisecond {
		t.Errorf("Open 应透传不减 backing：%v", adj.Open)
	}
	// seq 限制：想限到 100MiB/s（per-byte 9.5ns），backing 能 5GiB/s（0.19ns），sleep = 9.5-0.19 ≈ 9.3ns >0。
	if adj.ReadByte <= 0 {
		t.Errorf("ReadByte 不应被钳到 0：%v", adj.ReadByte)
	}

	// seq 目标带宽 > backing（想限到的速度比 backing 还快）：rand 仍透传，seq 钳到 backing 并告警。
	// 用 ≤1GiB/s 量级带宽（per-byte ≥1ns 可精确表达）；>1GiB/s 的 per-byte <1ns 会被量化为 0。
	fast := ProfileFromKnobs(1*time.Nanosecond, 500*MiB) // rand=1ns 增量，seq=500MiB/s（快于 backing 100MiB/s）
	adj2, warns2 := AdjustProfile(fast, 100*MiB)
	// rand 1ns 增量合法，透传不减、不告警。
	if adj2.ReadRand != 1*time.Nanosecond {
		t.Errorf("rand 增量应透传不减 backing：%v", adj2.ReadRand)
	}
	// seq 100GiB > backing 1GiB → 钳到 0（实际取 backing）并告警。
	if adj2.ReadByte != 0 {
		t.Errorf("ReadByte 应钳到 0（目标带宽超出 backing）：%v", adj2.ReadByte)
	}
	if len(warns2) == 0 {
		t.Errorf("seq 目标快于 backing 应告警")
	}
	// 告警只来自 seq（rand 是增量，永不告警）。
	for _, w := range warns2 {
		if strings.Contains(w, "随机") {
			t.Errorf("rand 增量不应告警，得 %q", w)
		}
	}

	// backing 未校准（measuredBw<=0）：seq 透传不钳；rand 透传。
	adj3, warns3 := AdjustProfile(target, 0)
	if len(warns3) != 0 || adj3.ReadRand != target.ReadRand {
		t.Errorf("未校准应透传不钳：%v / %v", warns3, adj3.ReadRand)
	}
}

func TestCalibrateMeasuresBacking(t *testing.T) {
	// 校准直接在 backing 目录上做实测；期望随机延迟 > 0、顺序带宽 > 0。
	dir := t.TempDir()
	rand, bw, err := Calibrate(dir)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if rand <= 0 {
		t.Errorf("实测随机寻址延迟应 > 0：%v", rand)
	}
	if bw <= 0 {
		t.Errorf("实测顺序带宽应 > 0：%v", bw)
	}
	// 残留校准文件应被清理。
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("校准后 backing 应无残留文件，得 %d 项", len(ents))
	}
}

// TestParseLatencyRejectsNegative:负延迟会让 sleepFor 静默当作不延迟（"要慢却变快"），
// 故须显式拒绝。
func TestParseLatencyRejectsNegative(t *testing.T) {
	for _, bad := range []string{"-8ms", "-100", "-5s"} {
		if _, err := ParseLatency(bad); err == nil {
			t.Errorf("ParseLatency(%q) 期望失败（负值），却成功", bad)
		}
	}
}

// TestParseLatencyEmptyHint:空串应给出带 kind/示例的结构化错误，而非裸 "空值"。
func TestParseLatencyEmptyHint(t *testing.T) {
	_, err := ParseLatency("")
	if err == nil {
		t.Fatal("ParseLatency(\"\") 期望失败")
	}
	if _, ok := err.(*knobParseError); !ok {
		t.Errorf("空串应返回 *knobParseError（带上下文），得 %T", err)
	}
}

// TestParseSpeedValidation:NaN/Inf 与过小正带宽（<1 B/s，per-byte 延迟会溢出或挂死）须拒绝；
// 0 合法（=不限速）。
func TestParseSpeedValidation(t *testing.T) {
	for _, bad := range []string{"NaN", "nan", "Inf", "inf", "1e-10", "0.5", "0.0001"} {
		if _, err := ParseSpeed(bad); err == nil {
			t.Errorf("ParseSpeed(%q) 期望失败，却成功", bad)
		}
	}
	// 0（含带单位）= 不限速，合法。
	for _, z := range []string{"0", "0M", "0G"} {
		got, err := ParseSpeed(z)
		if err != nil || got != 0 {
			t.Errorf("ParseSpeed(%q) = %v,%v；want 0（不限速）", z, got, err)
		}
	}
	// 1 B/s 是允许的下限；0.5K = 512 B/s 合法。
	if got, _ := ParseSpeed("1"); got != 1 {
		t.Errorf("ParseSpeed(\"1\") = %v，want 1", got)
	}
	if got, _ := ParseSpeed("0.5K"); got != 512 {
		t.Errorf("ParseSpeed(\"0.5K\") = %v，want 512", got)
	}
}

// TestBwToByteDurOverflowSafe:极慢带宽的 per-byte 延迟不得回绕成负（否则 sleepFor
// 静默不延迟）。应钳到最大正 Duration。
func TestBwToByteDurOverflowSafe(t *testing.T) {
	if d := bwToByteDur(1e-10); d <= 0 {
		t.Errorf("bwToByteDur(1e-10) = %v，应钳到正 Duration（非负），避免回绕", d)
	}
	if d := bwToByteDur(0); d != 0 {
		t.Errorf("bwToByteDur(0) = %v，want 0", d)
	}
	if d := bwToByteDur(-1); d != 0 {
		t.Errorf("bwToByteDur(-1) = %v，want 0", d)
	}
}

// TestAddByteDelayOverflowSafe:perByte × n 溢出时不得回绕成负。
func TestAddByteDelayOverflowSafe(t *testing.T) {
	got := addByteDelay(0, 10*time.Second, 1<<55) // 1e10ns × ~3.6e16 → 溢出
	if got <= 0 {
		t.Errorf("溢出的 per-byte 延迟回绕成非正：%v（应钳到正）", got)
	}
	if got := addByteDelay(5*time.Millisecond, 0, 100); got != 5*time.Millisecond {
		t.Errorf("perByte=0 不应叠加：%v", got)
	}
}

// TestFormatSpeedFractional:FormatSpeed 用最短浮点表示，2.5GiB/s 不被多余小数位污染。
func TestFormatSpeedFractional(t *testing.T) {
	if s := FormatSpeed(2.5 * GiB); s != "2.5GiB/s" {
		t.Errorf("FormatSpeed(2.5GiB)=%q want 2.5GiB/s", s)
	}
}

// TestAdjustProfileClampsMetadata:元数据 op 由 rand 派生（见 applyRandLatency）。新语义下
// rand 是叠加增量，元数据 op 也透传不减 backing（不再"快于 backing 时钳到 0"）。
func TestAdjustProfileClampsMetadata(t *testing.T) {
	fast := ProfileFromKnobs(1*time.Nanosecond, 0) // rand=1ns 增量
	adj, _ := AdjustProfile(fast, 0)
	// rand 1ns 增量合法，元数据 op = 1ns（不减 backing，不钳到 0）。
	for name, d := range map[string]time.Duration{
		"Open": adj.Open, "Getattr": adj.Getattr, "Create": adj.Create, "Statfs": adj.Statfs,
	} {
		if d != 1*time.Nanosecond {
			t.Errorf("%s 应透传 rand 增量 1ns（不减 backing）：%v", name, d)
		}
	}

	slow := ProfileFromKnobs(8*time.Millisecond, 0)
	adj2, warns := AdjustProfile(slow, 0)
	if adj2.Open != 8*time.Millisecond {
		t.Errorf("Open 应透传 8ms 增量（不减 backing）：%v", adj2.Open)
	}
	if len(warns) != 0 {
		t.Errorf("rand 增量不应告警：%v", warns)
	}
}

// TestSetProfileCalibratedRandOnlySkipsCalib:rand-only profile（无带宽字段）跳过 backing 校准，
// 直接写入、无告警（rand 是叠加增量，不需 backing 上限来判定）。
func TestSetProfileCalibratedRandOnlySkipsCalib(t *testing.T) {
	inj := NewInjector()
	backing := t.TempDir()
	target := ProfileFromKnobs(8*time.Millisecond, 0) // rand-only
	warns := inj.SetProfileCalibrated(backing, target)
	if len(warns) != 0 {
		t.Errorf("rand-only 不校准、不应告警：%v", warns)
	}
	p := inj.Profile()
	if p.ReadRand != 8*time.Millisecond {
		t.Errorf("ReadRand = %v, want 8ms（增量透传）", p.ReadRand)
	}
	// 未触发 backing 校准（CalibratedFloor 未就绪）。
	if _, _, ok := inj.CalibratedFloor(); ok {
		t.Errorf("rand-only 不应触发 backing 校准")
	}
}

// TestSetLatencyValidation:set-latency 的参数校验：profile×knob 互斥、负 rand 拒绝、
// 无参数拒绝；profile+speed、单独 --rand 合法。
func TestSetLatencyValidation(t *testing.T) {
	inj := NewInjector()
	backing := t.TempDir()

	if _, err := setLatency(inj, backing, control.Req{Profile: "hdd", HasRand: true, RandNs: 8_000_000}); err == nil {
		t.Fatal("profile + --rand 应互斥报错")
	}
	if _, err := setLatency(inj, backing, control.Req{HasRand: true, RandNs: -1}); err == nil {
		t.Fatal("负 --rand 应报错")
	}
	if _, err := setLatency(inj, backing, control.Req{Profile: "hdd", HasSeq: true, SeqBw: 100}); err == nil {
		t.Fatal("profile + --seq 应互斥报错")
	}
	if _, err := setLatency(inj, backing, control.Req{}); err == nil {
		t.Fatal("无参数应报错")
	}
	if _, err := setLatency(inj, backing, control.Req{Profile: "xyz"}); err == nil {
		t.Fatal("未知预设档应报错")
	}
	// 合法组合。
	if _, err := setLatency(inj, backing, control.Req{Profile: "ssd", HasSpeed: true, Speed: 2}); err != nil {
		t.Fatalf("profile + speed 应成功：%v", err)
	}
	if _, err := setLatency(inj, backing, control.Req{HasRand: true, RandNs: 8_000_000}); err != nil {
		t.Fatalf("仅 --rand 应成功（触发 backing 校准+钳制）：%v", err)
	}
}

// TestParseSpareSpec:备用块规格解析（count*size 与纯 count），含单位、负值、边界与非法。
func TestParseSpareSpec(t *testing.T) {
	cases := []struct {
		in        string
		count     int64
		blockSize int64
	}{
		{"8*4KiB", 8, 4096},
		{"8*4096", 8, 4096},
		{"8*4K", 8, 4096},
		{"16*1MiB", 16, 1 << 20},
		{"8", 8, 1},   // 纯数量 → 块大小默认 1（兼容旧语义）
		{"0", 0, 1},   // 默认 0
		{"-1", -1, 1}, // 无限
		{"-1*4KiB", -1, 4096},
		{"1*1", 1, 1},
	}
	for _, c := range cases {
		count, bs, err := ParseSpareSpec(c.in)
		if err != nil {
			t.Errorf("ParseSpareSpec(%q) err=%v", c.in, err)
			continue
		}
		if count != c.count || bs != c.blockSize {
			t.Errorf("ParseSpareSpec(%q) = (%d,%d), want (%d,%d)", c.in, count, bs, c.count, c.blockSize)
		}
	}
	for _, bad := range []string{"", "abc", "8*", "*4KiB", "8*0", "8*-1", "8*abc", "--1", "8*0.5", "8*1.5", "-2", "-2*4KiB", "8*99999999999999999999"} {
		if _, _, err := ParseSpareSpec(bad); err == nil {
			t.Errorf("ParseSpareSpec(%q) 期望失败，却成功", bad)
		}
	}
	if _, _, err := ParseSpareSpec("8*0"); err == nil {
		t.Fatal("ParseSpareSpec(\"8*0\") 期望失败")
	} else if _, ok := err.(*knobParseError); !ok {
		t.Errorf("ParseSpareSpec 非法应返回 *knobParseError，得 %T", err)
	}
}

// TestFormatSpareSize:FormatSpare / FormatSize 展示。
func TestFormatSpareSize(t *testing.T) {
	if s := FormatSize(4096); s != "4KiB" {
		t.Errorf("FormatSize(4096)=%q want 4KiB", s)
	}
	if s := FormatSize(1 << 20); s != "1MiB" {
		t.Errorf("FormatSize(1MiB)=%q want 1MiB", s)
	}
	if s := FormatSize(512); s != "512B" {
		t.Errorf("FormatSize(512)=%q want 512B", s)
	}
	if s := FormatSpare(-1, 1); s != "unlimited" {
		t.Errorf("FormatSpare(-1,1)=%q want unlimited", s)
	}
	if s := FormatSpare(8, 4096); s != "8*4KiB" {
		t.Errorf("FormatSpare(8,4096)=%q want 8*4KiB", s)
	}
	if s := FormatSpare(0, 1); s != "0" {
		t.Errorf("FormatSpare(0,1)=%q want 0", s)
	}
	if s := FormatSpare(4, 1); s != "4" {
		t.Errorf("FormatSpare(4,1)=%q want 4", s)
	}
}

// TestParseCapacity:容量字符串解析（单位/裸数字/负值/非法）。
func TestParseCapacity(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"100M", int64(100 * MiB)},
		{"1G", int64(1 * GiB)},
		{"100MiB", int64(100 * MiB)},
		{"2GiB", int64(2 * GiB)},
		{"512K", 512 * 1024},
		{"1000", 1000}, // 裸数字 → 字节
		{"0", 0},
	}
	for _, c := range cases {
		got, err := ParseCapacity(c.in)
		if err != nil {
			t.Errorf("ParseCapacity(%q) err=%v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseCapacity(%q)=%d want %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "abc", "-100M", "8Z"} {
		if _, err := ParseCapacity(bad); err == nil {
			t.Errorf("ParseCapacity(%q) 期望失败，却成功", bad)
		}
	}
	if _, err := ParseCapacity(""); err == nil {
		t.Fatal("ParseCapacity(\"\") 期望失败")
	} else if _, ok := err.(*knobParseError); !ok {
		t.Errorf("ParseCapacity 非法应返回 *knobParseError，得 %T", err)
	}
}
