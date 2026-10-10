package sseframe

import (
	"strings"
	"testing"
)

func TestScanBasicShapes(t *testing.T) {
	in := strings.Join([]string{
		"event: bridge.hello",
		`data: {"provider":"dsh","model":"deepseek-chat"}`,
		"",
		": ping",
		"",
		"event: session.status",
		`data: {"sessionId":"main","status":"idle"}`,
		"",
	}, "\n")
	var got []Frame
	if err := Scan(strings.NewReader(in), func(f Frame) error { got = append(got, f); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("frames = %d, want 2（注释/空行不成帧）", len(got))
	}
	if got[0].Name != "bridge.hello" || got[0].Line != 2 {
		t.Fatalf("frame0 = %+v", got[0])
	}
	if got[1].Name != "session.status" || got[1].Line != 7 {
		t.Fatalf("frame1 = %+v", got[1])
	}
}

func TestScanTornTail(t *testing.T) {
	in := "event: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"ty"
	var got []Frame
	if err := Scan(strings.NewReader(in), func(f Frame) error { got = append(got, f); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("torn 帧仍应产出（裁决归调用方），got %d", len(got))
	}
	if ValidJSON(got[0].Data) {
		t.Fatal("撕裂 data 不应通过 ValidJSON")
	}
}

// R5 帧时间戳注释：解析、只作用下一帧、旧文件缺失为空。
func TestScanTsAnnotation(t *testing.T) {
	in := strings.Join([]string{
		": ts 2026-09-17T10:00:01.5Z",
		"event: bridge.hello",
		`data: {"model":"m"}`,
		"",
		"event: session.status", // 无 ts 注释（旧协议帧）
		`data: {"status":"idle"}`,
		"",
		": ping",
		": ts 2026-09-17T10:00:02Z",
		"event: subagent.started",
		`data: {"childSessionId":"c1"}`,
		"",
	}, "\n")
	var got []Frame
	if err := Scan(strings.NewReader(in), func(f Frame) error { got = append(got, f); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("frames = %d, want 3", len(got))
	}
	if got[0].Ts != "2026-09-17T10:00:01.5Z" {
		t.Errorf("帧0 ts 应取注释: %q", got[0].Ts)
	}
	if got[1].Ts != "" {
		t.Errorf("无注释帧 ts 应为空（旧协议降级）: %q", got[1].Ts)
	}
	if got[2].Ts != "2026-09-17T10:00:02Z" {
		t.Errorf("ping 后注释应作用于下一帧: %q", got[2].Ts)
	}
}

// TestScanOverlongLine — 评审缺口 G4 修复：16MiB 行上限行为显式锁定
// （Scanner ErrTooLong → Scan 返回错误，不静默截断；api-external §4 边界）。
func TestScanOverlongLine(t *testing.T) {
	huge := "data: " + strings.Repeat("x", 17*1024*1024) + "\n"
	var gotErr error
	err := Scan(strings.NewReader(huge), func(Frame) error { return nil })
	if err == nil {
		t.Fatalf("超限行必须报错不静默（gotErr=%v, err=%v）", gotErr, err)
	}
}
