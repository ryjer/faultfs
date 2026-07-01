package faultfs

import (
	"os"
	"testing"
	"time"
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
	// 目标慢于 backing：不钳、不告警。
	target := ProfileFromKnobs(8*time.Millisecond, 100*MiB)
	adj, warns := AdjustProfile(target, 1*time.Microsecond, 5*GiB)
	if len(warns) != 0 {
		t.Errorf("目标慢于 backing 不应告警，得 %v", warns)
	}
	// 8ms - 1µs ≈ 7.999ms
	if adj.ReadRand >= 8*time.Millisecond || adj.ReadRand < 7*time.Millisecond {
		t.Errorf("ReadRand 钳制异常：%v", adj.ReadRand)
	}
	// per-byte: target(100MiB→9.5ns) - backing(5GiB→0.19ns) ≈ 9.3ns，仍 >0
	if adj.ReadByte <= 0 {
		t.Errorf("ReadByte 不应被钳到 0：%v", adj.ReadByte)
	}

	// 目标快于 backing：钳到 0 并告警。
	fast := ProfileFromKnobs(1*time.Nanosecond, 100*GiB) // 1ns rand, 100GiB/s
	adj2, warns2 := AdjustProfile(fast, 1*time.Microsecond, 1*GiB)
	if len(warns2) == 0 {
		t.Errorf("目标快于 backing 应告警")
	}
	if adj2.ReadRand != 0 {
		t.Errorf("ReadRand 应钳到 0（目标快于 backing）：%v", adj2.ReadRand)
	}
	if adj2.ReadByte != 0 {
		t.Errorf("ReadByte 应钳到 0（目标带宽超出 backing）：%v", adj2.ReadByte)
	}

	// backing 未校准（measured<=0）：透传不钳。
	adj3, warns3 := AdjustProfile(target, 0, 0)
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
