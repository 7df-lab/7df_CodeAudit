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

// TaskServiceImpl implements the TaskService gRPC service.
type TaskServiceImpl struct {
	pb.UnimplementedTaskServiceServer
	events *TaskEventProducer // ADR-199: Kafka 事件发布器（nil=禁用档）
	mu     sync.RWMutex
	tasks  map[string]*pb.ScanTask // task_id -> ScanTask
	idem   map[string]*idemRecord  // request_id -> 幂等记录（03 §2 三态）
	stgIdm map[string]string       // request_id -> 阶段上报指纹（ReportStage 幂等）
	// projectPaths/configs 按任务隔离（ADR-131：修复服务级单例字段被并发任务覆盖的竞态）
	projectPaths map[string]string
	configs      map[string]map[string]string
	contexts     map[string]*pb.TaskContext    // task_id → 编排产出上下文
	logs         map[string][]*pb.TaskLogEntry // task_id → 执行日志环形缓存（ADR-167）
	logIdem      map[string]string             // request_id → log_id（AppendTaskLog 幂等，R4）
	logSeq       int64                         // 日志全局单调序（跨任务分配 log_id）
	sm           *statemachine.StateMachine
	orch         *orchestrator.Orchestrator
	hub          *taskWatchHub // ADR-189 任务变更通知（StreamTaskSnapshot 推流源）
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
	// step_timeouts_s 已整体撤销（ADR-191 补遗，人类指令"都撤掉"）：编排步骤无外层时限。
	reposDir := must(cfg.Str("task.repos_dir", "CODEAUDIT_TASK_REPOS_DIR"))               // ADR-163
	cloneTimeout := time.Duration(mustInt(cfg.Int("task.clone_timeout_s"))) * time.Second // ADR-163
	return &TaskServiceImpl{
		tasks:        make(map[string]*pb.ScanTask),
		idem:         make(map[string]*idemRecord),
		stgIdm:       make(map[string]string),
		projectPaths: make(map[string]string),
		configs:      make(map[string]map[string]string),
		contexts:     make(map[string]*pb.TaskContext),
		logs:         make(map[string][]*pb.TaskLogEntry),
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
	}
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
	return nil
}

// cloneLocked — 返回任务深拷贝（proto message 含 sync.Mutex，禁止值拷贝；
// RPC 返回活指针会被编排协程并发变更 → data race，ADR-131 回归修复）。
func cloneLocked(task *pb.ScanTask) *pb.ScanTask {
	return proto.Clone(task).(*pb.ScanTask)
}

// fingerprintCreate — CreateScanTask 请求体指纹（03 §2 同键异体判定）。
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
	return fmt.Sprintf("%s|%s|%s|%s", req.GetProjectId(), req.GetScanMode().String(),
		strings.Join(req.GetSastTools(), ","), cfg.String())
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
			return s.tasks[requestID], nil
		}
		// 同键异体：按 03 §2 三态规则返回 ALREADY_EXISTS，不重放旧响应
		return nil, status.Errorf(codes.AlreadyExists,
			"request_id %s already used with a different request body (03 §2)", requestID)
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
		Config:    req.GetConfig(), // ADR-203 补遗: 任务自带 config 快照（proto L1128 config=13，随 GetTask 外露）
	}
	// 项目路径按任务登记（ADR-131：不再写服务级单例字段）
	if p, ok := req.GetConfig()["project_path"]; ok {
		s.projectPaths[task.TaskId] = p
	}
	s.configs[task.TaskId] = req.GetConfig()
	s.tasks[task.TaskId] = task
	s.idem[requestID] = &idemRecord{fingerprint: fp}
	log.Printf("Created task %s", task.TaskId)
	s.events.PublishAsync("task.created", task) // ADR-199: 09 §2 task→Kafka 行
	return task, nil
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
	// ADR-200: storage 上传件通道（人类指令：原始压缩包经 gateway 直传 storage 不落盘，
	// task 从 storage 拉回解包扫描；解压失败抛压缩包错误）。config.upload_file_id 优先。
	if r.ProjectPath == "" {
		// ADR-209: 读 proto 快照 task.Config（CreateScanTask L209 已写入，GetTask 外露、
		// 重启/持久化随行）而非 s.configs 内存副本——单一事实源，避免双写漂移
		if uploadID := task.GetConfig()["upload_file_id"]; uploadID != "" {
			prepare, msg := s.storagePrepare(task, uploadID)
			if msg != "" {
				_ = s.transitionLocked(task, pb.TaskStatus_TASK_STATUS_FAILED, "start")
				task.ErrorMessage = msg
				s.mu.Unlock()
				return nil, status.Error(codes.FailedPrecondition, msg)
			}
			r.Prepare = prepare
		}
	}
	// ADR-203: 项目级上传件兜底——项目弹窗上传（人类 2026-09-05 裁决"保留入口并改造"）经
	// gateway 零落盘直传 storage，upload_file_id 存项目 config；任务未携带上传件时从
	// 项目配置解析并**快照回写任务 config**（审核意见①：项目持"当前"指针，任务持"当时"
	// 快照——创建/启动间项目重传或 DEAD 重试前重传均不漂移，报告可回答"扫的是哪份包"）。
	// 解析链（ADR-203 补遗收口）：任务 config.upload_file_id → 项目 config.upload_file_id → repo_url clone。
	if r.ProjectPath == "" && r.Prepare == nil && task.GetProjectId() != "" {
		if uploadID := s.fetchProjectConfigValue(task.GetProjectId(), "upload_file_id"); uploadID != "" {
			prepare, msg := s.storagePrepare(task, uploadID)
			if msg != "" {
				_ = s.transitionLocked(task, pb.TaskStatus_TASK_STATUS_FAILED, "start")
				task.ErrorMessage = msg
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
				task.Config["upload_file_id"] = uploadID
			}
			if s.configs[task.GetTaskId()] == nil {
				s.configs[task.GetTaskId()] = map[string]string{}
			}
			if s.configs[task.GetTaskId()]["upload_file_id"] == "" {
				s.configs[task.GetTaskId()]["upload_file_id"] = uploadID
			}
			log.Printf("[task %s] storage mode via project config upload: %s (snapshot into task config)", task.GetTaskId(), uploadID)
		}
	}
	// ADR-163: 仓库拉取模式——路径仍缺省且项目配置 repo_url 时，编排协程内前置 git clone
	// （不阻塞 StartTask RPC；clone 失败走编排既有 FAILED→QUEUED 重试→DEAD 链，错误含 git 输出）。
	// ADR-209: 守卫必须含 r.Prepare == nil——repo clone 是解析链第三档**兜底**（任务级/项目级
	// upload_file_id 已解析出 storage 拉包闭包时，此处不得覆盖；自 ADR-200 起缺此条件导致
	// 优先级倒置，任务级上传件被静默换成 git clone）。
	if r.ProjectPath == "" && r.Prepare == nil && task.GetProjectId() != "" {
		if url, branch, gerr := s.fetchProjectRepo(task.GetProjectId()); gerr == nil && url != "" {
			dest := filepath.Join(s.reposDir, task.GetTaskId())
			timeout := s.cloneTimeout
			r.Prepare = func(ctx context.Context) (string, error) {
				log.Printf("[task %s] repo mode: cloning %s (branch=%s)", task.GetTaskId(), url, branch)
				return cloneRepo(ctx, url, branch, dest, timeout)
			}
		}
	}
	if r.ProjectPath == "" && r.Prepare == nil {
		// 明确失败：不空跑（此前回退 tests/samples 会让陌生项目扫到无关代码）
		msg := "project_path 未配置：请上传代码压缩包或为项目配置 repo_url（ADR-148/ADR-163）"
		_ = s.transitionLocked(task, pb.TaskStatus_TASK_STATUS_FAILED, "start")
		task.ErrorMessage = msg
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, msg)
	}
	repoPath := os.Getenv("CODEAUDIT_PROJECT_REPO_PATH")
	if repoPath != "" {
		r.ProjectPath = repoPath // 环境变量显式覆盖（E2E/CI 口径保留）
	}
	orch := s.orch
	recorder := s.stageRecorder(task.GetTaskId())
	s.mu.Unlock()

	go s.runOrchestration(orch, r, recorder)

	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := proto.Clone(s.tasks[req.GetTaskId()]).(*pb.ScanTask) // 深拷贝，禁止复制含锁的 MessageState
	return cp, nil
}

// runOrchestration — 执行编排并处理终态与自动重试。
// 依据: 04 §1 RUNNING→FAILED→QUEUED 自动重试≤2→耗尽 DEAD（proto L174/L177）
func (s *TaskServiceImpl) runOrchestration(orch *orchestrator.Orchestrator, r orchestrator.RunRequest, recorder orchestrator.StageRecorder) {
	// ADR-181 修复：Recorder 此前创建了却从未挂到 RunRequest——阶段事件从未到达
	// 阶段看板（时间线全程静止，终态靠 finalize 盖章；人类反馈"没有中间态"的根因）。
	r.Recorder = recorder
	// ADR-149b: 每次尝试独立幂等命名空间——重试复用同一 request_id 会让适配器
	// 返回失败尝试的缓存结果（其发现已被补偿删除），产出空报告的"假成功"。
	baseID := r.RequestID
	attempt := 0
	for {
		r.RequestID = fmt.Sprintf("%s-a%d", baseID, attempt)
		attempt++
		summary, err := orch.Execute(context.Background(), r)
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
			s.mu.Unlock()
			return
		}

		log.Printf("[task %s] orchestration failed (retry_count=%d): %v", r.TaskID, cur.GetRetryCount(), err)
		if int(cur.GetRetryCount()) < maxAutoRetries {
			// FAILED→QUEUED 自动重试（proto L174）
			if terr := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_FAILED, "fail"); terr != nil {
				log.Printf("[task %s] fail transition: %v", r.TaskID, terr)
			}
			cur.ErrorMessage = err.Error()
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
		if terr := s.transitionLocked(cur, pb.TaskStatus_TASK_STATUS_FAILED, "fail"); terr != nil {
			log.Printf("[task %s] fail transition: %v", r.TaskID, terr)
		}
		cur.ErrorMessage = err.Error()
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
		stages = append(stages, &pb.TaskStage{StageId: id, Type: typ, Status: pb.StageStatus_STAGE_STATUS_PENDING})
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
			s.completeStage(taskID, id)
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
		if st.Status == pb.StageStatus_STAGE_STATUS_PENDING {
			st.Status = pb.StageStatus_STAGE_STATUS_RUNNING
			st.StartedAt = timestamppb.Now()
			s.hub.notify(taskID) // ADR-189
		}
	}
}

// completeStage — 实时完成阶段（已终态则幂等跳过；未启动过的阶段补 StartedAt）。
func (s *TaskServiceImpl) completeStage(taskID, stageID string) {
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
		tasks = append(tasks, t)
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
	sort.Slice(tasks, func(i, j int) bool {
		ti, tj := tasks[i].GetUpdatedAt().AsTime(), tasks[j].GetUpdatedAt().AsTime()
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
	return &pb.TaskProgress{
		TaskId:         task.GetTaskId(),
		Status:         task.GetStatus(),
		OverallPercent: pct,
		Stages:         task.GetStages(),
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

// GetTask retrieves a task (alias for GetScanTask).
func (s *TaskServiceImpl) GetTask(ctx context.Context, req *pb.GetScanTaskRequest) (*pb.ScanTask, error) {
	return s.GetScanTask(ctx, req)
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
