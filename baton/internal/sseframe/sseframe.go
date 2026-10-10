// Package sseframe 解析 .sse.log 原始 SSE 帧流（event:/data: 行 + ": ping" 注释心跳）。
// 与 engine sseParser（services/dsh-runtime-service/internal/sandbox/sandbox.go:705-718）
// 同构：event: 行暂存帧名，data: 行成帧，空行清空暂存；注释行跳过——除
// ": ts <RFC3339Nano>"（R5 帧接收时刻，engine writeRaw 在 event: 行前写入；
// 旧文件无此注释 → Frame.Ts 空，调用方降级投影时刻）。
// 单 data 行假设与 bridge.mjs 发送侧一致（多 data 行拼接不支持，见 docs/projection-dsh.md）。
package sseframe

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// Frame 一帧：event 名 + data 原文（未解析 JSON）。Line 为 data 行号（1 基，告警定位用）。
// Ts 为帧接收时刻原文（": ts" 注释，RFC3339Nano；空=旧文件无时刻）。
type Frame struct {
	Name string
	Data string
	Line int
	Ts   string
}

// Scan 逐帧产出。末行撕裂（data JSON 截断）由调用方用 ValidJSON 裁决——本层不猜。
// ts 注释只作用紧随其后的一个帧（组帧即消费，不留存到后续帧）。
func Scan(r io.Reader, fn func(Frame) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // submit_findings 参数可达数十 KB
	pending := ""
	pendingTs := ""
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.HasPrefix(s, ": ts "):
			pendingTs = strings.TrimPrefix(s, ": ts ")
		case strings.HasPrefix(s, "event: "):
			pending = strings.TrimPrefix(s, "event: ")
		case strings.HasPrefix(s, "data: "):
			f := Frame{Name: pending, Data: strings.TrimPrefix(s, "data: "), Line: line, Ts: pendingTs}
			pending = ""
			pendingTs = ""
			if err := fn(f); err != nil {
				return err
			}
		case s == "":
			pending = ""
			pendingTs = ""
		}
	}
	return sc.Err()
}

// ValidJSON 报告 data 是否完整 JSON——撕裂尾行检测（投影规则 1）。
func ValidJSON(s string) bool {
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}
