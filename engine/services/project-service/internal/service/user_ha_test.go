// TP19-T7 用户面 HA 单测（roadmap TP19-T7 ③ 单测设施首批兑现）：
// 读回落（跨副本用户改密/重置/登录）与建号写穿唯一冲突映射（并发同名回滚）
// 经 fakeUserMirror 锁定——此前 mirror 为具体类型，此两族语义只有 ha-verify
// 集成验证（TP19-T2 先例），无单测无变异锚（R139"如实分层"欠账的本批清偿面）。
package service

import (
	"errors"
	"testing"
	"time"

	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/project-service/internal/repo"
)

// fakeUserMirror — UserMirror 测试替身：罐装记录两张索引 + upsert 记录/注错 +
// 纪元写穿记录。仅服务本文件语义锁定，不模拟 PG 并发（并发面由 23505 注错代表）。
type fakeUserMirror struct {
	byID       map[string]*repo.UserRecord
	byUsername map[string]*repo.UserRecord
	upserts    []*repo.UserRecord
	upsertErr  error
	epochs     map[string]time.Time
}

func newFakeUserMirror() *fakeUserMirror {
	return &fakeUserMirror{
		byID:       map[string]*repo.UserRecord{},
		byUsername: map[string]*repo.UserRecord{},
		epochs:     map[string]time.Time{},
	}
}

func (f *fakeUserMirror) GetUser(id string) (*repo.UserRecord, bool, error) {
	rec, ok := f.byID[id]
	return rec, ok, nil
}

func (f *fakeUserMirror) GetUserByUsername(username string) (*repo.UserRecord, bool, error) {
	rec, ok := f.byUsername[username]
	return rec, ok, nil
}

func (f *fakeUserMirror) UpsertUser(rec *repo.UserRecord) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts = append(f.upserts, rec)
	f.byID[rec.User.GetUserId()] = rec
	f.byUsername[rec.User.GetUsername()] = rec
	return nil
}

func (f *fakeUserMirror) SaveRevokedToken(string, time.Time) error { return nil }

func (f *fakeUserMirror) SaveSessionEpoch(userID string, at time.Time) error {
	f.epochs[userID] = at
	return nil
}

func (f *fakeUserMirror) ListMembersForUser(string) ([]*repo.ProjectMemberRecord, error) {
	return nil, nil
}

func (f *fakeUserMirror) HydrateRevoked(time.Duration) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}

func (f *fakeUserMirror) HydrateEpochs() (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt hash: %v", err)
	}
	return string(h)
}

func haUser(id, username, passwordHash string) *repo.UserRecord {
	return &repo.UserRecord{
		User: &v1.User{
			UserId:             id,
			Username:           username,
			Email:              username + "@example.com",
			State:              v1.User_USER_STATE_ACTIVE,
			Role:               v1.Role_ROLE_DEVELOPER,
			MustChangePassword: true,
		},
		Password: passwordHash,
	}
}

// 跨副本改密：本地未中的用户经镜像读回落可自助改密，且结果写回本地（后续读全热）、
// 写穿镜像、清除强改密标记、推进会话纪元（R87 踢旧 refresh）。
func TestChangePassword_MirrorFallbackHit(t *testing.T) {
	svc := NewUserService(repo.NewMemoryStore(false))
	fm := newFakeUserMirror()
	u := haUser("user-ha-1", "ha-user", mustHash(t, "old-pass-123"))
	fm.byID["user-ha-1"] = u
	fm.byUsername["ha-user"] = u
	svc.mirror = fm

	if err := svc.ChangePassword("user-ha-1", "old-pass-123", "new-pass-456"); err != nil {
		t.Fatalf("跨副本改密应经读回落成功: %v", err)
	}
	// 写回本地：清空镜像后同 ID 仍可命中（本地已热）。
	fm.byID = map[string]*repo.UserRecord{}
	if _, ok := svc.store.GetUser("user-ha-1"); !ok {
		t.Fatal("读回落命中后未写回本地 store")
	}
	if rec, _ := svc.store.GetUser("user-ha-1"); rec.User.GetMustChangePassword() {
		t.Fatal("改密成功后 must_change_password 应清除")
	}
	if len(fm.upserts) != 1 {
		t.Fatalf("新口令哈希应写穿镜像一次，实际 %d 次", len(fm.upserts))
	}
	if bcrypt.CompareHashAndPassword([]byte(fm.upserts[0].Password), []byte("new-pass-456")) != nil {
		t.Fatal("写穿镜像的不是新口令哈希")
	}
	if !svc.sessionInvalidated("user-ha-1", time.Now().Add(-time.Second)) {
		t.Fatal("改密成功应推进会话纪元（R87）")
	}
}

// 双面未中=诚实 NotFound（不以上次残留冒充）。
func TestChangePassword_MirrorMissIsNotFound(t *testing.T) {
	svc := NewUserService(repo.NewMemoryStore(false))
	svc.mirror = newFakeUserMirror()
	err := svc.ChangePassword("user-ghost", "whatever", "new-pass-456")
	if status.Code(err) != codes.NotFound {
		t.Fatalf("本地+镜像双未中应为 NotFound，实际 %v", err)
	}
}

// 跨副本重置：管理员对本地未中用户可经读回落重置，一次性临时密码生效且强改密置真。
func TestResetPassword_MirrorFallbackHit(t *testing.T) {
	svc := NewUserService(repo.NewMemoryStore(false))
	fm := newFakeUserMirror()
	u := haUser("user-ha-2", "ha-user-2", mustHash(t, "old-pass-123"))
	fm.byID["user-ha-2"] = u
	svc.mirror = fm

	temp, err := svc.ResetPassword("user-ha-2")
	if err != nil {
		t.Fatalf("跨副本重置应经读回落成功: %v", err)
	}
	if temp == "" {
		t.Fatal("重置应返回一次性临时密码")
	}
	if len(fm.upserts) != 1 {
		t.Fatalf("重置哈希应写穿镜像一次，实际 %d 次", len(fm.upserts))
	}
	if bcrypt.CompareHashAndPassword([]byte(fm.upserts[0].Password), []byte(temp)) != nil {
		t.Fatal("写穿镜像的不是临时密码哈希")
	}
	if rec, ok := svc.store.GetUser("user-ha-2"); !ok || !rec.User.GetMustChangePassword() {
		t.Fatal("重置后本地应写回且 must_change_password 置真")
	}
	if !svc.sessionInvalidated("user-ha-2", time.Now().Add(-time.Second)) {
		t.Fatal("重置应推进会话纪元（R87）")
	}
}

// 双副本并发同名注册：本地各自通过、PG 侧 UNIQUE 拒第二笔（23505）——回滚本地
// 插入并按 AlreadyExists 上抛（此前仅记日志=两端皆成功脑裂，TP19-T7 ②）。
func TestRegisterUser_MirrorUniqueConflictRollsBack(t *testing.T) {
	svc := NewUserService(repo.NewMemoryStore(false))
	svc.SetAuthConfig(AuthConfig{RegistrationMode: "open"})
	fm := newFakeUserMirror()
	fm.upsertErr = &pq.Error{Code: "23505"} // PG username/email UNIQUE 冲突
	svc.mirror = fm

	_, err := svc.RegisterUser("dup-user", "dup@example.com", "valid-pass-1", "")
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("PG 唯一冲突应映射 AlreadyExists，实际 %v", err)
	}
	if _, ok := svc.store.GetUserByUsername("dup-user"); ok {
		t.Fatal("唯一冲突后本地插入应回滚（否则副本间脑裂同名）")
	}
}

// 非唯一类写穿失败维持 best-effort（ADR-250：PG 故障=中间件故障域不反噬本地成功）。
func TestRegisterUser_MirrorOtherErrorTolerated(t *testing.T) {
	svc := NewUserService(repo.NewMemoryStore(false))
	svc.SetAuthConfig(AuthConfig{RegistrationMode: "open"})
	fm := newFakeUserMirror()
	fm.upsertErr = errors.New("connection refused")
	svc.mirror = fm

	user, err := svc.RegisterUser("solo-user", "solo@example.com", "valid-pass-1", "")
	if err != nil {
		t.Fatalf("非唯一写穿失败应 best-effort 放行: %v", err)
	}
	if _, ok := svc.store.GetUserByUsername("solo-user"); !ok {
		t.Fatal("best-effort 语义下本地用户应保留")
	}
	if user.GetUsername() != "solo-user" {
		t.Fatalf("回执用户名不符: %s", user.GetUsername())
	}
}

// 管理员直建通道同型映射（CreateUser 与 RegisterUser 共用 mirrorCreateUser）。
func TestCreateUser_AdminUniqueConflictRollsBack(t *testing.T) {
	svc := NewUserService(repo.NewMemoryStore(false))
	fm := newFakeUserMirror()
	fm.upsertErr = &pq.Error{Code: "23505"}
	svc.mirror = fm

	_, err := svc.CreateUser("dup-admin", "dup-admin@example.com", "valid-pass-1", v1.Role_ROLE_DEVELOPER)
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("管理通道 PG 唯一冲突应映射 AlreadyExists，实际 %v", err)
	}
	if _, ok := svc.store.GetUserByUsername("dup-admin"); ok {
		t.Fatal("管理通道唯一冲突后本地插入应回滚")
	}
}

// 登录读回落写回：跨副本注册用户在本副本首登即热身（username 侧帮手）。
func TestLogin_MirrorFallbackWarmLocal(t *testing.T) {
	t.Setenv("CODEAUDIT_JWT_SECRET", "test-secret-tp19t7")
	svc := NewUserService(repo.NewMemoryStore(false))
	fm := newFakeUserMirror()
	u := haUser("user-ha-3", "ha-login", mustHash(t, "old-pass-123"))
	u.User.MustChangePassword = false
	fm.byID["user-ha-3"] = u
	fm.byUsername["ha-login"] = u
	svc.mirror = fm

	resp, err := svc.Login("ha-login", "old-pass-123")
	if err != nil {
		t.Fatalf("跨副本用户登录应经读回落成功: %v", err)
	}
	if resp.GetAccessToken() == "" {
		t.Fatal("登录应签发 access token")
	}
	fm.byUsername = map[string]*repo.UserRecord{}
	if _, ok := svc.store.GetUserByUsername("ha-login"); !ok {
		t.Fatal("登录读回落命中后应写回本地（同副本后续读全热）")
	}
}
