package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/ryjer/faultfs"
	"github.com/ryjer/faultfs/control"
)

// ---- 跨命令共享管道：control client + errno 映射 + 展示助手 ----

// sendCtl 发请求到 mp 的 control socket；返回响应或在失败/!OK 时返回错误。
func sendCtl(mp string, req control.Req) (*control.Resp, error) {
	resp, err := control.Send(control.SocketPath(mp), req)
	if err != nil {
		return nil, fmt.Errorf("control socket %s: %w（mount 未运行或未就绪?）", control.SocketPath(mp), err)
	}
	if !resp.OK {
		return &resp, fmt.Errorf("%s", resp.Err)
	}
	return &resp, nil
}

// errnoNames 是 syscall.Errno → 名称的映射，可作为 parseErrno 和 errnoName 的
// 单一真实来源。添加新 errno 时只需更新此 map。
var errnoNames = map[syscall.Errno]string{
	syscall.EIO:        "EIO",
	syscall.ENOSPC:     "ENOSPC",
	syscall.EROFS:      "EROFS",
	syscall.ESTALE:     "ESTALE",
	syscall.ENODEV:     "ENODEV",
	syscall.EUCLEAN:    "EUCLEAN",
	syscall.EACCES:     "EACCES",
	syscall.EPERM:      "EPERM",
	syscall.ENOSYS:     "ENOSYS",
	syscall.EFBIG:      "EFBIG",
	syscall.EDQUOT:     "EDQUOT",
	syscall.ENODATA:    "ENODATA",    // xattr：属性不存在（getxattr/removexattr）
	syscall.EOPNOTSUPP: "EOPNOTSUPP", // xattr：不支持（filesystem/namespce）
	syscall.ERANGE:     "ERANGE",     // xattr：缓冲过小（getxattr/listxattr）
	syscall.E2BIG:      "E2BIG",      // xattr：属性名/值过大
}

// nameToErrno 在 init 中由 errnoNames 自动构建。
var nameToErrno map[string]syscall.Errno

func init() {
	nameToErrno = make(map[string]syscall.Errno, len(errnoNames))
	for e, n := range errnoNames {
		nameToErrno[n] = e
	}
	// ENOTSUP 与 EOPNOTSUPP 在 Linux 同值；errnoNames 只保留 EOPNOTSUPP（显示用），
	// 这里补 ENOTSUP 作为解析别名，让 xattr "not supported" 场景两种写法都被接受。
	nameToErrno["ENOTSUP"] = syscall.EOPNOTSUPP
}

// parseErrno 把 errno 名（EIO/ENOSPC/...）或数字字符串转 syscall.Errno。无法解析时返回错误。
func parseErrno(s string) (syscall.Errno, error) {
	trimmed := strings.TrimSpace(s)
	if n, err := strconv.Atoi(trimmed); err == nil {
		return syscall.Errno(n), nil
	}
	if e, ok := nameToErrno[strings.ToUpper(trimmed)]; ok {
		return e, nil
	}
	return 0, fmt.Errorf("unknown errno: %q", s)
}

// errnoName 反查常见 errno 数字对应的名称；未知返回 "?"。数据来源为 [errnoNames] map。
func errnoName(n int) string {
	if name, ok := errnoNames[syscall.Errno(n)]; ok {
		return name
	}
	return "?"
}

// ---- 展示助手 ----

// biHelp 把英文/中文两段帮助文案拼成"英文一行 + 换行 + 中文一行"，供 cobra Short 与
// flag 描述使用：-h 输出先显示英文，再新起一行显示中文。flag 描述里的换行会被 pflag
// 自动缩进对齐到描述列；Short 在命令列表里换行后第二行不缩进（cobra 模板如此），可接受。
func biHelp(en, zh string) string {
	return en + "\n" + zh
}

// writeJSON 以 2 空格缩进把 v 编码到 stdout。
func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// sortedKeys 返回 map 的键排序后的切片，便于确定性输出。
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// formatCapacity 把模拟容量格式化为展示串：<=0（未启用）→ "unlimited"，否则用 FormatSize。
func formatCapacity(cap int64) string {
	if cap <= 0 {
		return "unlimited"
	}
	return faultfs.FormatSize(cap)
}

// formatHealed 把规则的治愈状态格式化为展示串：非 HealOnWrite → "n/a"；HealOnWrite → "N/M"
// （已治愈块数/总块数）。按块模式 N=已治愈网格块、M=网格块总数；整段/回退模式 M=1（故显示
// "0/1" 或 "1/1"）。List() 对所有 HealOnWrite 规则都填 TotalBlocks>=1，故无需 bool 兜底。
func formatHealed(r control.RuleView) string {
	if !r.HealOnWrite {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d", r.HealedBlocks, r.TotalBlocks)
}
