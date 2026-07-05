package faultfs

import (
	"fmt"
	"syscall"
	"time"

	"github.com/ryjer/faultfs/control"
)

// handleControl 是 control server 的请求处理适配器：把 control.Req 翻译成对
// *Injector 的调用并返回 control.Resp。control 包不 import faultfs，故由本包
// 提供此闭包，避免循环依赖。
func handleControl(inj *Injector, meta mountMeta, req control.Req) control.Resp {
	switch req.Cmd {
	case control.CmdAddRule:
		r := Rule{
			Op:          req.Op,
			Path:        req.Path,
			Off:         req.Off,
			OffLen:      req.OffLen,
			Errno:       syscall.Errno(req.Errno),
			N:           req.N,
			HealOnWrite: req.HealOnWrite,
		}
		return control.Resp{OK: true, ID: inj.Add(r)}
	case control.CmdDeleteRule:
		if !inj.Delete(req.ID) {
			return control.Resp{OK: false, Err: fmt.Sprintf("rule id %d does not exist", req.ID)}
		}
		return control.Resp{OK: true}
	case control.CmdClear:
		inj.Clear()
		return control.Resp{OK: true}
	case control.CmdListRules:
		return control.Resp{OK: true, Rules: toControlViews(inj.List())}
	case control.CmdRefreshRules:
		res := inj.Refresh(RefreshOptions{SkipLatency: req.SkipLatency})
		return control.Resp{OK: true, Resets: toResetViews(res.Entries)}
	case control.CmdSetLatency:
		warns, err := setLatency(inj, meta.backing, req)
		if err != nil {
			return control.Resp{OK: false, Err: err.Error()}
		}
		return control.Resp{OK: true, Warns: warns}
	case control.CmdSetSpare:
		if !req.HasSpare {
			return control.Resp{OK: false, Err: "no spare value specified"}
		}
		inj.SetSpareBlocks(req.Spare, req.SpareBlockSize) // blockSize<1 与 count<-1 由 SetSpareBlocks 统一钳制（单一真实来源）
		return control.Resp{OK: true}
	case control.CmdStatus:
		return control.Resp{OK: true, Rules: toControlViews(inj.List()), Profile: profileName(inj.Profile()), Spare: inj.Spare(), SpareBlockSize: inj.SpareBlockSize(), Capacity: inj.Capacity(), Speed: inj.Speed()}
	case control.CmdDump:
		return control.Resp{OK: true, Dump: buildDump(inj, meta)}
	}
	return control.Resp{OK: false, Err: "unknown cmd: " + string(req.Cmd)}
}

// mountMeta 记录一次挂载的元信息，供 dump/status 回传给 CLI。
type mountMeta struct {
	pid       int
	backing   string
	socket    string
	mountTime string // RFC3339
}

// setLatency 处理 set-latency：解析预设档/手动性能旋钮/--speed，按 backing 实测上限
// 钳制后写入 profile，倍速单独写入。返回告警列表（可能为空）与错误（参数非法时）。
// 钳制由 [Injector.SetProfileCalibrated] 统一完成（与库用户共用同一实现）。
// --profile 与 --rand/--seq 互斥（叠加会产生难解释的半覆盖混合 profile）；--speed
// 可与任一组合。
func setLatency(inj *Injector, backing string, req control.Req) ([]string, error) {
	if req.Profile == "" && !req.HasSpeed && !req.HasRand && !req.HasSeq {
		return nil, errf("未指定任何参数；用 --profile / --rand / --seq / --speed 之一")
	}
	// 预设档与手动旋钮互斥：二者叠加时旋钮只覆盖随机/带宽字段，预设的其余字段
	// （如顺序 per-request、带宽）会静默保留，形成既非预设也非旋钮意图的混合 profile。
	// 自定义组合请用库 API ProfileFromKnobs。
	if req.Profile != "" && (req.HasRand || req.HasSeq) {
		return nil, errf("--profile 与 --rand/--seq 互斥：预设档与手动旋钮请二选一")
	}

	var warns []string
	if req.Profile != "" || req.HasRand || req.HasSeq {
		var target LatencyProfile
		switch {
		case req.Profile != "":
			p, ok := ProfileByName(req.Profile)
			if !ok {
				return nil, errf("未知预设档：%q（none/memory/ssd/hdd）", req.Profile)
			}
			target = p
		default:
			// 从零开始用手动旋钮构建：走 ProfileFromKnobs（与 mount --rand/--seq 的
			// buildInjector 同一构造入口，避免两套实现漂移）。未给出的旋钮传 0 = 该维度不启用。
			var randDur time.Duration
			if req.HasRand {
				if req.RandNs < 0 {
					return nil, errf("--rand 不能为负（得到 %d ns）", req.RandNs)
				}
				randDur = time.Duration(req.RandNs)
			}
			var seqBw float64
			if req.HasSeq {
				seqBw = req.SeqBw
			}
			target = ProfileFromKnobs(randDur, seqBw)
		}
		warns = append(warns, inj.SetProfileCalibrated(backing, target)...)
	}

	// 全局倍速（可与 profile 或旋钮并存）。<=0 会被 SetSpeed 钳制为 1.0；这里明示告警，
	// 避免用户想"清零/暂停延迟"却静默得到正常速度（spec/latency.md 注明的既定钳制行为）。
	if req.HasSpeed {
		if req.Speed <= 0 {
			warns = append(warns, fmt.Sprintf("speed %s <= 0 is invalid, treating as 1.0 (normal); use a small positive value to slow down", trimFloat(req.Speed)))
		}
		inj.SetSpeed(req.Speed)
	}
	return warns, nil
}

// errf 是 fmt.Errorf 的简写，避免在本文件多处重复 fmt.Errorf。
func errf(format string, args ...any) error { return fmt.Errorf(format, args...) }

// buildDump 构造一份全量快照：规则 + 挂载元信息 + 完整延迟 profile。
func buildDump(inj *Injector, meta mountMeta) *control.DumpView {
	p := inj.Profile()
	return &control.DumpView{
		Rules:          toControlViews(inj.List()),
		MountPID:       meta.pid,
		Backing:        meta.backing,
		Socket:         meta.socket,
		MountTime:      meta.mountTime,
		ProfileName:    profileName(p),
		Speed:          inj.Speed(),
		Spare:          inj.Spare(),
		SpareBlockSize: inj.SpareBlockSize(),
		Capacity:       inj.Capacity(),
		ProfileFields:  profileFields(p),
	}
}

// toControlViews 把 faultfs.RuleView 列表转成 control 协议的 RuleView。
func toControlViews(vs []RuleView) []control.RuleView {
	out := make([]control.RuleView, len(vs))
	for i, v := range vs {
		out[i] = control.RuleView{
			ID:           v.ID,
			Op:           v.Op,
			Path:         v.Path,
			Off:          v.Off,
			OffLen:       v.OffLen,
			Errno:        int(v.Errno),
			N:            v.N,
			HealOnWrite:  v.HealOnWrite,
			Healed:       v.Healed,
			HealedBlocks: v.HealedBlocks,
			TotalBlocks:  v.TotalBlocks,
			Remaining:    v.Remaining,
		}
	}
	return out
}

// toResetViews 把 faultfs.ResetEntry 列表转成 control 协议的 ResetView。
func toResetViews(es []ResetEntry) []control.ResetView {
	out := make([]control.ResetView, len(es))
	for i, e := range es {
		out[i] = control.ResetView{What: e.What, ID: e.ID, Before: e.Before, After: e.After}
	}
	return out
}
