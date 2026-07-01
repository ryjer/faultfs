package faultfs

import (
	"errors"
	"strconv"
	"strings"
)

// errEmptyKnob 表示手动性能旋钮（--rand/--seq）给了空字符串。
var errEmptyKnob = errors.New("空值")

// knobParseError 是手动性能旋钮解析失败的错误，携带 kind/raw/hint 便于 CLI 展示。
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

// trimZeros 去掉浮点字符串末尾多余的 0（保留 1 位小数含义），如 "100.0"→"100"、"2.50"→"2.5"。
func trimZeros(f float64) string {
	s := strconv.FormatFloat(f, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	return s
}
