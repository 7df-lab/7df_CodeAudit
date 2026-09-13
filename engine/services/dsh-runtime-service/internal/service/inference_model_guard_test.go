package service

// R80 锁定测试：路由 model 名写入口白名单——route.Model 会被烘进沙箱 launchScript
// （bash -c），空格即分词断裂（launch 必败难归因）、shell 元字符即沙箱内命令注入。
// 拒绝发生在触达 manager 之前（哨兵断言）。

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSetInferenceRoute_RejectsUnsafeModel(t *testing.T) {
	called := false
	svc := newInferenceSvc(t, func(w http.ResponseWriter, req *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(map[string]any{"provider": "p", "model": "m", "version": 1})
	})
	for _, bad := range []string{
		"x; touch /tmp/pwned",   // shell 元字符注入
		"a b",                   // 空格分词断裂
		"$(curl evil)",          // 命令替换
		"`id`",                  // 反引号
	} {
		_, err := svc.SetInferenceRoute(context.Background(), &pb.SetInferenceRouteRequest{
			Metadata: &pb.RequestMetadata{RequestId: "req-r80-" + bad},
			Provider: "prov", Model: bad,
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("model %q: want InvalidArgument, got %v", bad, err)
		}
		if called {
			t.Fatalf("model %q: 拒绝前触达了 manager", bad)
		}
	}
	// 合法形态放行（走到 manager——哨兵已应答）
	_, err := svc.SetInferenceRoute(context.Background(), &pb.SetInferenceRouteRequest{
		Metadata: &pb.RequestMetadata{RequestId: "req-r80-ok"},
		Provider: "prov", Model: "glm-5.3-flash",
	})
	if err != nil {
		t.Fatalf("合法 model 被误拒: %v", err)
	}
}
