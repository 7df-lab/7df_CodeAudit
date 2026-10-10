// PG 镜像织入层（HA P1-a，ADR-250）：两 service 的 mirror 注入+回落/写穿帮手。
// 写穿语义=task-service persistTaskLocked 同款 best-effort（失败 log 不反噬本地
// ——PG 故障=中间件故障域，Q4 边界外；跨副本可见性以 PG 健康为前提）。
package service

import (
	"log"
	"time"

	"github.com/codeaudit/services/project-service/internal/repo"
)

// mirrorErr — 写穿失败统一日志（含定位信息；不反噬调用方）。
func mirrorErr(what, id string, err error) {
	log.Printf("[project-mirror] %s %s 写穿失败（本副本可见、跨副本暂不可见）: %v", what, id, err)
}

// hydrateAuthState — 装配期全量回放黑名单/纪元（认证语义读高频，读走内存；
// 过期行窗内过滤，PG 残留无害）。
func (s *UserService) hydrateAuthState(m *repo.PGMirror) {
	if revoked, err := m.HydrateRevoked(24 * time.Hour); err != nil {
		log.Printf("[project-mirror] 黑名单回放失败（空集起步，诚实降级）: %v", err)
	} else {
		s.mu.Lock()
		for tok, at := range revoked {
			s.revokedTokens[tok] = at
		}
		s.mu.Unlock()
	}
	if epochs, err := m.HydrateEpochs(); err != nil {
		log.Printf("[project-mirror] 会话纪元回放失败（空集起步）: %v", err)
	} else {
		s.mu.Lock()
		for uid, at := range epochs {
			s.sessionsInvalidBefore[uid] = at
		}
		s.mu.Unlock()
	}
}

// syncProjectsFromPG — List 权威面：PG 全量拉→内存补缺失（不覆盖本地未落穿
// 残留——保守合流），再由调用方走既有内存排序。
func (s *ProjectService) syncProjectsFromPG(m *repo.PGMirror) {
	recs, err := m.ListProjects()
	if err != nil {
		log.Printf("[project-mirror] projects 全量同步失败（本副本视图）: %v", err)
		return
	}
	for _, rec := range recs {
		if _, ok := s.store.GetProject(rec.Project.GetProjectId()); !ok {
			s.store.CreateProject(rec)
		}
	}
}
