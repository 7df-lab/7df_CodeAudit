package service

import (
	"context"
	"testing"
	"time"
)

func TestRedisIdemRoundtrip(t *testing.T) {
	// 内部钉值缺省（发布清源纪律：测试字面量钉内部值），env 可覆盖供跨环境复跑。
	addr := envOr("CODEAUDIT_IDEM_REDIS_ADDR", "192.0.2.122:16379")
	st, err := newRedisIdemStore(addr)
	if err != nil {
		t.Skipf("redis 不可达（%v）——环境依赖", err)
	}
	ctx := context.Background()
	key := "probe-" + time.Now().Format("150405.000000000")
	if err := st.save(ctx, key, "fp-1", []byte(`{"converged":true}`)); err != nil {
		t.Fatalf("save: %v", err)
	}
	stt, cached, cerr := st.check(ctx, key, "fp-1")
	if cerr != nil || stt != idemReplay || string(cached) != `{"converged":true}` {
		t.Fatalf("roundtrip: state=%v cached=%q err=%v", stt, cached, cerr)
	}
	if stt, _, _ := st.check(ctx, key, "fp-2"); stt != idemConflict {
		t.Fatalf("conflict: %v", stt)
	}
	t.Log("roundtrip OK")
}
