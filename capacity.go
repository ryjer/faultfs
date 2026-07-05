package faultfs

import (
	"syscall"
	"time"
)

// usedCacheTTL 是 backing 已用字节数（statfs）缓存的有效期。容量判定在每个 Write 上触发，
// 若每次都 statfs 会在高 IOPS 下成为热点（statfs 取 superblock 全局锁）。缓存把 statfs 频率
// 限制到 ~100/秒；10ms 级陈旧对"磁盘满"这种粗粒度模拟可忽略。
const usedCacheTTL = 10 * time.Millisecond

// SetCapacity 设模拟容量上限（字节）；<0 钳到 0（=未启用）。capacity 是 mount 时固化的
// 设备属性，不被消耗、不进 Refresh（改值需重新挂载）。挂载时由 [Mount] 按 backing statfs
// 校验 capacity∈(backing已用, backing总量)；运行时 write 超容量返 ENOSPC、statfs 反映
// 模拟容量（见 spec/capacity.md）。0=不限制（直透 backing 真实容量）。
func (in *Injector) SetCapacity(capacity int64) {
	if capacity < 0 {
		capacity = 0
	}
	in.capacity.Store(capacity)
}

// Capacity 返回模拟容量上限（字节）；0=未启用。atomic 读，可在 Write 热路径无锁调用。
func (in *Injector) Capacity() int64 {
	return in.capacity.Load()
}

// backingStatfsUsed/Total 从 syscall.Statfs_t 推算 backing 已用/总量（字节）。按 f_frsize
// （基本块大小）换算——statfs 的 Blocks/Bfree 以 f_frsize 为单位，而非 f_bsize（"优选传输
// 块"）；二者在 tmpfs/ext4 等通常相等，但部分 fs/配置不同时只有 frsize 正确。负值钳 0
// （理论上 Bfree<=Blocks，钳制为防御）。三处容量判定共用此口径，避免漂移。
func backingStatfsUsed(sf *syscall.Statfs_t) int64 {
	used := (int64(sf.Blocks) - int64(sf.Bfree)) * int64(sf.Frsize)
	if used < 0 {
		used = 0
	}
	return used
}

func backingStatfsTotal(sf *syscall.Statfs_t) int64 {
	total := int64(sf.Blocks) * int64(sf.Frsize)
	if total < 0 {
		total = 0
	}
	return total
}

// capacityUsed 返回 backing 已用字节数的运行估值，供容量判定读。TTL 内随每条放行的 write
// 乐观累加（见 checkWriteCapacity），使一个在 TTL 窗口（~10ms）内完成的突发写仍会逼近 capacity
// 而被拦下——纯 TTL 缓存会让突发写一直读到旧 used 而全部放行、超额。TTL 到期时用真实 statfs
// 重新对齐，修正乐观累加对覆盖写/短写/外部写入的漂移。statfs 失败沿用上次估值（fail-open）。
func (in *Injector) capacityUsed(backing string) int64 {
	now := time.Now().UnixNano()
	if now-in.usedCachedAt.Load() < int64(usedCacheTTL) {
		return in.usedCached.Load()
	}
	var sf syscall.Statfs_t
	if err := syscall.Statfs(backing, &sf); err != nil {
		return in.usedCached.Load() // fail-open：沿用上次估值（或 0）
	}
	used := backingStatfsUsed(&sf)
	in.usedCached.Store(used) // 重新对齐到真实值
	in.usedCachedAt.Store(now)
	return used
}

// checkWriteCapacity 按模拟容量上限判定本次写是否放行：cap<=0（未启用）或 n<=0 →放行；
// 否则取估值（capacityUsed），n > cap-used → ENOSPC；放行则把 n 乐观计入 usedCached，使后续
// 写看到递增的已用、突发写能逼近 capacity 被拦下（TTL 到期由 statfs 重对齐修正）。保守近似：
// 覆盖写也按请求字节计，仅在接近满时触发。原子读 capacity + 估值，热路径无锁、不每 write statfs。
// 设备级判定，与规则注入独立——见 [FaultFile.Write] 在 Check 之前调用，保证规则治愈等副作用
// 不会在随后判定 ENOSPC 时已落盘（heal-then-ENOSPC 原子性）。
func (in *Injector) checkWriteCapacity(backing string, n int64) syscall.Errno {
	cap := in.capacity.Load()
	if cap <= 0 || n <= 0 {
		return 0
	}
	if n > cap-in.capacityUsed(backing) {
		return syscall.ENOSPC
	}
	in.usedCached.Add(n) // 乐观累计：本次 write 将落地 ~n 字节
	return 0
}
