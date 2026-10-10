package repo

// R138 锁定测试（第四轮审计 P3-2）：GenerateID 并发唯一。原实现=纳秒时间戳，
// 并发调用可取到同一纳秒——file_id/notification_id 碰撞互相覆盖（通知侧每事件
// 一个 ID，高频消费下碰撞面实际；project-service R65 同类先例已改 crypto/rand）。
// 判杀锚=格式正则（时间戳+随机后缀）：变异回退纯时间戳形态时正则不匹配，确定性红。

import (
	"regexp"
	"sync"
	"testing"
)

func TestGenerateID_FormatAndConcurrentUnique(t *testing.T) {
	if !regexp.MustCompile(`^file-\d{14}-[0-9a-f]{16}$`).MatchString(GenerateID("file")) {
		t.Fatalf("GenerateID 形态漂移（须为 <prefix>-<14位时间戳>-<16位随机hex>）: %s", GenerateID("file"))
	}
	const n = 500
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- GenerateID("notif")
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	dup := 0
	for id := range ids {
		if seen[id] {
			dup++
		}
		seen[id] = true
	}
	if dup > 0 {
		t.Fatalf("并发 %d 次 GenerateID 碰撞 %d 个（纳秒时间戳同刻碰撞，P3-2）", n, dup)
	}
}
