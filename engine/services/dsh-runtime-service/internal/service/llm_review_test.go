// 判定解析回归（ADR-175）：parseVerdictJSON 是 4a 沙箱回合最终消息的唯一解析器。
// 直连链（llmReviewWith/aiNativeDiscovery）已随 ai-inference 删除，用例同步移除。
package service

import (
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

func TestParseVerdictJSON(t *testing.T) {
	cases := []struct {
		name, in string
		want     pb.AIVerdict
		wantConf float32
	}{
		{"裸JSON", `{"verdict":"TRUE_POSITIVE","confidence":0.9,"reason":"r"}`, pb.AIVerdict_AI_VERDICT_TRUE_POSITIVE, 0.9},
		{"栅栏包裹", "```json\n{\"verdict\":\"FALSE_POSITIVE\",\"confidence\":0.8,\"reason\":\"x\"}\n```", pb.AIVerdict_AI_VERDICT_FALSE_POSITIVE, 0.8},
		{"未知verdict保守UNCERTAIN", `{"verdict":"MAYBE","confidence":0.5}`, pb.AIVerdict_AI_VERDICT_UNCERTAIN, 0.5},
		{"非JSON", "我觉得有问题", pb.AIVerdict_AI_VERDICT_UNCERTAIN, -1}, // want nil
	}
	for _, c := range cases {
		v := parseVerdictJSON(c.in)
		if c.wantConf < 0 {
			if v != nil {
				t.Fatalf("%s: want nil, got %+v", c.name, v)
			}
			continue
		}
		if v == nil {
			t.Fatalf("%s: nil", c.name)
		}
		if v.Verdict != c.want {
			t.Fatalf("%s: verdict=%v want=%v", c.name, v.Verdict, c.want)
		}
		if v.Confidence != c.wantConf {
			t.Fatalf("%s: conf=%v want=%v", c.name, v.Confidence, c.wantConf)
		}
	}
}
