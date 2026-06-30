package faultfs

import (
	"testing"
	"time"
)

// TestDelayReadApplies:随机读延迟确实生效（计时验证）。
func TestDelayReadApplies(t *testing.T) {
	inj := NewInjector()
	inj.SetProfile(LatencyProfile{ReadRand: 30 * time.Millisecond, ReadSeq: 5 * time.Millisecond})
	start := time.Now()
	inj.DelayRead(false, 0) // 随机读
	if d := time.Since(start); d < 25*time.Millisecond {
		t.Fatalf("random read delay = %v, want >=25ms", d)
	}
}

// TestDelaySequentialVsRandom:顺序读用 ReadSeq，比随机（ReadRand）短。
func TestDelaySequentialVsRandom(t *testing.T) {
	inj := NewInjector()
	inj.SetProfile(LatencyProfile{ReadRand: 40 * time.Millisecond, ReadSeq: 5 * time.Millisecond})
	t0 := time.Now()
	inj.DelayRead(true, 0) // 顺序
	seq := time.Since(t0)
	t1 := time.Now()
	inj.DelayRead(false, 0) // 随机
	rand := time.Since(t1)
	if seq >= 20*time.Millisecond {
		t.Fatalf("sequential delay = %v, want <20ms", seq)
	}
	if rand < 30*time.Millisecond {
		t.Fatalf("random delay = %v, want >=30ms", rand)
	}
}

// TestDelaySpeedMultiplier:倍速 2.0 → ~2× 延迟。
func TestDelaySpeedMultiplier(t *testing.T) {
	inj := NewInjector()
	inj.SetProfile(LatencyProfile{ReadRand: 20 * time.Millisecond})
	inj.SetSpeed(2.0)
	start := time.Now()
	inj.DelayRead(false, 0)
	if d := time.Since(start); d < 35*time.Millisecond {
		t.Fatalf("speed 2.0 delay = %v, want >=35ms (2x20)", d)
	}
}

// TestDelayNoneNoSleep:默认 ProfileNone 不应产生可观测延迟。
func TestDelayNoneNoSleep(t *testing.T) {
	inj := NewInjector()
	start := time.Now()
	inj.DelayRead(false, 0)
	inj.DelayWrite(false, 0)
	inj.DelayOp(OpOpen)
	if d := time.Since(start); d > 5*time.Millisecond {
		t.Fatalf("ProfileNone slept %v, want ~0", d)
	}
}

// TestProfileByName:档名解析（含大小写、别名）。
func TestProfileByName(t *testing.T) {
	for _, n := range []string{"none", "", "memory", "tmpfs", "ram", "ssd", "hdd", "disk", "HDD", "SSD"} {
		if _, ok := ProfileByName(n); !ok {
			t.Fatalf("ProfileByName(%q) = false, want true", n)
		}
	}
	if _, ok := ProfileByName("quantum-disk"); ok {
		t.Fatal("unknown profile should return false")
	}
}
