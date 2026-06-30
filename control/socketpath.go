package control

import (
	"encoding/hex"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
)

// SocketPath 返回挂载点 mp 对应的控制 socket 路径：优先 $XDG_RUNTIME_DIR/faultfs/
// <hash(mp)>.sock，回退到 /tmp/faultfs-<uid>/<hash(mp)>.sock。同一 mp 稳定映射到
// 同一路径，便于客户端按挂载点定位。
func SocketPath(mp string) string {
	h := fnv.New64a()
	h.Write([]byte(mp))
	name := hex.EncodeToString(h.Sum(nil))[:16] + ".sock"
	if r := os.Getenv("XDG_RUNTIME_DIR"); r != "" {
		return filepath.Join(r, "faultfs", name)
	}
	return filepath.Join(os.TempDir(), "faultfs-"+strconv.Itoa(os.Getuid()), name)
}
