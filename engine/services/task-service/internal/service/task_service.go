// Package service provides the TaskService gRPC server implementation.
// 依据: codeaudit_common.proto L858-L883 TaskService 定义
// 依据: 04_工作流设计.md §1 统一状态机 / §2 Saga / §3 四模式流程
// ADR-131: 状态转换单一权威=statemachine 包；ReportStage*/GetTaskProgress/
// UpdateStageStatus/GetTaskContext 为真实现；FAILED→QUEUED 自动重试≤2 接线。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	codeauditcfg "github.com/codeaudit/go-config"
	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/task-service/internal/orchestrator"
	"github.com/codeaudit/services/task-service/internal/statemachine"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// maxAutoRetries — FAILED→QUEUED 自动重试上限（值在全局配置 task.max_auto_retries）。
// 依据: proto L174 "自动重试≤2次"；ADR-137 代码不留缺省。
var maxAutoRetries = mustMaxAutoRetries()

func mustMaxAutoRetries() int {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		panic(fmt.Sprintf("task-service config: %v (ADR-137)", err))
	}
	n, err := cfg.Int("task.max_auto_retries")
	if err != nil {
		panic(fmt.Sprintf("task-service config: %v (ADR-137)", err))
	}
	return n
}

// cancelEntry — R85: 取消器注册条目。defer 注销按 entry 指针身份比对——
// 编排协程在锁内落 DEAD 后、defer 拿锁前，RetryScanTask+StartTask 可完成重注册，
// 身份盲删会把新协程的取消器误删（R75 同型缺陷经重注册窗口回潮）。
type cancelEntry struct{ cancel context.CancelFunc }

// orchExecutor — R75: 编排执行最小接口（*orchestrator.Orchestrator 天然实现）。
// 测试经 s.orch 注入可阻塞桩，真验证"取消打断在途 Execute"——原取消测试用不可达
// 下游的快速失败循环，无论传播是否生效都绿（伪绿，REGRESSIONS R75）。
type orchExecutor interface {
	Execute(ctx context.Context, r orchestrator.RunRequest) (map[string]interface{}, error)
}

// TaskServiceImpl implements the TaskService gRPC service.
type TaskServiceImpl struct {
	pb.UnimplementedTaskServiceServer
	events *TaskEventProducer // ADR-199: Kafka 事件发布器（nil=禁用档）
	resultAddr string          // R64/D5: finding.created 收尾拉取 findings 用
	cancels    map[string]*cancelEntry // R67/R85: taskID→取消器条目（entry 指针身份防重注册窗口跨代误删）
	mu     sync.RWMutex
	tasks  map[string]*pb.ScanTask // task_id -> ScanTask
	idem   map[string]*idemRecord  // request_id -> 幂等记录（03 §2 三态）
	stgIdm map[string]string       // request_id -> 阶段上报指纹（ReportStage 幂等）
	// projectPaths/configs 按任务隔离（ADR-131：修复服务级单例字段被并发任务覆盖的竞态）
	projectPaths map[string]string
	configs      map[string]map[string]string
	contexts     map[string]*pb.TaskContext    // task_id → 编排产出上下文
	logs         map[string][]*pb.TaskLogEntry // task_id → 执行日志环形缓存（ADR-167）
	incrementalCtx map[string]*orchestrator.IncrementalContext // task_id → 增量上下文（ADR-225）
	logIdem      map[string]string             // request_id → log_id（AppendTaskLog 幂等，R4）
	logSeq       int64                         // 日志全局单调序（跨任务分配 log_id）
	sm           *statemachine.StateMachine
	orch         orchExecutor // R75: 编排执行 seam（生产=*orchestrator.Orchestrator）
	hub          *taskWatchHub // ADR-189 任务变更通知（StreamTaskSnapshot 推流源）
	pgStore      *pgTaskStore  // 任务实体 PG 写穿镜像（nil=内存档；R-31 持久化）
	projectAddr  string        // project-service 地址（project_path 兜底查询，ADR-148）
	reposDir     string        // 仓库拉取 clone 根目录（ADR-163）
	cloneTimeout time.Duration // 单次 git clone 上限（ADR-163）
}

// idemRecord — CreateScanTask 幂等记录：同键同体回放，同键异体 ALREADY_EXISTS（03 §2）。
type idemRecord struct {
	fingerprint string
}

// NewTaskService creates a new TaskServiceImpl instance.
// ADR-137: 下游地址与步骤超时来自全局配置（env CODEAUDIT_* 可覆盖），无代码缺省。
// SetEventProducer — 注入 Kafka 事件发布器（ADR-199；nil=禁用档 no-op）。
func (s *TaskServiceImpl) SetEventProducer(p *TaskEventProducer) { s.events = p }

func NewTaskService() *TaskServiceImpl {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		panic(fmt.Sprintf("task-service config: %v (ADR-137)", err))
	}
	must := func(v string, err error) string {
		if err != nil {
			panic(fmt.Sprintf("task-service config: %v (ADR-137)", err))
		}
		return v
	}
	projectAddr := must(cfg.Str("addresses.project", "CODEAUDIT_PROJECT_ADDR"))
	mustInt := func(v int, err error) int {
		if err != nil {
			panic(fmt.Sprintf("task-service config: %v (ADR-137)", err))
		}
		return v
	}
	sastAddr := must(cfg.Str("addresses.sast_adapter", "CODEAUDIT_SAST_ADAPTER_ADDR"))
	dshAddr := must(cfg.Str("addresses.dsh_runtime", "CODEAUDIT_DSH_RUNTIME_ADDR"))
	resultAddr := must(cfg.Str("addresses.result", "CODEAUDIT_RESULT_ADDR"))
	// step_timeouts_s 已整体撤销（ADR-191 补遗）：编排步骤无外层时限。
	reposDir := must(cfg.Str("task.repos_dir", "CODEAUDIT_TASK_REPOS_DIR"))               // ADR-163
	cloneTimeout := time.Duration(mustInt(cfg.Int("task.clone_timeout_s"))) * time.Second // ADR-163
	// ADR-225 D6: 卷缓存回收器配置（桶为 SSOT 前提下的可丢弃缓存；键见 yaml task.repo_cache_*）
	gcEnabled := func() bool {
		v, err := cfg.Bool("task.repo_cache_gc_enabled", "CODEAUDIT_TASK_REPO_CACHE_GC_ENABLED")
		if err != nil {
			panic(fmt.Sprintf("task-service config: %v (ADR-137)", err))
		}
		return v
	}()
	gcInterval := time.Duration(mustInt(cfg.Int("task.repo_cache_gc_interval_s", "CODEAUDIT_TASK_REPO_CACHE_GC_INTERVAL_S"))) * time.Second
	gcTTL := time.Duration(mustInt(cfg.Int("task.repo_cache_ttl_s", "CODEAUDIT_TASK_REPO_CACHE_TTL_S"))) * time.Second
	gcOrphan := time.Duration(mustInt(cfg.Int("task.repo_cache_orphan_ttl_s", "CODEAUDIT_TASK_REPO_CACHE_ORPHAN_TTL_S"))) * time.Second
	gcMax := int64(mustInt(cfg.Int("task.repo_cache_max_bytes", "CODEAUDIT_TASK_REPO_CACHE_MAX_BYTES")))
	s := &TaskServiceImpl{
		tasks:          make(map[string]*pb.ScanTask),
	cancels:        make(map[string]*cancelEntry),
		idem:           make(map[string]*idemRecord),
		stgIdm:         make(map[string]string),
		projectPaths:   make(map[string]string),
		configs:        make(map[string]map[string]string),
		contexts:       make(map[string]*pb.TaskContext),
		logs:           make(map[string][]*pb.TaskLogEntry),
		incrementalCtx: make(map[string]*orchestrator.IncrementalContext),
		logIdem:      make(map[string]string),
		sm:           statemachine.New(),
		hub:          newTaskWatchHub(),
		projectAddr:  projectAddr,
		reposDir:     reposDir,
		cloneTimeout: cloneTimeout,
		orch: orchestrator.New(orchestrator.Config{
			SastAdapterAddr: sastAddr,
			DSHRuntimeAddr:    dshAddr,
			ResultAddr:      resultAddr,
		}),
	resultAddr: resultAddr,
	}
	// R-31: 任务实体 PG 持久化——DSN 非空即启用写穿镜像 + 启动回放；空=内存档（诚实降级）。
	// DSN 已配置但 PG 不可用属 fail-loud（与 ADR-137 配置 panic 同口径）。
	if dsn := must(cfg.Str("task.pg_dsn", "CODEAUDIT_TASK_PG_DSN")); dsn != "" {
		st, err := newPGTaskStore(dsn)
		if err != nil {
			panic(fmt.Sprintf("task-service pg store: %v (R-31)", err))
		}
		s.pgStore = st
		replayed, err := st.hydrateTasks()
		if err != nil {
			panic(fmt.Sprintf("task-service pg hydrate: %v (R-31)", err))
		}
		for _, t := range replayed {
			s.tasks[t.GetTaskId()] = t
		}
		log.Printf("[task-store] PG 持久化启用：回放 %d 个历史任务", len(replayed))
	}
	// ADR-225 D6: 卷缓存对账回收器（三触发线+硬保护，repo_cache.go；ADR-210 同模式）
	go (&repoCacheGC{s: s, enabled: gcEnabled, interval: gcInterval,
		ttl: gcTTL, orphanTTL: gcOrphan, maxBytes: gcMax}).run()
	return s
}

// envOr 保留给项目路径等非配置键场景（CODEAUDIT_PROJECT_REPO_PATH）。
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// transitionLocked 在持有写锁的前提下执行状态转换。
// 依据: 04 §1 状态机图；校验单一权威 = statemachine（ADR-131，消除 service 手写检查的漂移）
// 每次流转同步落执行日志（ADR-167）：GUI 时间线之外的状态流转史。
func (s *TaskServiceImpl) transitionLocked(task *pb.ScanTask, to pb.TaskStatus, rpc string) error {
	if err := s.sm.ValidateTransition(task.Status, to); err != nil {
		return status.Errorf(codes.FailedPrecondition, "cannot %s task in state %s", rpc, task.Status.String())
	}
	from := task.Status
	task.Status = to
	task.UpdatedAt = timestamppb.Now()
	s.appendLogLocked(task.GetTaskId(), pb.TaskLogLevel_TASK_LOG_LEVEL_INFO, "task",
		fmt.Sprintf("状态流转 %s → %s（%s）", from.String(), to.String(), rpc))
	s.persistTaskLocked(task)
	return nil
}

// persistTaskLocked — 任务实体写穿镜像（调用方须持 s.mu；R-31）。
// 错误只记日志：运行期内存仍是权威，持久化降级不反噬任务流。
func (s *TaskServiceImpl) persistTaskLocked(task *pb.ScanTask) {
	if s.pgStore == nil {
		return
	}
	if err := s.pgStore.upsert(task); err != nil {
		log.Printf("[task-store] upsert %s: %v", task.GetTaskId(), err)
	}
}

// cloneLocked — 返回任务深拷贝（proto message 含 sync.Mutex，禁止值拷贝；
// RPC 返回活指针会被编排协程并发变更 → data race，ADR-131 回归修复）。
func cloneLocked(task *pb.ScanTask) *pb.ScanTask {
	return proto.Clone(task).(*pb.ScanTask)
}

// fingerprintCreate — CreateScanTask 请求体指纹（03 §2 同键异体判定）。
// ADR-225: 增量四字段入指纹——同幂等键携带不同增量意图属"同键异体"。
func fingerprintCreate(req *pb.CreateScanTaskRequest) string {
	keys := make([]string, 0, len(req.GetConfig()))
	for k := range req.GetConfig() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var cfg strings.Builder
	for _, k := range keys {
		cfg.WriteString(k + "=" + req.GetConfig()[k] + ";")
	}
	anchor := ""
	if a := req.GetGitAnchor(); a != nil {
		anchor = a.GetCommit() + "," + a.GetBranch()
	}
	return fmt.Sprintf("%s|%s|%s|%s|inc=%v|base=%s|anchor=%s|hint=%d",
		req.GetProjectId(), req.GetScanMode().String(),
		strings.Join(req.GetSastTools(), ","), cfg.String(),
		req.GetIncremental(), req.GetBaselineTaskId(), anchor, len(req.GetDiffHint()))
}

// CreateScanTask creates a new scan task.
// 依据: 04 §1 CREATED 初始状态；03 §2 幂等三态
func (s *TaskServiceImpl) CreateScanTask(ctx context.Context, req *pb.CreateScanTaskRequest) (*pb.ScanTask, error) {
	if req.GetProjectId() == "" {
		return nil, status.Error(codes.InvalidArgument, "project_id is required")
	}
	// R4: 检查幂等键
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}

	requestID := req.GetMetadata().GetRequestId()
	fp := fingerprintCreate(req)

	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, ok := s.idem[requestID]; ok {
		if rec.fingerprint == fp {
			log.Printf("Idempotent replay for task %s", requestID)
			return cloneLocked(s.tasks[requestID]), nil // R48: 回放也出克隆（活引用=调用方可改内部态）
		}
		// 同键异体：按 03 §2 三态规则返回 ALREADY_EXISTS，不重放旧响应
		return nil, status.Errorf(codes.AlreadyExists,
			"request_id %s already used with a different request body (03 §2)", requestID)
	}
	// R68（2026-09-12 待办收尾）：idem 表内存态而任务实体 PG 持久（R-31）——服务重启后
	// 同 request_id 重放（直连 gRPC 客户端重试）会走"新建"覆盖既有任务（状态归 CREATED）。
	// task_id=request_id（03 §2）——任务在即重放：指纹随任务 Config 落库（_idem_fp 内部键）。
	if existing, ok := s.tasks[requestID]; ok {
		if existing.GetConfig()["_idem_fp"] == fp {
			log.Printf("Idempotent replay (post-restart, task persisted) for task %s", requestID)
			return cloneLocked(existing), nil
		}
		return nil, status.Errorf(codes.AlreadyExists,
			"request_id %s already used with a different request body (03 §2)", requestID)
	}

	// ADR-225: 显式基线强契约校验——存在/同项目/COMPLETED 三者齐备，否则创建即
	// InvalidArgument（显式指定是强契约：不静默替换、不自动降级；自动选定才允许降级）。
	if b := req.GetBaselineTaskId(); b != "" {
		bt, ok := s.tasks[b]
		switch {
		case !ok:
			return nil, status.Errorf(codes.InvalidArgument, "baseline_task_id %s not found (ADR-225)", b)
		case bt.GetProjectId() != req.GetProjectId():
			return nil, status.Errorf(codes.InvalidArgument,
				"baseline_task_id %s belongs to project %s (ADR-225)", b, bt.GetProjectId())
		case bt.GetStatus() != pb.TaskStatus_TASK_STATUS_COMPLETED:
			return nil, status.Errorf(codes.InvalidArgument,
				"baseline_task_id %s is %s, not COMPLETED (ADR-225)", b, bt.GetStatus().String())
		}
	}

	cfgSnapshot := make(map[string]string, len(req.GetConfig()))
	for k, v := range req.GetConfig() {
		cfgSnapshot[k] = v
	}
	task := &pb.ScanTask{
		TaskId:    requestID, // task_id=request_id（03 §2 口径；幂等键即任务标识）
		ProjectId: req.GetProjectId(),
		Status:    pb.TaskStatus_TASK_STATUS_CREATED,
		ScanMode:  req.GetScanMode(),
		SastTools: append([]string(nil), req.GetSastTools()...),
		CreatedBy: req.GetCreatedBy(), // ADR-199: 事件通知收件人链（gateway 自 JWT 注入）
		CreatedAt: timestamppb.Now(),
		UpdatedAt: timestamppb.Now(),
		Config:        cfgSnapshot, // ADR-203 补遗: 任务自带 config 快照（proto L1128 config=13，随 GetTask 外露）
		BaselineTaskId: req.GetBaselineTaskId(), // ADR-225: 显式基线快照（自动选定者启动阶段回填）
		GitAnchor:      req.GetGitAnchor(),      // ADR-225 D5: 版本锚点"当时"快照
	}
	// ADR-225: 增量意图 + diff_hint（截断）落 config——随 PG payload 持久、API 可见
	task.Config["_idem_fp"] = fp // R68: 指纹随任务持久（重启后重放判定；内部键 "_" 前缀）
	if req.GetIncremental() {
		task.Config["incremental"] = "true"
		if h := req.GetDiffHint(); h != "" {
			if len(h) > maxDiffHintBytes {
				h = h[:maxDiffHintBytes] + "…(truncated)"
			}
			task.Config["diff_hint"] = h
		}
	}
	// 项目路径按任务登记（ADR-131：不再写服务级单例字段）
	if p, ok := req.GetConfig()["project_path"]; ok {
		s.projectPaths[task.TaskId] = p
	}
	s.configs[task.TaskId] = req.GetConfig()
	s.tasks[task.TaskId] = task
	s.persistTaskLocked(task) // R-31: 创建即落库
	s.idem[requestID] = &idemRecord{fingerprint: fp}
	log.Printf("Created task %s", task.TaskId)
	cp := cloneLocked(task)
	// R48: 事件与返回值都出克隆——异步消费方（Kafka 发布）与后续状态机并发写同一
	// message 是 data race（proto MessageState 非并发安全）
	s.events.PublishAsync("task.created", cp) // ADR-199: 09 §2 task→Kafka 行
	return cp, nil
}

// GetScanTask retrieves a scan task by ID.
func (s *TaskServiceImpl) GetScanTask(ctx context.Context, req *pb.GetScanTaskRequest) (*pb.ScanTask, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[req.GetTaskId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.GetTaskId())
	}
	return cloneLocked(task), nil
}

// SubmitTask transitions task from CREATED to PENDING.
// 依据: 04 §1 CREATED → PENDING (SubmitTask)
func (s *TaskServiceImpl) StartTask(ctx context.Context, req *pb.StartTaskRequest) (*pb.ScanTask, error) {
	s.mu.Lock()
	task, ok := s.tasks[req.GetTaskId()]
	if !ok {
		s.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.GetTaskId())
	}
	if err := s.transitionLocked(task, pb.TaskStatus_TASK_STATUS_RUNNING, "start"); err != nil {
		s.mu.Unlock()
		return nil, err
	}

	// 注册阶段看板（04 §3.x 阶段划分；UpdateStageStatus/GetTaskProgress 消费）
	s.registerStagesLocked(task)
	// ADR-149b: 重试场景清除上次失败残留——每次（重）启动阶段看板归零，
	// 否则成功任务残留上次的 FAILED 阶段/0% 进度，与 COMPLETED 矛盾
	s.resetStagesLocked(task)

	// 快照编排参数（锁内拷贝，避免竞态；项目路径按任务取——ADR-131）
	r := orchestrator.RunRequest{
		TaskID:      task.GetTaskId(),
		RequestID:   "orch-" + task.GetTaskId(),
		ProjectID:   task.GetProjectId(),
		ProjectPath: s.projectPaths[task.GetTaskId()],
		ScanMode:    task.GetScanMode(),
		SastTools:   append([]string(nil), task.GetSastTools()...),
	}
	// ADR-200: storage 上传件通道（原始压缩包经 gateway 直传 storage 不落盘，
	// task 从 storage 拉回解包扫描；解压失败抛压缩包错误）。config.upload_file_id 优先。
	if r.ProjectPath == "" {
		// ADR-209: 读 proto 快照 task.Config（CreateScanTask L209 已写入，GetTask 外露、
		// 重启/持久化随行）而非 s.configs 内存副本——单一事实源，避免双写漂移
		if uploadID := task.GetConfig()["upload_file_id"]; uploadID != "" {
			prepare, msg := s.storagePrepare(task, uploadID)
			if msg != "" {
				task.ErrorMessage = msg // R49: 先赋值再 transition——persist 才带上原因
				_ = s.transitionLocked(task, pb.TaskStatus_TASK_STATUS_FAILED, "start")
				s.finalizeStagesLocked(task, fmt.Errorf("%s", msg)) // R63: 早失败阶段收敛（原悬挂全 PENDING）
				s.mu.Unlock()
				return nil, status.Error(codes.FailedPrecondition, msg)
			}
			r.Prepare = prepare
		}
	}
	// R47: 项目侧解析链（项目 config.upload_file_id → repo_url clone 兜底）需 project-service
	// RPC，移到锁外执行——此前持 s.mu 拨 gRPC（fetchProjectConfigValue/fetchProjectRepo），
	// project-service 慢/挂时 StartTask 卡锁，全服务任务面（创建/列表/快照/流）一并冻结。
	// 锁内只留快照，RPC 后重锁校验状态未变再应用结果。
	needProjectLookup := r.ProjectPath == "" && r.Prepare == nil && task.GetProjectId() != ""
	projectID := task.GetProjectId()
	s.mu.Unlock()

	var projUploadID, repoURL, repoBranch, projFetchErr string
	if needProjectLookup {
		uid, cfgErr := s.fetchProjectConfigValueErr(projectID, "upload_file_id") // R63: 失败原因带回
		projUploadID = uid
		projFetchErr = cfgErr
		url, branch, gerr := s.fetchProjectRepo(projectID)
		if gerr != nil {
			projFetchErr = fmt.Sprintf("项目配置读取失败（repo_url 兜底不可用）: %v", gerr)
		} else {
			repoURL, repoBranch = url, branch
		}
	}

	s.mu.Lock()
	task, ok = s.tasks[req.GetTaskId()]
	if !ok {
		s.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.GetTaskId())
	}
	if task.GetStatus() != pb.TaskStatus_TASK_STATUS_RUNNING {
		// RPC 窗口内被 Cancel/Pause 等转移——放弃启动编排，如实返回当前态
		cp := cloneLocked(task)
		s.mu.Unlock()
		log.Printf("[task %s] start aborted: status changed during project lookup to %s", req.GetTaskId(), task.GetStatus())
		return cp, nil
	}
	// ADR-203: 项目级上传件兜底——项目弹窗上传（人类 2026-09-05 裁决"保留入口并改造"）经
	// gateway 零落盘直传 storage，upload_file_id 存项目 config；任务未携带上传件时从
	// 项目配置解析并**快照回写任务 config**（审核意见①：项目持"当前"指针，任务持"当时"
	// 快照——创建/启动间项目重传或 DEAD 重试前重传均不漂移，报告可回答"扫的是哪份包"）。
	// 解析链（ADR-203 补遗收口）：任务 config.upload_file_id → 项目 config.upload_file_id → repo_url clone。
	if needProjectLookup && projUploadID != "" {
		prepare, msg := s.storagePrepare(task, projUploadID)
		if msg != "" {
			task.ErrorMessage = msg // R49: 先赋值再 transition——persist 才带上原因
			_ = s.transitionLocked(task, pb.TaskStatus_TASK_STATUS_FAILED, "start")
			s.finalizeStagesLocked(task, fmt.Errorf("%s", msg)) // R63: 早失败阶段收敛
			s.mu.Unlock()
			return nil, status.Error(codes.FailedPrecondition, msg)
		}
		r.Prepare = prepare
		// 快照回写（proto ScanTask.config 与进程内 configs 双写；仅在任务尚未自带时写，
		// 任务自带指针者本就是自包含快照，重试语义不受项目后续变化影响）
		if task.GetConfig()["upload_file_id"] == "" {
			if task.Config == nil {
				task.Config = map[string]string{}
			}
			task.Config["upload_file_id"] = projUploadID
		}
		if s.configs[task.GetTaskId()] == nil {
			s.configs[task.GetTaskId()] = map[string]string{}
		}
		if s.configs[task.GetTaskId()]["upload_file_id"] == "" {
			s.configs[task.GetTaskId()]["upload_file_id"] = projUploadID
		}
		log.Printf("[task %s] storage mode via project config upload: %s (snapshot into task config)", task.GetTaskId(), projUploadID)
	}
	// ADR-163: 仓库拉取模式——路径仍缺省且项目配置 repo_url 时，编排协程内前置 git clone
	// （不阻塞 StartTask RPC；clone 失败走编排既有 FAILED→QUEUED 重试→DEAD 链，错误含 git 输出）。
	// ADR-209: 守卫必须含 r.Prepare == nil——repo clone 是解析链第三档**兜底**（任务级/项目级
	// upload_file_id 已解析出 storage 拉包闭包时，此处不得覆盖；自 ADR-200 起缺此条件导致
	// 优先级倒置，任务级上传件被静默换成 git clone）。
	if needProjectLookup && r.Prepare == nil && repoURL != "" {
		dest := filepath.Join(s.reposDir, task.GetTaskId())
		timeout := s.cloneTimeout
		r.Prepare = func(ctx context.Context) (string, error) {
			log.Printf("[task %s] repo mode: cloning %s (branch=%s)", task.GetTaskId(), repoURL, repoBranch)
			return cloneRepo(ctx, repoURL, repoBranch, dest, timeout)
		}
	}
	if r.ProjectPath == "" && r.Prepare == nil {
		// 明确失败：不空跑（此前回退 tests/samples 会让陌生项目扫到无关代码）
		// R47: fetchProjectRepo 失败不再静默——真实原因追加进 ErrorMessage（暂态 RPC 失败
		// 与终态"未配置"同走 FAILED 诚实路径，但排障可见差异）
		msg := "project_path 未配置：请上传代码压缩包或为项目配置 repo_url（ADR-148/ADR-163）"
		if projFetchErr != "" {
			msg += "；" + projFetchErr
		}
		task.ErrorMessage = msg // R49: 先赋值再 transition——persist 才带上原因
		_ = s.transitionLocked(task, pb.TaskStatus_TASK_STATUS_FAILED, "start")
		s.finalizeStagesLocked(task, fmt.Errorf("%s", msg)) // R63: 早失败阶段收敛
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, msg)
	}
	repoPath := os.Getenv("CODEAUDIT_PROJECT_REPO_PATH")
	if repoPath != "" {
		r.ProjectPath = repoPath // 环境变量显式覆盖（E2E/CI 口径保留）
	}
	// ADR-225 D6: 源码树持久层——Prepare 产出剥壳根后异步树 tar 入桶（全量/增量
	// 一视同仁：桶是 SSOT，卷降级为可丢弃缓存）；失败不阻塞任务主流程。
	// 静态路径（Prepare==nil）也包一层，让 Execute 统一经 Prepare 产出根。
	{
		bp, sp := r.Prepare, r.ProjectPath
		taskID := task.GetTaskId()
		r.Prepare = func(ctx context.Context) (string, error) {
			var p string
			var err error
			if bp != nil {
				p, err = bp(ctx)
			} else {
				p = sp
			}
			if err != nil {
				return "", err
			}
			go s.uploadTreeTar(taskID, p)
			return p, nil
		}
	}
	// ADR-225: 增量意图装配——包装既有 Prepare/静态路径，解包产出新树根后跑增量解析
	//（基线选定/内容 diff/快照回写/降级，见 incremental.go）。全量任务零行为差异。
	if task.GetConfig()["incremental"] == "true" {
		inc := &orchestrator.IncrementalContext{}
		r.Incremental = inc
		s.incrementalCtx[task.GetTaskId()] = inc
		r.Prepare = s.wrapIncrementalPrepare(task.GetTaskId(), r.Prepare, r.ProjectPath)
	}
	orch := s.orch
	recorder := s.stageRecorder(task.GetTaskId())
	// R67（2026-09-12 待办收尾）：取消传播——编排协程用可取消 ctx（原恒 Background，
	// CancelScanTask 只改状态不通知在途编排，阻塞 RPC/重试循环继续跑完）
	orchCtx, orchCancel := context.WithCancel(context.Background())
	entry := &cancelEntry{cancel: orchCancel}
	s.cancels[task.GetTaskId()] = entry
	s.mu.Unlock()

	go s.runOrchestration(orchCtx, entry, orch, r, recorder)

	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := proto.Clone(s.tasks[req.GetTaskId()]).(*pb.ScanTask) // 深拷贝，禁止复制含锁的 MessageState
	return cp, nil
}

// emitIncrementalScopeNotice — A6.3 视野声明（验收 F6）：增量任务执行日志明示
// "以文件为边界"的精度口径（taint 类跨文件规则可能漏报跨文件数据流）。
// 零变更任务零扫全继承，无扫描即无此声明。
func (s *TaskServiceImpl) emitIncrementalScopeNotice(taskID string, inc *orchestrator.IncrementalContext) {
	if inc == nil {
		return
	}
	if active, _, changed, _, _ := inc.Snapshot(); active && len(changed) > 0 {
		s.emitTaskLog(taskID, pb.TaskLogLevel_TASK_LOG_LEVEL_WARN, "task",
			fmt.Sprintf("增量扫描以文件为边界（%d 个变更文件）：taint 类跨文件规则可能漏报跨文件数据流，需全量精度请选全量", len(changed)))
	}
}

// runOrchestration — 执行编排并处理终态与自动重试。
// 依据: 04 §1 RUNNING→FAILED→QUEUED 自动重试≤2→耗尽 DEAD（proto L174/L177）
func (s *TaskServiceImpl) runOrchestration(orchCtx context.Context, entry *cancelEntry, orch orchExecutor, r orchestrator.RunRequest, recorder orchestrator.StageRecorder) {
	// ADR-181 修复：Recorder 此前创建了却从未挂到 RunRequest——阶段事件从未到达
	// 阶段看板（时间线全程静止，终态靠 finalize 盖章；人类反馈"没有中间态"的根因）。
	r.Recorder = recorder
	// ADR-149b: 每次尝试独立幂等命名空间——重试复用同一 request_id 会让适配器
	// 返回失败尝试的缓存结果（其发现已被补偿删除），产出空报告的"假成功"。
	baseID := r.RequestID
	attempt := 0
	// R75: 原入口处 delete(s.cancels, ...) 已删——它注销的正是 StartTask 刚注册的本次
	// 取消器（注册→协程首步删除），此后 CancelScanTask 恒 miss、orchCtx 永不取消，
	// R67 传播链死代码。退出注销由下方 defer 承担；R85 起 defer 按 entry 身份比对——
	// 本协程锁内落 DEAD 后、defer 拿锁前，Retry→StartTask 可重注册新条目，身份盲删
	// 会误删新协程的取消器（重注册窗口 TOCTOU，复审发现）。
	defer func() {
		s.mu.Lock()
		if s.cancels[r.TaskID] == entry {
			delete(s.cancels, r.TaskID)
		}
		s.mu.Unlock()
	}()
	for {
		select {
		case <-orchCtx.Done(): // R67: 任务已被取消——不再发起下一次尝试（状态由 CancelTask 落 CANCELLED）
			return
		default:
		}
		r.RequestID = fmt.Sprintf("%s-a%d", baseID, attempt)
		attempt++
		summary, err := orch.Execute(orchCtx, r)
		s.mu.Lock()
		cur, ok := s.tasks[r.TaskID]
		if !ok || cur.Status != pb.TaskStatus_TASK_STATUS_RUNNING { // 被取消等迁移则不覆盖
			s.mu.Unlock()
			return
		}
		if err == nil {
			if terr := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_COMPLETED, "complete"); terr != nil {
				log.Printf("[task %s] complete transition: %v", r.TaskID, terr)
			}
			s.storeContextLocked(cur, r, summary)
			s.finalizeStagesLocked(cur, nil)
			s.events.PublishAsync("task.completed", cur) // ADR-199: 终态事件
			createdBy := cur.GetCreatedBy()
			s.mu.Unlock()
			s.publishHighSeverityFindings(r.TaskID, createdBy) // R64/D5: 高危发现通知（锁外，非致命）
			return
		}

		log.Printf("[task %s] orchestration failed (retry_count=%d): %v", r.TaskID, cur.GetRetryCount(), err)
		if int(cur.GetRetryCount()) < maxAutoRetries {
			// FAILED→QUEUED 自动重试（proto L174）
			cur.ErrorMessage = err.Error() // R49: 先赋值再 transition——persist 才带上原因
			if terr := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_FAILED, "fail"); terr != nil {
				log.Printf("[task %s] fail transition: %v", r.TaskID, terr)
			}
			cur.RetryCount++
			if terr := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_QUEUED, "auto-retry"); terr != nil {
				log.Printf("[task %s] auto-retry transition: %v", r.TaskID, terr)
				s.mu.Unlock()
				return
			}
			if terr := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_RUNNING, "start"); terr != nil {
				log.Printf("[task %s] re-start transition: %v", r.TaskID, terr)
				s.mu.Unlock()
				return
			}
			s.finalizeStagesLocked(cur, err)
			s.mu.Unlock()
			continue
		}
		// 重试耗尽 → DEAD（proto L177；经 FAILED 过渡）
		cur.ErrorMessage = err.Error() // R49: 先赋值再 transition——persist 才带上原因
		if terr := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_FAILED, "fail"); terr != nil {
			log.Printf("[task %s] fail transition: %v", r.TaskID, terr)
		}
		if terr := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_DEAD, "retry-exhausted"); terr != nil {
			log.Printf("[task %s] retry-exhausted transition: %v", r.TaskID, terr)
		}
		s.finalizeStagesLocked(cur, err)
		s.events.PublishAsync("task.completed", cur) // ADR-199: DEAD 以 FAILED 语义进通知（payload.status=DEAD）
		s.mu.Unlock()
		return
	}
}

// registerStagesLocked — 按扫描模式预注册阶段（ADR-186 五模式矩阵 + 旧模式兼容）。
// StageType 枚举依据: proto L545-L554
func (s *TaskServiceImpl) registerStagesLocked(task *pb.ScanTask) {
	if len(task.GetStages()) > 0 {
		return
	}
	var stages []*pb.TaskStage
	add := func(id string, typ pb.StageType) {
		// ADR-212: Metadata 就地初始化——ReportStageComplete 会写 output_refs，
		// 注册阶段不带 map 时对 nil map 赋值 panic（grpc-go 无内建 recover=杀进程）
		stages = append(stages, &pb.TaskStage{StageId: id, Type: typ, Status: pb.StageStatus_STAGE_STATUS_PENDING,
			Metadata: map[string]string{}})
	}
	switch task.GetScanMode() {
	case pb.ScanMode_SCAN_MODE_SAST_ONLY: // 模式A（ADR-182）：纯SAST→去重合并→报告
		add("sast", pb.StageType_STAGE_TYPE_SAST_SCAN)
		add("fusion", pb.StageType_STAGE_TYPE_RESULT_FUSION)
		add("report", pb.StageType_STAGE_TYPE_REPORT_GENERATION)
	case pb.ScanMode_SCAN_MODE_AI_ONLY: // 模式B（ADR-182）：纯AI
		add("analyze", pb.StageType_STAGE_TYPE_CODE_ANALYSIS)
		add("ai", pb.StageType_STAGE_TYPE_AI_INFERENCE)
		add("report", pb.StageType_STAGE_TYPE_REPORT_GENERATION)
	case pb.ScanMode_SCAN_MODE_AI_ENHANCED_SAST: // 模式D（ADR-186）：AI增强SAST——sast→ai(验证)→fusion→report
		add("sast", pb.StageType_STAGE_TYPE_SAST_SCAN)
		add("ai", pb.StageType_STAGE_TYPE_AI_INFERENCE)
		add("fusion", pb.StageType_STAGE_TYPE_RESULT_FUSION)
		add("report", pb.StageType_STAGE_TYPE_REPORT_GENERATION)
	case pb.ScanMode_SCAN_MODE_PARALLEL, pb.ScanMode_SCAN_MODE_COMPARE: // 模式C 融合 / 模式E 对比（共用并行审计段；E 原称模式D，ADR-186）
		add("sast", pb.StageType_STAGE_TYPE_SAST_SCAN)
		add("ai", pb.StageType_STAGE_TYPE_AI_INFERENCE)
		add("fusion", pb.StageType_STAGE_TYPE_RESULT_FUSION)
		add("report", pb.StageType_STAGE_TYPE_REPORT_GENERATION)
	case pb.ScanMode_SCAN_MODE_TRADITIONAL_FIRST: // 旧模式B（已弃用，历史兼容）
		add("sast", pb.StageType_STAGE_TYPE_SAST_SCAN)
		add("analyze", pb.StageType_STAGE_TYPE_CODE_ANALYSIS)
		add("ai", pb.StageType_STAGE_TYPE_AI_INFERENCE)
		add("fusion", pb.StageType_STAGE_TYPE_RESULT_FUSION)
		add("report", pb.StageType_STAGE_TYPE_REPORT_GENERATION)
	case pb.ScanMode_SCAN_MODE_SAST_REVIEW: // 旧模式D（已弃用，历史兼容）
		add("sast", pb.StageType_STAGE_TYPE_SAST_SCAN)
		add("review", pb.StageType_STAGE_TYPE_AI_REVIEW)
		add("report", pb.StageType_STAGE_TYPE_REPORT_GENERATION)
	}
	task.Stages = stages
}

// stageRecorder — 把编排器阶段事件映射到阶段看板（事件键→stage_id 见 stageEventStageID）。
// 首个事件将 PENDING 阶段置 RUNNING；"done:<stage_id>" 事件把阶段实时置 COMPLETED
// （ADR-181：此前终态统一由 finalizeStagesLocked 在编排全部结束后盖章，运行中
// 时间线长时间静止——15 分钟沙箱审计期间无任何中间态，人类反馈 2026-09-02）。
func (s *TaskServiceImpl) stageRecorder(taskID string) orchestrator.StageRecorder {
	return func(eventKey, msg string) {
		if id, ok := strings.CutPrefix(eventKey, "done:"); ok {
			s.completeStage(taskID, id, msg)
			return
		}
		id := stageEventStageID(eventKey)
		if id == "" {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		task, ok := s.tasks[taskID]
		if !ok {
			return
		}
		st := findOrInsertStageLocked(task, id)
		// R57: 事件 msg 落 metadata（进度行与 "[降级]" 标记进看板——此前整体丢弃，
		// AI 降级 RuleScan 兜底时前端阶段照样绿勾，用户误以为 AI 真跑完）
		if msg != "" {
			if st.Metadata == nil {
				st.Metadata = map[string]string{}
			}
			st.Metadata["message"] = msg
			if strings.HasPrefix(msg, "[降级]") {
				st.Metadata["degraded"] = "true"
			}
		}
		if st.Status == pb.StageStatus_STAGE_STATUS_PENDING {
			st.Status = pb.StageStatus_STAGE_STATUS_RUNNING
			st.StartedAt = timestamppb.Now()
			s.hub.notify(taskID) // ADR-189
		}
	}
}

// completeStage — 实时完成阶段（已终态则幂等跳过；未启动过的阶段补 StartedAt）。
// R57: 收尾 msg 落 metadata["message"]（降级标记 metadata["degraded"] 不被覆盖）。
func (s *TaskServiceImpl) completeStage(taskID, stageID, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return
	}
	for _, st := range task.GetStages() {
		if st.GetStageId() != stageID {
			continue
		}
		if st.Status == pb.StageStatus_STAGE_STATUS_COMPLETED ||
			st.Status == pb.StageStatus_STAGE_STATUS_FAILED {
			return
		}
		now := timestamppb.Now()
		if st.StartedAt == nil {
			st.StartedAt = now
		}
		st.Status = pb.StageStatus_STAGE_STATUS_COMPLETED
		st.CompletedAt = now
		if msg != "" {
			if st.Metadata == nil {
				st.Metadata = map[string]string{}
			}
			st.Metadata["message"] = msg
		}
		s.hub.notify(taskID) // ADR-189
		return
	}
}

// stageEventStageID — 编排器事件键 → 阶段看板 stage_id。
func stageEventStageID(key string) string {
	switch {
	case key == "analyze":
		return "analyze"
	case key == "scans":
		return "sast"
	case key == "ai", key == "verify", key == "missed":
		return "ai"
	case key == "review":
		return "review"
	case key == "compare", key == "fusion", key == "S7":
		return "fusion"
	case strings.Contains(key, "report"):
		return "report"
	default:
		return ""
	}
}

// finalizeStagesLocked — 编排收尾：未终态的阶段按结果落终态。
// ADR-149b 语义细分：失败时，已启动的阶段=FAILED；从未启动的阶段=SKIPPED
// （此前一律 FAILED，把"没跑到的阶段"也标成失败，时间线失真）。
func (s *TaskServiceImpl) finalizeStagesLocked(task *pb.ScanTask, orchErr error) {
	now := timestamppb.Now()
	for _, st := range task.GetStages() {
		if st.Status == pb.StageStatus_STAGE_STATUS_COMPLETED ||
			st.Status == pb.StageStatus_STAGE_STATUS_FAILED {
			continue
		}
		if orchErr != nil {
			if st.StartedAt == nil {
				st.Status = pb.StageStatus_STAGE_STATUS_SKIPPED
			} else {
				st.Status = pb.StageStatus_STAGE_STATUS_FAILED
				st.ErrorMessage = orchErr.Error()
			}
		} else {
			st.Status = pb.StageStatus_STAGE_STATUS_COMPLETED
		}
		if st.StartedAt == nil {
			st.StartedAt = now
		}
		st.CompletedAt = now
	}
	s.hub.notify(task.GetTaskId()) // ADR-189
}

// resetStagesLocked — （重）启动前阶段看板归零（ADR-149b）。
func (s *TaskServiceImpl) resetStagesLocked(task *pb.ScanTask) {
	for _, st := range task.GetStages() {
		st.Status = pb.StageStatus_STAGE_STATUS_PENDING
		st.ErrorMessage = ""
		st.StartedAt = nil
		st.CompletedAt = nil
	}
	s.hub.notify(task.GetTaskId()) // ADR-189
}

// storeContextLocked — 编排 summary → TaskContext（GetTaskContext 消费）。
func (s *TaskServiceImpl) storeContextLocked(task *pb.ScanTask, r orchestrator.RunRequest, summary map[string]interface{}) {
	cfgJson := "{}"
	if cfg := s.configs[task.GetTaskId()]; cfg != nil {
		if b, err := json.Marshal(cfg); err == nil {
			cfgJson = string(b)
		}
	}
	tc := &pb.TaskContext{
		TaskId:            task.GetTaskId(),
		ProjectConfigJson: cfgJson,
	}
	if v, ok := summary["cpg_storage_path"].(string); ok {
		tc.CpgStoragePath = v
	}
	if ids, ok := summary["sast_finding_ids"].([]string); ok {
		tc.SastFindingIds = ids
	}
	if ids, ok := summary["ai_finding_ids"].([]string); ok {
		tc.AiFindingIds = ids
	}
	s.contexts[task.GetTaskId()] = tc
}

// CompleteTask transitions task from RUNNING to COMPLETED.
// 依据: 04 §1 RUNNING → COMPLETED (CompleteTask)
func (s *TaskServiceImpl) CompleteTask(ctx context.Context, req *pb.CompleteTaskRequest) (*pb.ScanTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[req.GetTaskId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.GetTaskId())
	}
	if err := s.transitionLocked(task, pb.TaskStatus_TASK_STATUS_COMPLETED, "complete"); err != nil {
		return nil, err
	}
	return cloneLocked(task), nil
}

// FailTask transitions task from RUNNING to FAILED; retryable failures auto-retry.
// 依据: 04 §1 RUNNING → FAILED；proto L174 FAILED→QUEUED 自动重试≤2；proto L177 耗尽→DEAD
func (s *TaskServiceImpl) FailTask(ctx context.Context, req *pb.FailTaskRequest) (*pb.ScanTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[req.GetTaskId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.GetTaskId())
	}
	if err := s.transitionLocked(task, pb.TaskStatus_TASK_STATUS_FAILED, "fail"); err != nil {
		return nil, err
	}
	task.ErrorMessage = req.GetErrorMessage()
	if msg := req.GetErrorMessage(); msg != "" {
		s.appendLogLocked(task.GetTaskId(), pb.TaskLogLevel_TASK_LOG_LEVEL_ERROR, "orchestrator", msg)
	}

	if req.GetRetryable() && int(task.GetRetryCount()) < maxAutoRetries {
		task.RetryCount++
		if err := s.transitionLocked(task, pb.TaskStatus_TASK_STATUS_QUEUED, "auto-retry"); err != nil {
			return nil, err
		}
		return cloneLocked(task), nil // 调用方此后重新 StartTask（外部驱动口径）
	}
	if int(task.GetRetryCount()) >= maxAutoRetries {
		if err := s.transitionLocked(task, pb.TaskStatus_TASK_STATUS_DEAD, "retry-exhausted"); err != nil {
			return nil, err
		}
	}
	return cloneLocked(task), nil
}

// CancelScanTask transitions task to CANCELLED.
// 依据: 04 §1 "任何状态可取消"；终态无出边由 statemachine 判定（ADR-131 单一权威）
func (s *TaskServiceImpl) CancelScanTask(ctx context.Context, req *pb.CancelScanTaskRequest) (*pb.ScanTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[req.GetTaskId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.GetTaskId())
	}
	if err := s.transitionLocked(task, pb.TaskStatus_TASK_STATUS_CANCELLED, "cancel"); err != nil {
		return nil, err
	}
	if e, ok := s.cancels[req.GetTaskId()]; ok { // R67/R85: 取消传播到在途编排
		delete(s.cancels, req.GetTaskId())
		e.cancel()
	}
	return cloneLocked(task), nil
}

// RetryScanTask transitions task from DEAD to QUEUED (human retry).
// 依据: proto L863 "DEAD状态人工重试入口"（该边在 statemachine 中按设计注释未注册，此处显式校验）
func (s *TaskServiceImpl) RetryScanTask(ctx context.Context, req *pb.RetryScanTaskRequest) (*pb.ScanTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[req.GetTaskId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.GetTaskId())
	}
	if task.Status != pb.TaskStatus_TASK_STATUS_DEAD {
		return nil, status.Errorf(codes.FailedPrecondition,
			"cannot retry task in state %s (only DEAD)", task.Status.String())
	}
	task.Status = pb.TaskStatus_TASK_STATUS_QUEUED
	task.UpdatedAt = timestamppb.Now()
	task.ErrorMessage = ""
	s.persistTaskLocked(task) // R-31: 重试回 QUEUED 也落库
	return cloneLocked(task), nil
}

// PauseTask — ADR-200: RUNNING → PAUSED，AI 交互会话回合闸门挂起。
// 顺序纪律: 先挂 dsh-runtime 闸门（fail-safe：闸门先扣上，状态转换失败也只是
// 多暂停一个无会话任务），再转状态。
func (s *TaskServiceImpl) PauseTask(ctx context.Context, req *pb.PauseTaskRequest) (*pb.ScanTask, error) {
	taskID := req.GetTaskId()
	s.mu.RLock()
	_, ok := s.tasks[taskID]
	s.mu.RUnlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %s not found", taskID)
	}

	// 闸门先行（no-op 安全：AI 阶段未运行时只是预约，且立即被下述转换校验兜住）
	s.dshControl(ctx, "PauseAnalysis", &pb.PauseAnalysisRequest{TaskId: taskID})

	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.tasks[taskID]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %s not found", taskID)
	}
	if err := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_PAUSED, "pause"); err != nil {
		return nil, err
	}
	cur.UpdatedAt = timestamppb.Now()
	log.Printf("[task %s] paused (AI session gate engaged)", taskID)
	return cloneLocked(cur), nil
}

// ResumeTask — ADR-200: PAUSED → RUNNING，会话继续。
// 顺序纪律与 PauseTask 相反: 先转状态（PAUSED→RUNNING 合法性校验），后释放闸门
// （转换失败时闸门保持扣上——宁可不恢复，不可"状态没恢复但 AI 在跑"）。
func (s *TaskServiceImpl) ResumeTask(ctx context.Context, req *pb.ResumeTaskRequest) (*pb.ScanTask, error) {
	taskID := req.GetTaskId()
	s.mu.Lock()
	task, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "task %s not found", taskID)
	}
	if err := s.transitionLocked(task, pb.TaskStatus_TASK_STATUS_RUNNING, "resume"); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	task.UpdatedAt = timestamppb.Now()
	pausedAt := task.UpdatedAt.AsTime()
	s.mu.Unlock()

	s.dshControl(ctx, "ResumeAnalysis", &pb.ResumeAnalysisRequest{TaskId: taskID})
	log.Printf("[task %s] resumed (gate released, paused at %s)", taskID, pausedAt.Format(time.TimeOnly))
	return func() *pb.ScanTask { s.mu.RLock(); defer s.mu.RUnlock(); return cloneLocked(task) }(), nil
}

// dshControl — dsh-runtime 会话控制调用（尽力而为：地址未配/不可达仅 WARN——
// 闸门语义由 ADR-200 的状态机与调用序保证，不阻塞暂停/恢复主路径）。
func (s *TaskServiceImpl) dshControl(ctx context.Context, method string, req proto.Message) {
	addr := envOr("CODEAUDIT_DSH_RUNTIME_ADDR", "localhost:50057")
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("[task %s] dsh dial %s: %v", method, addr, err)
		return
	}
	defer conn.Close()
	resp := &emptypb.Empty{}
	if err := conn.Invoke(ctx, "/codeaudit.common.v1.DSHRuntimeService/"+method, req, resp); err != nil {
		log.Printf("[task %s] dsh %s: %v (WARN)", method, addr, err)
	}
}

// ListScanTasks lists scan tasks with stable ordering and pagination.
// 依据: 03 §5 分页规范（稳定序：created_at 升序 + task_id 决胜，消除 map 随机序）
func (s *TaskServiceImpl) ListScanTasks(ctx context.Context, req *pb.ListScanTasksRequest) (*pb.ListScanTasksResponse, error) {
	s.mu.RLock()
	tasks := make([]*pb.ScanTask, 0, len(s.tasks))
	for _, t := range s.tasks {
		tasks = append(tasks, cloneLocked(t)) // R48: 出锁克隆——锁外排序/过滤/分页期间编排协程会并发写
	}
	s.mu.RUnlock()

	// ADR-160: 契约 L1108-1112 的 project_id 与 filter 过滤真实生效（此前恒忽略——
	// 列表无法按项目/模式隔离, 测试数据与演示数据混排）。过滤先于排序/游标分页, 语义才正确。
	if pid := req.GetProjectId(); pid != "" {
		kept := tasks[:0]
		for _, t := range tasks {
			if t.GetProjectId() == pid {
				kept = append(kept, t)
			}
		}
		tasks = kept
	}
	if conds := req.GetFilter().GetConditions(); len(conds) > 0 {
		op := req.GetFilter().GetOperator()
		if op != pb.LogicalOperator_LOGICAL_OPERATOR_UNSPECIFIED &&
			op != pb.LogicalOperator_LOGICAL_OPERATOR_AND &&
			op != pb.LogicalOperator_LOGICAL_OPERATOR_OR {
			return nil, status.Errorf(codes.InvalidArgument, "unsupported logical operator %s (03 §5)", op)
		}
		taskValue := func(t *pb.ScanTask, field string) (string, bool) {
			switch field {
			case "scan_mode":
				return t.GetScanMode().String(), true
			case "status":
				return t.GetStatus().String(), true
			default:
				return "", false
			}
		}
		matchOne := func(t *pb.ScanTask, c *pb.FilterCondition) (bool, error) {
			cur, known := taskValue(t, c.GetField())
			if !known {
				return false, status.Errorf(codes.InvalidArgument, "unsupported filter field %q (supported: scan_mode, status)", c.GetField())
			}
			switch c.GetOperator() {
			case pb.FilterOperator_FILTER_OPERATOR_EQ:
				return cur == c.GetValue(), nil
			case pb.FilterOperator_FILTER_OPERATOR_NEQ:
				return cur != c.GetValue(), nil
			default:
				return false, status.Errorf(codes.InvalidArgument, "unsupported filter operator %s for field %q (supported: EQ, NEQ)", c.GetOperator(), c.GetField())
			}
		}
		filtered := tasks[:0]
		for _, t := range tasks {
			if op == pb.LogicalOperator_LOGICAL_OPERATOR_OR {
				any := false
				for _, c := range conds {
					if m, err := matchOne(t, c); err != nil {
						return nil, err
					} else if m {
						any = true
					}
				}
				if any {
					filtered = append(filtered, t)
				}
			} else { // UNSPECIFIED 缺省=AND（proto 枚举 0 值语义）
				all := true
				for _, c := range conds {
					if m, err := matchOne(t, c); err != nil {
						return nil, err
					} else if !m {
						all = false
					}
				}
				if all {
					filtered = append(filtered, t)
				}
			}
		}
		tasks = filtered
	}

	// ADR-149: 与报告中心同一套排序语义——"最新活动优先"。
	// 列表展示列是"更新时间"，排序键=updated_at 降序（与报告中心"最新生成优先"一致），
	// task_id 决胜保证稳定序（03 §5）。
	// R68（2026-09-12 待办收尾）：排序键改 created_at——不可变，offset 游标页间稳定；
	// 原按 updated_at（高频变更）翻页期间任务跨页边界移动 → 重复/跳页。
	sort.Slice(tasks, func(i, j int) bool {
		ti, tj := tasks[i].GetCreatedAt().AsTime(), tasks[j].GetCreatedAt().AsTime()
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return tasks[i].GetTaskId() < tasks[j].GetTaskId()
	})

	pageSize := int(req.GetPagination().GetPageSize())
	if pageSize <= 0 {
		pageSize = 20 // 依据: proto L227 "默认20，最大100"
	}
	if pageSize > 100 {
		pageSize = 100
	}
	offset := 0
	if cur := req.GetPagination().GetCursor(); cur != "" {
		n, err := strconv.Atoi(cur)
		if err != nil || n < 0 {
			return nil, status.Error(codes.InvalidArgument, "invalid cursor (03 §5)")
		}
		offset = n
	}
	if offset > len(tasks) {
		offset = len(tasks)
	}
	end := offset + pageSize
	if end > len(tasks) {
		end = len(tasks)
	}
	resp := &pb.ListScanTasksResponse{Tasks: tasks[offset:end]}
	if end < len(tasks) {
		resp.Pagination = &pb.PaginationResponse{
			NextCursor: strconv.Itoa(end), // 稳定偏移游标（列表有序，ADR-131）
			HasNext:    true,
			Total:      int32(len(tasks)),
		}
	}
	return resp, nil
}

// UpdateStageStatus updates the status of a specific stage.
// 依据: proto L1124 UpdateStageStatusRequest / TaskStage L530
func (s *TaskServiceImpl) UpdateStageStatus(ctx context.Context, req *pb.UpdateStageStatusRequest) (*pb.ScanTask, error) {
	if req.GetTaskId() == "" || req.GetStageId() == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id and stage_id are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[req.GetTaskId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.GetTaskId())
	}
	st := findOrInsertStageLocked(task, req.GetStageId())
	st.Status = req.GetStatus()
	st.ErrorMessage = req.GetErrorMessage()
	now := timestamppb.Now()
	switch req.GetStatus() {
	case pb.StageStatus_STAGE_STATUS_RUNNING:
		if st.StartedAt == nil {
			st.StartedAt = now
		}
	case pb.StageStatus_STAGE_STATUS_COMPLETED, pb.StageStatus_STAGE_STATUS_FAILED, pb.StageStatus_STAGE_STATUS_SKIPPED:
		if st.StartedAt == nil {
			st.StartedAt = now
		}
		st.CompletedAt = now
	}
	task.UpdatedAt = now
	s.hub.notify(req.GetTaskId()) // ADR-189
	return cloneLocked(task), nil
}

// GetTaskProgress retrieves the progress of a task.
// 依据: proto TaskProgress L1163（overall_percent=已完成阶段/总阶段）
func (s *TaskServiceImpl) GetTaskProgress(ctx context.Context, req *pb.GetTaskProgressRequest) (*pb.TaskProgress, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[req.GetTaskId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.GetTaskId())
	}
	return progressOf(task), nil
}

// progressOf — 从任务态派生进度（ADR-189 起 StreamTaskSnapshot 与本 RPC 共用）。
// 调用方持 s.mu 读锁或已持有任务克隆。
func progressOf(task *pb.ScanTask) *pb.TaskProgress {
	total := len(task.GetStages())
	done := 0
	for _, st := range task.GetStages() {
		if st.Status == pb.StageStatus_STAGE_STATUS_COMPLETED || st.Status == pb.StageStatus_STAGE_STATUS_SKIPPED {
			done++
		}
	}
	var pct float32
	if total > 0 {
		pct = float32(done) / float32(total) * 100
	}
	// R48: Stages 深拷贝出锁——原引用让 RPC 序列化（锁外）与编排阶段上报并发读写
	stages := make([]*pb.TaskStage, 0, total)
	for _, st := range task.GetStages() {
		stages = append(stages, proto.Clone(st).(*pb.TaskStage))
	}
	return &pb.TaskProgress{
		TaskId:         task.GetTaskId(),
		Status:         task.GetStatus(),
		OverallPercent: pct,
		Stages:         stages,
	}
}

// WatchTaskProgress watches the progress of a task (server streaming).
// 诚实声明: 推送式进度流未实现（ADR-131）；轮询 GetTaskProgress 是当前可用口径。
func (s *TaskServiceImpl) WatchTaskProgress(req *pb.WatchTaskProgressRequest, stream pb.TaskService_WatchTaskProgressServer) error {
	return status.Error(codes.Unimplemented, "WatchTaskProgress not implemented (poll GetTaskProgress)")
}

// fingerprintStage — 阶段上报请求体指纹（03 §2 同键异体判定）。
func fingerprintStage(req interface {
	GetTaskId() string
	GetStageId() string
}) string {
	return req.GetTaskId() + "|" + req.GetStageId()
}

// ReportStageComplete reports a stage as complete (idempotent).
// 依据: proto L880 "幂等"；proto L1132 output_refs=阶段产出引用；
// 幂等三态（03 §2）: 同键同体成功回放 / 同键异体 ALREADY_EXISTS / 未知任务 NOT_FOUND
func (s *TaskServiceImpl) ReportStageComplete(ctx context.Context, req *pb.ReportStageCompleteRequest) (*emptypb.Empty, error) {
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}
	requestID := req.GetMetadata().GetRequestId()
	fp := fingerprintStage(req) + "|" + sortedKeys(req.GetOutputRefs())

	s.mu.Lock()
	defer s.mu.Unlock()

	if prev, ok := s.stgIdm[requestID]; ok {
		if prev == fp {
			return &emptypb.Empty{}, nil // 同键同体幂等回放
		}
		return nil, status.Errorf(codes.AlreadyExists,
			"request_id %s already used with a different stage report (03 §2)", requestID)
	}

	task, ok := s.tasks[req.GetTaskId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.GetTaskId())
	}
	st := findOrInsertStageLocked(task, req.GetStageId())
	now := timestamppb.Now()
	st.Status = pb.StageStatus_STAGE_STATUS_COMPLETED
	if st.StartedAt == nil {
		st.StartedAt = now
	}
	st.CompletedAt = now
	for k, v := range req.GetOutputRefs() {
		st.Metadata[k] = v
	}
	task.UpdatedAt = now
	s.stgIdm[requestID] = fp
	s.hub.notify(req.GetTaskId()) // ADR-189
	return &emptypb.Empty{}, nil
}

// ReportStageFailed reports a stage as failed (idempotent).
// 依据: proto L881 "幂等"；proto L1138 error_message
func (s *TaskServiceImpl) ReportStageFailed(ctx context.Context, req *pb.ReportStageFailedRequest) (*emptypb.Empty, error) {
	if req.GetMetadata() == nil || req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}
	requestID := req.GetMetadata().GetRequestId()
	fp := fingerprintStage(req) + "|" + req.GetErrorMessage()

	s.mu.Lock()
	defer s.mu.Unlock()

	if prev, ok := s.stgIdm[requestID]; ok {
		if prev == fp {
			return &emptypb.Empty{}, nil
		}
		return nil, status.Errorf(codes.AlreadyExists,
			"request_id %s already used with a different stage report (03 §2)", requestID)
	}

	task, ok := s.tasks[req.GetTaskId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.GetTaskId())
	}
	st := findOrInsertStageLocked(task, req.GetStageId())
	now := timestamppb.Now()
	st.Status = pb.StageStatus_STAGE_STATUS_FAILED
	if st.StartedAt == nil {
		st.StartedAt = now
	}
	st.CompletedAt = now
	st.ErrorMessage = req.GetErrorMessage()
	task.UpdatedAt = now
	s.stgIdm[requestID] = fp
	s.hub.notify(req.GetTaskId()) // ADR-189
	return &emptypb.Empty{}, nil
}

// GetTaskContext retrieves the context of a task.
// 依据: proto TaskContext L1170（编排产出：项目配置/CPG路径/发现ID引用）
func (s *TaskServiceImpl) GetTaskContext(ctx context.Context, req *pb.GetTaskContextRequest) (*pb.TaskContext, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tc, ok := s.contexts[req.GetTaskId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound,
			"task context for %s not available (task not completed or unknown)", req.GetTaskId())
	}
	return tc, nil
}

// ---- reconciler.TaskStore 适配（04 §1 对账接线，ADR-131）----

// GetRunningTasks returns all RUNNING tasks.
func (s *TaskServiceImpl) GetRunningTasks() []*pb.ScanTask {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*pb.ScanTask, 0)
	for _, t := range s.tasks {
		if t.GetStatus() == pb.TaskStatus_TASK_STATUS_RUNNING {
			out = append(out, cloneLocked(t))
		}
	}
	return out
}

// UpdateTaskStatus validates and applies a status change (reconciler: RUNNING→TIMEOUT).
// 依据: 04 §1 T9 RUNNING→TIMEOUT (ReconcileTimeout)；转换校验=statemachine
func (s *TaskServiceImpl) UpdateTaskStatus(taskID string, newStatus pb.TaskStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	return s.transitionLocked(task, newStatus, "reconcile")
}

// ---- 内部工具 ----

func findOrInsertStageLocked(task *pb.ScanTask, stageID string) *pb.TaskStage {
	for _, st := range task.GetStages() {
		if st.GetStageId() == stageID {
			// ADR-212: 旧代码路径注册的阶段可能无 Metadata，防御式补齐
			if st.Metadata == nil {
				st.Metadata = map[string]string{}
			}
			return st
		}
	}
	st := &pb.TaskStage{
		StageId:  stageID,
		Status:   pb.StageStatus_STAGE_STATUS_PENDING,
		Metadata: map[string]string{},
	}
	task.Stages = append(task.Stages, st)
	return st
}

func sortedKeys(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ";")
}


// publishHighSeverityFindings — R64/D5finding.created 此前
// 只有消费端零生产者（高危发现站内通知永远不触发）。任务成功收尾时拉取本任务
// findings，HIGH/CRITICAL 者补发 finding.created（收件人=任务创建者）。失败非致命
// （通知缺失不阻任务终态），翻页拉全。
func (s *TaskServiceImpl) publishHighSeverityFindings(taskID, createdBy string) {
	if s.events == nil || createdBy == "" {
		return // 未启用事件档/无收件人（系统任务）——消费端同口径跳过
	}
	conn, err := grpc.Dial(s.resultAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("[task %s] finding.created: dial result: %v (non-fatal)", taskID, err)
		return
	}
	defer conn.Close()
	client := pb.NewResultServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cursor := ""
	for {
		resp, err := client.ListFindings(ctx, &pb.ListFindingsRequest{
			TaskId: taskID, Pagination: &pb.PaginationRequest{PageSize: 100, Cursor: cursor}})
		if err != nil {
			log.Printf("[task %s] finding.created: list: %v (non-fatal)", taskID, err)
			return
		}
		s.events.PublishFindingsCreatedAsync(taskID, createdBy, resp.GetFindings())
		if !resp.GetPagination().GetHasNext() {
			return
		}
		cursor = resp.GetPagination().GetNextCursor()
	}
}
