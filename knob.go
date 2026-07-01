package faultfs

import (
	"strconv"
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
