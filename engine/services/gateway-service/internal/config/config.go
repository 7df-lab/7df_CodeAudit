package config

// gateway 配置 — ADR-137：全部可调值承载于全局配置文件 configs/codeaudit.yaml，
// 代码内不留业务缺省（缺键 fail-fast）；env 保留部署覆盖能力（优先级 env > 配置文件）。

import (
	"fmt"
	"os"

	codeauditcfg "github.com/codeaudit/go-config"
)

// Config holds the gateway service configuration
type Config struct {
	// JWT signing secret - 依据: 03 §4 (JWT HS256 auth)；密钥只从环境变量注入（ADR-115）
	JWTSecret string

	// Backend service addresses - 依据: 01 §4 (7 services, ADR-175/197)；端口依据 ADR-113/117（值在全局配置 ports/addresses）
	ProjectServiceAddr string
	TaskServiceAddr    string
	ResultServiceAddr  string
	StorageAddr        string // 通知中心（TP12-T0）
	SastAdapterAddr    string // /v1/tools 能力探测
	DSHRuntimeAddr       string // DSHRuntimeService（ADR-168 /v1/tasks/{id}/ai-log）

	// 限流是否信任 X-Forwarded-For（仅部署于可信反向代理之后时开启）
	// 依据: 07 §7 按请求方限流；ADR-132 防客户端伪造
	TrustProxy bool

	// 07 §7 限流值与调用超时（值在全局配置 gateway 段，07 §7 溯源）
	RateLimitPerMin  int
	GRPCCallTimeoutS int
	ShutdownGraceS   int

	// 上传解包根目录（ADR-145，值在全局配置 gateway.uploads_dir）
	UploadsDir string

	// 仓库拉取流任务源根（ADR-195，值在全局配置 gateway.repos_dir，与 task.repos_dir
	// 同值——repo 流 clone 目的地 <repos_dir>/<task_id>，source-file 端点据此回查）
	ReposDir string

	// Server configuration - 依据: ADR-113 (port 8080，值在全局配置 ports.gateway)
	Port string

	jwtSecretEnv string
}

// Load loads configuration: 全局配置文件 + env 覆盖（ADR-137）。
func Load() (*Config, error) {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		return nil, err
	}
	secretEnv, err := cfg.Str("gateway.jwt_secret_env")
	if err != nil {
		return nil, err
	}
	port, err := cfg.Int("ports.gateway", "CODEAUDIT_GATEWAY_PORT")
	if err != nil {
		return nil, err
	}
	projectAddr, err := cfg.Str("addresses.project", "CODEAUDIT_PROJECT_SERVICE_ADDR")
	if err != nil {
		return nil, err
	}
	taskAddr, err := cfg.Str("addresses.task", "CODEAUDIT_TASK_SERVICE_ADDR")
	if err != nil {
		return nil, err
	}
	resultAddr, err := cfg.Str("addresses.result", "CODEAUDIT_RESULT_SERVICE_ADDR")
	if err != nil {
		return nil, err
	}
	storageAddr, err := cfg.Str("addresses.storage", "CODEAUDIT_STORAGE_SERVICE_ADDR")
	if err != nil {
		return nil, err
	}
	adapterAddr, err := cfg.Str("addresses.sast_adapter", "CODEAUDIT_SAST_ADAPTER_ADDR")
	if err != nil {
		return nil, err
	}
	dshRuntimeAddr, err := cfg.Str("addresses.dsh_runtime", "CODEAUDIT_DSH_RUNTIME_ADDR")
	if err != nil {
		return nil, err
	}
	trustProxy, err := cfg.Bool("gateway.trust_proxy", "CODEAUDIT_TRUST_PROXY")
	if err != nil {
		return nil, err
	}
	rateLimit, err := cfg.Int("gateway.rate_limit_per_min")
	if err != nil {
		return nil, err
	}
	callTimeout, err := cfg.Int("gateway.grpc_call_timeout_s")
	if err != nil {
		return nil, err
	}
	grace, err := cfg.Int("gateway.shutdown_grace_s")
	if err != nil {
		return nil, err
	}
	uploadsDir, err := cfg.Str("gateway.uploads_dir")
	if err != nil {
		return nil, err
	}
	reposDir, err := cfg.Str("gateway.repos_dir")
	if err != nil {
		return nil, err
	}

	return &Config{
		JWTSecret:          os.Getenv(secretEnv),
		jwtSecretEnv:       secretEnv,
		ProjectServiceAddr: projectAddr,
		TaskServiceAddr:    taskAddr,
		ResultServiceAddr:  resultAddr,
		StorageAddr:        storageAddr,
		SastAdapterAddr:    adapterAddr,
		DSHRuntimeAddr:       dshRuntimeAddr,
		TrustProxy:         trustProxy,
		RateLimitPerMin:    rateLimit,
		GRPCCallTimeoutS:   callTimeout,
		ShutdownGraceS:     grace,
		UploadsDir:         uploadsDir,
		ReposDir:           reposDir,
		Port:               fmt.Sprintf("%d", port),
	}, nil
}

// JWTSecretName — 密钥所在环境变量名（错误提示用；密钥值不入配置，ADR-115）。
func (c *Config) JWTSecretName() string { return c.jwtSecretEnv }
