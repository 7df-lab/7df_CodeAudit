// Package handler — 任务源码全文读取（ADR-195）。
// 依据: 14号 §3.3 ④"V1.1 经文件服务取源码"演进方向；工程形态=gateway 本地源树
// （ADR-145 uploads/archive 的读侧对应物；storage/MinIO 多节点生产化仍留原设计）。
// 安全: JWT 链内（apiMux）；safeJoin 同款穿越防护（ADR-145）+ EvalSymlinks 根内校验；
// 大小上限 + 二进制拒绝（源码复核只需文本）。
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/status"
)

// 源码全文读取上限（ADR-195）：2MiB——覆盖常规源码文件；超限如实 413，
// 前端降级回 ADR-143 扫描时捕获的 ±10 行片段。
const maxSourceFileBytes = 2 << 20

// ReposDir — 仓库拉取流的任务源根（ADR-195: gateway.repos_dir，与 task.repos_dir
// 同值；repo 流 clone 目的地=<repos_dir>/<task_id>，task-service 内存态重启后仍可回查）。
var ReposDir string

// taskLinkName — 上传流任务链接文件名（ADR-195）：CreateScanTask 拦截
// config.project_path 写入 <dir>/.codeaudit-task-<task_id>，供任务→上传目录持久回查。
func taskLinkName(taskID string) string { return ".codeaudit-task-" + taskID }

// resolveTaskRoot — 任务源根解析（ADR-195 顺序）：
// ①repos_dir/<task_id>（仓库拉取流）②上传目录链接文件 ③project config
// project_path（ADR-148 上传流）④唯一内容回退（按 seedPath 在全部上传目录查包含者；
// 唯一命中即用，多命中取 mtime 最新——覆盖 ADR-195 之前创建的无链接存量任务）。
func (t *Transcoder) resolveTaskRoot(ctx context.Context, taskID, projectID, seedPath string) (string, string, error) {
	// ① 仓库拉取流
	if ReposDir != "" {
		candidate := filepath.Join(ReposDir, taskID)
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			return candidate, "repos_dir", nil
		}
	}
	// ② 上传目录链接文件
	if UploadsDir != "" {
		if entries, err := os.ReadDir(UploadsDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				dir := filepath.Join(UploadsDir, e.Name())
				if fi, err := os.Stat(filepath.Join(dir, taskLinkName(taskID))); err == nil && !fi.IsDir() {
					return dir, "upload_link", nil
				}
			}
		}
	}
	// ③ project config project_path（ADR-148）
	if projectID != "" && t.projectConn != nil {
		client := pb.NewProjectServiceClient(t.projectConn)
		cctx, cancel := context.WithTimeout(ctx, t.callTimeout)
		defer cancel()
		if resp, err := client.GetProjectConfig(cctx, &pb.GetProjectConfigRequest{ProjectId: projectID}); err == nil {
			if p := resp.GetConfig()["project_path"]; p != "" {
				if fi, serr := os.Stat(p); serr == nil && fi.IsDir() {
					return p, "project_config", nil
				}
			}
		}
	}
	// ④ 唯一内容回退：seedPath（发现的 file_path，项目相对路径）在哪些上传目录中存在
	if UploadsDir != "" && seedPath != "" {
		type hit struct {
			dir   string
			mtime time.Time
		}
		var hits []hit
		if entries, err := os.ReadDir(UploadsDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				dir := filepath.Join(UploadsDir, e.Name())
				rel, _, err := resolveWithinRoot(dir, seedPath)
				if err != nil {
					continue
				}
				if fi, serr := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); serr == nil {
					hits = append(hits, hit{dir: dir, mtime: fi.ModTime()})
				}
			}
		}
		if len(hits) > 0 {
			sort.Slice(hits, func(i, j int) bool { return hits[i].mtime.After(hits[j].mtime) })
			return hits[0].dir, "upload_content_newest", nil
		}
	}
	return "", "", fmt.Errorf("任务源根不可解析（repo 目录/上传链接/project config 均无，且上传目录中无包含 %q 的源树）；该任务可能运行于环境变量覆盖路径或源目录已清理", seedPath)
}

// resolveWithinRoot — 在选定根内解析请求路径（ADR-195）：
// 直接命中 > 后缀匹配 > 基名匹配（AI 结论常写裸文件名或截断路径）。
// 返回（项目相对路径, via, error）。拒绝软链与链接文件自身。
func resolveWithinRoot(root, path string) (string, string, error) {
	clean := filepath.Clean("/" + strings.ReplaceAll(path, "\\", "/")) // 前置 "/" 使 .. 被吸收
	clean = strings.TrimPrefix(clean, string(os.PathSeparator))
	if clean == "" || clean == "." {
		return "", "", fmt.Errorf("empty path")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", "", err
	}

	accept := func(abs string) (string, string, error) {
		fi, err := os.Lstat(abs)
		if err != nil || fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return "", "", fmt.Errorf("not a regular file: %s", abs)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", "", err
		}
		if real != rootReal && !strings.HasPrefix(real, rootReal+string(os.PathSeparator)) {
			return "", "", fmt.Errorf("resolved outside project root: %s", abs)
		}
		rel, err := filepath.Rel(rootReal, real)
		if err != nil {
			return "", "", err
		}
		base := filepath.Base(rel)
		if strings.HasPrefix(base, ".codeaudit-task-") {
			return "", "", fmt.Errorf("internal marker file")
		}
		return filepath.ToSlash(rel), "", nil
	}

	// ① 直接命中
	if rel, _, err := accept(filepath.Join(rootReal, filepath.FromSlash(clean))); err == nil {
		return rel, "exact", nil
	}
	// ②/③ 后缀匹配（path 含目录时）与基名匹配：遍历源树收集
	want := filepath.ToSlash(clean)
	base := path[strings.LastIndex(path, "/")+1:]
	if base == "" {
		return "", "", fmt.Errorf("file not found: %s", path)
	}
	var suffixHits, baseHits []string
	err = filepath.WalkDir(rootReal, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(rootReal, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(filepath.Base(rel), ".codeaudit-task-") {
			return nil
		}
		if want != base && strings.HasSuffix(rel, "/"+want) {
			suffixHits = append(suffixHits, rel)
		}
		if strings.HasSuffix(rel, "/"+base) || rel == base {
			baseHits = append(baseHits, rel)
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	pick := func(hits []string, via string) (string, string, error) {
		if len(hits) == 0 {
			return "", "", fmt.Errorf("no candidate")
		}
		sort.Slice(hits, func(i, j int) bool { // 最短路径=最靠近根=最可能是主源文件
			if len(hits[i]) != len(hits[j]) {
				return len(hits[i]) < len(hits[j])
			}
			return hits[i] < hits[j]
		})
		return hits[0], via, nil
	}
	if rel, via, err := pick(suffixHits, "suffix"); err == nil {
		return rel, via, nil
	}
	if rel, via, err := pick(baseHits, "basename"); err == nil {
		return rel, via, nil
	}
	return "", "", fmt.Errorf("file not found in project: %s", path)
}

// sourceFile — GET /v1/tasks/{id}/source-file?path=<项目相对路径|裸文件名>。
// 响应: {path, content, total_lines, bytes, root_via, resolved_via}。
func (t *Transcoder) sourceFile(w http.ResponseWriter, r *http.Request, taskID string) {
	pathParam := r.URL.Query().Get("path")
	if pathParam == "" {
		writeError(w, http.StatusBadRequest, "query 'path' is required (project-relative path or bare filename)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), t.callTimeout)
	defer cancel()

	// 任务存在性校验 + project_id（repo/上传流判别用）
	if t.taskConn == nil {
		writeError(w, http.StatusServiceUnavailable, "backend connection not configured")
		return
	}
	task, err := pb.NewTaskServiceClient(t.taskConn).GetScanTask(ctx, &pb.GetScanTaskRequest{TaskId: taskID})
	if err != nil {
		writeError(w, grpcToHTTP(err), status.Code(err).String()+": "+status.Convert(err).Message())
		return
	}

	root, rootVia, rerr := t.resolveTaskRoot(ctx, taskID, task.GetProjectId(), pathParam)
	if rerr != nil {
		writeError(w, http.StatusNotFound, rerr.Error())
		return
	}
	rel, via, perr := resolveWithinRoot(root, pathParam)
	if perr != nil {
		writeError(w, http.StatusNotFound, "root="+rootVia+": "+perr.Error())
		return
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Stat(abs)
	if err != nil {
		writeError(w, http.StatusNotFound, "stat: "+err.Error())
		return
	}
	if fi.Size() > maxSourceFileBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("file %s is %d bytes, exceeds %d bytes limit (ADR-195); use scan-time snippet instead", rel, fi.Size(), maxSourceFileBytes))
		return
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read: "+err.Error())
		return
	}
	// 二进制探测：前 8KB 含 NUL 字节即拒绝（源码复核只需文本）
	probe := content
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if strings.IndexByte(string(probe), 0) >= 0 {
		writeError(w, http.StatusUnsupportedMediaType, "binary file not supported: "+rel)
		return
	}
	totalLines := strings.Count(string(content), "\n") + 1
	if len(content) == 0 {
		totalLines = 0
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"path":         rel,
		"content":      string(content),
		"total_lines":  totalLines,
		"bytes":        fi.Size(),
		"root_via":     rootVia,
		"resolved_via": via,
	})
}

// writeTaskLink — CreateScanTask 成功后写任务→上传目录链接（ADR-195 流程②）。
// config.project_path 位于 UploadsDir 内时，在该目录落 .codeaudit-task-<task_id>；
// 写失败只记日志不影响任务创建（链接缺失时回退 ③④ 仍可解析）。
func writeTaskLink(taskID, projectPath string) {
	if projectPath == "" || UploadsDir == "" {
		return
	}
	uploadsAbs, err := filepath.Abs(UploadsDir)
	if err != nil {
		return
	}
	dirAbs, err := filepath.Abs(projectPath)
	if err != nil {
		return
	}
	if dirAbs != uploadsAbs && !strings.HasPrefix(dirAbs, uploadsAbs+string(os.PathSeparator)) {
		return // 非上传目录（宿主机路径/仓库流），不落链接
	}
	if err := os.WriteFile(filepath.Join(dirAbs, taskLinkName(taskID)), []byte(taskID+"\n"), 0o644); err != nil {
		log.Printf("[transcoder] write task link for %s: %v", taskID, err)
	}
}
