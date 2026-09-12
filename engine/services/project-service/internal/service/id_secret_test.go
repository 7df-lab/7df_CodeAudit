package service

import (
	"fmt"
	"testing"
	"time"
)

// R65（2026-09-12 待办收尾·账号面）：ID 生成必须密码学随机——原 UnixNano&0xFFFFFFFF
// 截断 32 位，相差恰 4.295s 的两次生成必然同 ID 且可预测。
func TestGenerateID_CryptoRandom(t *testing.T) {
	seen := make(map[string]bool, 10000)
	for i := 0; i < 10000; i++ {
		id := generateID()
		if len(id) != 16 { // crypto/rand 8 字节 hex
			t.Fatalf("unexpected length %d: %s", len(id), id)
		}
		if seen[id] {
			t.Fatalf("collision at %d: %s", i, id)
		}
		seen[id] = true
	}
	ts := fmt.Sprintf("%08x", time.Now().UnixNano()&0xFFFFFFFF)
	if len(ts) == len(generateID()) && generateID() == ts {
		t.Fatal("crypto ID must not match time-derived form")
	}
}

// R65：显式密钥优先；未配置时 panic（fail-fast，对齐 gateway 同键位）——
// 无法在测试进程卸载已设 env，锁"已配置路径"+实现审读。
func TestJwtSecret_ExplicitValueWins(t *testing.T) {
	t.Setenv("CODEAUDIT_JWT_SECRET", "explicit-secret-r65")
	if string(jwtSecret()) != "explicit-secret-r65" {
		t.Fatal("explicit secret must win")
	}
}
