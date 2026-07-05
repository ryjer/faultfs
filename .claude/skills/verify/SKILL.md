---
name: verify
description: Build faultfs and drive a real FUSE mount end-to-end to verify a change reaches its runtime surface.
---

# Verify faultfs

faultfs is a FUSE loopback filesystem + CLI. The runtime surface is the **CLI
driving a real mount** — that's what reaches the rule engine, latency model,
and FUSE node code. Tests can `t.Skip` without `/dev/fuse`; do not rely on them
as the surface.

## Build

```bash
go build -o /tmp/faultfs ./cmd/faultfs
```

## Environment

- Needs `/dev/fuse` (rw) + `fusermount3`/`fusermount` on PATH. WSL2 has them.
- Backing dir goes on tmpfs (`/tmp`) — `--capacity N` must be `> tmpfs_used`,
  else `checkCapacityAtMount` rejects with "挂载即满". Probe `df /tmp` first,
  or drop `--capacity` if not verifying the capacity path.

## Drive (end-to-end in ~10s)

Isolate per run: `mktemp -d` for backing + mountpoint.

```bash
BK=$(mktemp -d); MP=$(mktemp -d)
# detach = background daemon; returns once socket ready
/tmp/faultfs mount "$BK" "$MP" --detach --rand 1ms --seq 100M --spare '8*4KiB'
/tmp/faultfs status "$MP"                       # rules/spare/profile overview
ID=$(/tmp/faultfs add "$MP" --op read --path x.txt --errno EIO)   # inject
echo data > "$MP/x.txt"; cat "$MP/x.txt"        # → "Input/output error" (EIO)
/tmp/faultfs add badsector "$MP" --path b.bin --off 0 --len 4096 --spare 4
dd if=/dev/zero of="$MP/b.bin" bs=4096 count=1  # create first (no rule yet)
cat "$MP/b.bin" 2>/dev/null; echo $?            # → 1 (EIO, unhealed)
dd if=/dev/zero of="$MP/b.bin" bs=4096 count=1 conv=notrunc  # write → heals
cat "$MP/b.bin" 2>/dev/null; echo $?            # → 0 (healed)
/tmp/faultfs refresh "$MP"                      # resets healed + spare
/tmp/faultfs dump "$MP" --json | python3 -m json.tool
/tmp/faultfs unmount "$MP"
```

## What each changed file reaches

| File | Reached by |
|---|---|
| `cmd/faultfs/{main,mount,rules,set,status,helpers}.go` | every CLI subcommand |
| `faultfs.go` (mount) + `faultnode.go` + `file.go` + `dir.go` | any read/write/readdir through `$MP` |
| `control.go` (handleControl/setLatency/buildDump) | every client subcommand (control socket) |
| `injector.go`/`rule.go`/`check.go` | add + runtime EIO + heal |
| `capacity.go` | `--capacity` (reject if < used) + write-ENOSPC |
| `state.go` | `set spare` + `refresh` |
| `profile.go`/`calibrate.go`/`delay.go` | `--rand`/`--seq`/`--profile` + every op's sleep |
| `knob.go` | `--rand`/`--seq`/`--spare`/`--capacity` parsing + all Format* |

## Probes worth running

- `set spare -1` → must succeed (unlimited); `set.go` uses `SetInterspersed(false)` so pflag won't eat the leading `-1` as a flag.
- `set spare abc` → structured bilingual `knobParseError`.
- `add --errno NOPE` → `unknown errno`.
- `refresh` then re-read a healed bad-sector file → EIO again (proves Refresh resets runtime state, not just display).

## Gotchas

- A failed first `mount` leaves any files you wrote to the bare mountpoint dir
  visible after a later successful mount is unmounted — harness residue, not a leak.
- ms/µs delay sleeps (`delay.go`) are invoked on every op once a profile is set,
  but aren't directly observable in CLI output; the profile applying (`profile=...`
  in status) is the signal that the path runs.
