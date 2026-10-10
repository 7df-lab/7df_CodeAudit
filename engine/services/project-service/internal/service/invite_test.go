// V2.2 (ADR-255) 邀请码语义锁定：三值创建语义/消费原子性（含并发）/吊销幂等/
// 删除护栏（Q14 两步走）/注册合并校验（DB ∪ 静态）/策略读。fakeInviteMirror 锁
// PG 档语义（CAS 谓词+注错），并发面以 fake 的 compare-and-swap 代表（真 PG 由
// e2e 五连覆盖）。
package service

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/project-service/internal/repo"
)

// fakeInviteMirror — InviteMirror 测试替身：罐装记录 + CAS 消费（谓词同
// repo.InviteUsable，与真 SQL WHERE 同语义注记）+ 注错。
type fakeInviteMirror struct {
	mu         sync.Mutex
	byCode     map[string]*v1.InviteCode
	consumeErr error
}

func newFakeInviteMirror() *fakeInviteMirror {
	return &fakeInviteMirror{byCode: map[string]*v1.InviteCode{}}
}

func (f *fakeInviteMirror) UpsertInviteCode(rec *v1.InviteCode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *rec
	f.byCode[rec.GetCode()] = &clone
	return nil
}

func (f *fakeInviteMirror) ListInviteCodes() ([]*v1.InviteCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*v1.InviteCode
	for _, rec := range f.byCode {
		out = append(out, rec)
	}
	return out, nil
}

func (f *fakeInviteMirror) GetInviteCode(code string) (*v1.InviteCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rec, ok := f.byCode[code]; ok {
		clone := *rec
		return &clone, nil
	}
	return nil, nil
}

// ConsumeInviteCode — CAS：谓词判定+计数一体（模拟 UPDATE ... WHERE used_count=$cas）。
func (f *fakeInviteMirror) ConsumeInviteCode(code, userID string, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.consumeErr != nil {
		return false, f.consumeErr
	}
	rec, ok := f.byCode[code]
	if !ok || !repo.InviteUsable(rec, now) {
		return false, nil
	}
	rec.UsedCount++
	rec.LastUsedBy = userID
	rec.LastUsedAt = timestamppb.New(now)
	return true, nil
}

func (f *fakeInviteMirror) DeleteInviteCode(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for code, rec := range f.byCode {
		if rec.GetId() == id {
			delete(f.byCode, code)
			return nil
		}
	}
	return nil
}

// newInviteSvc — invitation 档 + 可选镜像的统一构造。
func newInviteSvc(mirror repo.InviteMirror, staticCodes ...string) *UserService {
	svc := NewUserService(repo.NewMemoryStore(false))
	svc.SetAuthConfig(AuthConfig{RegistrationMode: "invitation", InviteCodes: staticCodes})
	if mirror != nil {
		svc.SetInviteMirror(mirror)
	}
	return svc
}

var inviteCodeRe = regexp.MustCompile(`^CODEAUDIT-[0-9A-HJKMNP-TV-Z]{5}-[0-9A-HJKMNP-TV-Z]{5}$`)

// 三值创建语义（Q1/Q2/Q3）：缺省=一次性+30 天；显式 0=不限；never_expires 与
// expires_at 互斥；过期时间须在未来；码形=系统随机 Crockford。
func TestCreateInviteCode_Semantics(t *testing.T) {
	svc := newInviteSvc(nil)

	rec, err := svc.CreateInviteCode(0, false, nil, false, "n1", "admin-1", v1.Role_ROLE_UNSPECIFIED)
	if err != nil {
		t.Fatalf("default create: %v", err)
	}
	if rec.GetMaxUses() != 1 {
		t.Fatalf("default max_uses = %d, want 1 (一次性缺省 07 §7.1)", rec.GetMaxUses())
	}
	if rec.GetExpiresAt() == nil {
		t.Fatal("default expires_at 未落缺省 30 天")
	}
	got := time.Until(rec.GetExpiresAt().AsTime())
	if got < 29*24*time.Hour || got > 31*24*time.Hour {
		t.Fatalf("default TTL ≈ %.0fh, want ≈ 720h", got.Hours())
	}
	if !strings.HasPrefix(rec.GetId(), "invite-") {
		t.Fatalf("id 前缀 = %s", rec.GetId())
	}
	if !inviteCodeRe.MatchString(rec.GetCode()) {
		t.Fatalf("码形 %q 不符 Crockford 形制", rec.GetCode())
	}

	unlim, err := svc.CreateInviteCode(0, true, nil, true, "n2", "admin-1", v1.Role_ROLE_UNSPECIFIED) // 显式 0=不限+永久
	if err != nil || unlim.GetMaxUses() != 0 || unlim.GetExpiresAt() != nil {
		t.Fatalf("unlimited/never: rec=%+v err=%v", unlim, err)
	}

	if _, err := svc.CreateInviteCode(0, true, timestamppb.New(time.Now().Add(time.Hour)), true, "", "", v1.Role_ROLE_UNSPECIFIED); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("never_expires+expires_at 同设: got %v, want InvalidArgument", err)
	}
	if _, err := svc.CreateInviteCode(0, true, timestamppb.New(time.Now().Add(-time.Hour)), false, "", "", v1.Role_ROLE_UNSPECIFIED); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("过去时间: got %v, want InvalidArgument", err)
	}
	if _, err := svc.CreateInviteCode(-1, true, nil, false, "", "", v1.Role_ROLE_UNSPECIFIED); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("负值 max_uses: got %v, want InvalidArgument", err)
	}
}

// 注册消费主锁（Q1/Q4/Q9）：DB 码一次性（二用拒）；静态配置码仍可用（bootstrap
// 合并）；码归属 user_id 记入 last_used_by。
func TestRegisterUser_InviteConsumption(t *testing.T) {
	svc := newInviteSvc(nil, "STATIC-1")

	rec, err := svc.CreateInviteCode(0, false, nil, false, "给张三", "admin-1", v1.Role_ROLE_UNSPECIFIED)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.RegisterUser("alice", "a@x.io", "Passw0rd123", rec.GetCode()); err != nil {
		t.Fatalf("首用注册: %v", err)
	}
	got, ok := svc.store.GetInviteCode(rec.GetCode())
	if !ok || got.GetUsedCount() != 1 || !strings.HasPrefix(got.GetLastUsedBy(), "user-") {
		t.Fatalf("消费记账: rec=%+v", got)
	}
	if _, err := svc.RegisterUser("bob", "b@x.io", "Passw0rd123", rec.GetCode()); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("二用: got %v, want FailedPrecondition", err)
	}
	// 用尽码不产生孤儿用户
	if recs := svc.ListUsers(v1.User_USER_STATE_UNSPECIFIED, "bob"); len(recs) != 0 {
		t.Fatal("消费失败的注册不应留下用户")
	}
	// 静态码（V1 语义：无限）
	for i := 0; i < 2; i++ {
		if _, err := svc.RegisterUser(fmt.Sprintf("st%d", i), fmt.Sprintf("st%d@x.io", i), "Passw0rd123", "STATIC-1"); err != nil {
			t.Fatalf("静态码第 %d 用: %v", i+1, err)
		}
	}
	if _, err := svc.RegisterUser("eve", "e@x.io", "Passw0rd123", "NO-SUCH"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("废码: got %v, want FailedPrecondition", err)
	}
}

// 内存档并发防超卖：max_uses=N 的码在 2N 并发下恰成功 N 次。
func TestConsumeInviteCode_ConcurrencyNoOversell(t *testing.T) {
	svc := newInviteSvc(nil)
	rec, _ := svc.CreateInviteCode(5, true, nil, true, "", "", v1.Role_ROLE_UNSPECIFIED)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if svc.store.ConsumeInviteCode(rec.GetCode(), "u", time.Now()) {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 5 {
		t.Fatalf("并发消费成功 %d 次, want 5（防超卖）", ok)
	}
}

// 吊销语义（Q11）：状态翻转+幂等；吊销后注册拒。
func TestRevokeInviteCode(t *testing.T) {
	svc := newInviteSvc(nil)
	rec, _ := svc.CreateInviteCode(0, true, nil, true, "", "", v1.Role_ROLE_UNSPECIFIED)
	revoked, err := svc.RevokeInviteCode(rec.GetId())
	if err != nil || revoked.GetState() != "revoked" || revoked.GetRevokedAt() == nil {
		t.Fatalf("吊销: rec=%+v err=%v", revoked, err)
	}
	if again, err := svc.RevokeInviteCode(rec.GetId()); err != nil || again.GetState() != "revoked" {
		t.Fatalf("重复吊销应幂等: rec=%+v err=%v", again, err)
	}
	if _, err := svc.RegisterUser("carol", "c@x.io", "Passw0rd123", rec.GetCode()); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("吊销后注册: got %v, want FailedPrecondition", err)
	}
	if _, err := svc.RevokeInviteCode("invite-none"); status.Code(err) != codes.NotFound {
		t.Fatalf("不存在: got %v, want NotFound", err)
	}
}

// 删除护栏（Q14）：活跃码拒（两步走）；吊销后/用尽后/过期后可删。
func TestDeleteInviteCode_TerminalGuard(t *testing.T) {
	svc := newInviteSvc(nil)
	active, _ := svc.CreateInviteCode(0, true, nil, true, "", "", v1.Role_ROLE_UNSPECIFIED)
	if err := svc.DeleteInviteCode(active.GetId()); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("活跃码直删: got %v, want FailedPrecondition", err)
	}
	svc.RevokeInviteCode(active.GetId()) // Q14 两步走：先吊销
	if err := svc.DeleteInviteCode(active.GetId()); err != nil {
		t.Fatalf("吊销后删: %v", err)
	}

	revoked, _ := svc.CreateInviteCode(0, true, nil, true, "", "", v1.Role_ROLE_UNSPECIFIED)
	svc.RevokeInviteCode(revoked.GetId())
	if err := svc.DeleteInviteCode(revoked.GetId()); err != nil {
		t.Fatalf("吊销后删: %v", err)
	}

	expired, _ := svc.CreateInviteCode(0, true, timestamppb.New(time.Now().Add(time.Hour)), false, "", "", v1.Role_ROLE_UNSPECIFIED)
	expired.ExpiresAt = timestamppb.New(time.Now().Add(-time.Minute)) // 直接改内存态模拟到期
	svc.store.PutInviteCode(expired)
	if err := svc.DeleteInviteCode(expired.GetId()); err != nil {
		t.Fatalf("过期后删: %v", err)
	}

	exhausted, _ := svc.CreateInviteCode(1, true, nil, true, "", "", v1.Role_ROLE_UNSPECIFIED)
	svc.store.ConsumeInviteCode(exhausted.GetCode(), "u", time.Now())
	if err := svc.DeleteInviteCode(exhausted.GetId()); err != nil {
		t.Fatalf("用尽后删: %v", err)
	}
	if err := svc.DeleteInviteCode("invite-none"); status.Code(err) != codes.NotFound {
		t.Fatalf("不存在: got %v, want NotFound", err)
	}
}

// 列表过滤（Q11/Q4）：默认仅有效；include_terminal 全量；静态注入码只读随行。
func TestListInviteCodes(t *testing.T) {
	svc := newInviteSvc(nil, "STATIC-1")
	a, _ := svc.CreateInviteCode(0, true, nil, true, "", "", v1.Role_ROLE_UNSPECIFIED)
	b, _ := svc.CreateInviteCode(0, true, nil, true, "", "", v1.Role_ROLE_UNSPECIFIED)
	svc.RevokeInviteCode(b.GetId())
	codes0, config := svc.ListInviteCodes(false)
	if len(codes0) != 1 || codes0[0].GetId() != a.GetId() {
		t.Fatalf("默认筛: %d 条", len(codes0))
	}
	if len(config) != 1 || config[0] != "STATIC-1" {
		t.Fatalf("静态随行: %v", config)
	}
	all, _ := svc.ListInviteCodes(true)
	if len(all) != 2 {
		t.Fatalf("含终态: %d 条", len(all))
	}
}

// 策略读（Q10）：三档透传。
func TestGetRegistrationPolicy(t *testing.T) {
	svc := newInviteSvc(nil)
	if got := svc.GetRegistrationPolicy(); got != "invitation" {
		t.Fatalf("invitation 档: %s", got)
	}
	svc.SetAuthConfig(AuthConfig{RegistrationMode: "open"})
	if got := svc.GetRegistrationPolicy(); got != "open" {
		t.Fatalf("open 档: %s", got)
	}
	svc.SetAuthConfig(AuthConfig{RegistrationMode: "disabled"})
	if got := svc.GetRegistrationPolicy(); got != "disabled" {
		t.Fatalf("disabled 档: %s", got)
	}
}

// PG 档消费（镜像权威）：CAS 命中+回灌；PG 无此码回落静态；PG 故障降级内存档
// （ADR-250 故障域，超卖风险 ADR-255 注记）。
func TestConsumeInviteCode_MirrorPaths(t *testing.T) {
	fm := newFakeInviteMirror()
	svc := newInviteSvc(fm, "STATIC-2")

	pgOnly, _ := svc.CreateInviteCode(0, false, nil, true, "", "", v1.Role_ROLE_UNSPECIFIED) // 建码即写穿 fake 镜像
	svc.store.DeleteInviteCode(pgOnly.GetId())                     // 抹掉内存副本模拟跨副本码
	if _, err := svc.RegisterUser("dave", "d@x.io", "Passw0rd123", pgOnly.GetCode()); err != nil {
		t.Fatalf("镜像档消费（仅 PG 有码）: %v", err)
	}

	if _, err := svc.RegisterUser("erin", "e2@x.io", "Passw0rd123", "STATIC-2"); err != nil {
		t.Fatalf("PG miss 静态兜底: %v", err)
	}

	fm.consumeErr = fmt.Errorf("pg down")
	memOnly, _ := svc.CreateInviteCode(0, true, nil, true, "", "", v1.Role_ROLE_UNSPECIFIED)
	if !svc.consumeInviteCode(memOnly.GetCode(), "u-x") {
		t.Fatal("PG 故障降级内存档消费应成功")
	}

	// 审查批：PG miss（无此码/谓词拒）先落内存档再静态——镜像写穿 best-effort
	// 失败的码（仅内存可见）须可消费（与预检查源对称），不再直跳静态层
	fm.consumeErr = nil
	memOnly2, _ := svc.CreateInviteCode(0, true, nil, true, "", "", v1.Role_ROLE_UNSPECIFIED)
	fm.mu.Lock()
	delete(fm.byCode, memOnly2.GetCode()) // 抹掉镜像副本，仅内存可见
	fm.mu.Unlock()
	if !svc.consumeInviteCode(memOnly2.GetCode(), "u-y") {
		t.Fatal("PG miss 的内存档码应可消费（预检/消费查源对称）")
	}
}

// ADR-257: 邀请码角色绑定——create 侧校验（ADMIN 禁发/缺省 DEVELOPER/VIEWER 落值）
// 与注册侧落地（VIEWER 码→ROLE_VIEWER，旧码/静态码→DEVELOPER）。
func TestInviteCode_RoleBinding(t *testing.T) {
	svc := newInviteSvc(nil, "STATIC-9")

	// create：ADMIN 禁发（防自助注册自封 admin）
	if _, err := svc.CreateInviteCode(1, true, nil, false, "", "admin-1", v1.Role_ROLE_ADMIN); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ADMIN 码应 INVALID_ARGUMENT, got %v", err)
	}
	// create：缺省（UNSPECIFIED）→ 落值 DEVELOPER
	def, err := svc.CreateInviteCode(1, true, nil, false, "", "admin-1", v1.Role_ROLE_UNSPECIFIED)
	if err != nil || def.GetRole() != v1.Role_ROLE_DEVELOPER {
		t.Fatalf("缺省码 role = %v err=%v, want DEVELOPER", def.GetRole(), err)
	}
	// create：VIEWER 落值
	vw, err := svc.CreateInviteCode(2, true, nil, false, "外发只读", "admin-1", v1.Role_ROLE_VIEWER)
	if err != nil || vw.GetRole() != v1.Role_ROLE_VIEWER {
		t.Fatalf("VIEWER 码 role = %v err=%v, want VIEWER", vw.GetRole(), err)
	}
	// 注册：VIEWER 码 → 用户 ROLE_VIEWER
	u1, err := svc.RegisterUser("ro1", "ro1@x.io", "Passw0rd123", vw.GetCode())
	if err != nil {
		t.Fatalf("viewer-code register: %v", err)
	}
	if u1.GetRole() != v1.Role_ROLE_VIEWER {
		t.Fatalf("viewer-code 注册角色 = %v, want ROLE_VIEWER", u1.GetRole())
	}
	// 注册：DEVELOPER 码（缺省）→ ROLE_DEVELOPER
	u2, err := svc.RegisterUser("dev1", "dev1@x.io", "Passw0rd123", def.GetCode())
	if err != nil {
		t.Fatalf("dev-code register: %v", err)
	}
	if u2.GetRole() != v1.Role_ROLE_DEVELOPER {
		t.Fatalf("dev-code 注册角色 = %v, want ROLE_DEVELOPER", u2.GetRole())
	}
	// 注册：静态配置码（无 role 字段）→ DEVELOPER（历史兼容语义）
	u3, err := svc.RegisterUser("st9", "st9@x.io", "Passw0rd123", "STATIC-9")
	if err != nil {
		t.Fatalf("static-code register: %v", err)
	}
	if u3.GetRole() != v1.Role_ROLE_DEVELOPER {
		t.Fatalf("static-code 注册角色 = %v, want ROLE_DEVELOPER", u3.GetRole())
	}
}
