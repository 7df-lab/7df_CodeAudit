<!-- 06 V1.1: 环境变量拼写修正 OPENHELL→OPENSHELL；存储层含Milvus(ADR-004保留——↪已被 ADR-197 推翻,Milvus/Neo4j 出栈)；服务部署口径见《01 总体架构设计》§4，沙箱资源配置见《07 非功能指标基线》§8.1 -->

# NVIDIA OpenShell 集成设计

## 文档信息

| 项目 | 内容 |
|------|------|
| **文档名称** | NVIDIA OpenShell 集成设计 |
| **文档版本** | V1.1 |
| **编制日期** | 2025-07-15 |
| **更新日期** | 2025-08-21 |

---

## 1. OpenShell 概述

### 1.1 什么是 OpenShell

[NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell) 是英伟达推出的 **AI代理安全运行时**，为自主AI代理提供安全、私密的执行环境。

**核心特性**：
- **安全沙箱**：基于 MicroVM 的隔离执行环境
- **工具权限控制**：细粒度的工具访问权限管理
- **资源限制**：CPU、内存、磁盘、时间的严格限制
- **网络隔离**：默认禁止网络访问，白名单控制
- **审计日志**：完整的工具调用和执行日志

### 1.2 OpenShell vs 容器编排

| 维度 | OpenShell | Kubernetes |
|------|-----------|------------|
| **定位** | AI代理安全运行时 | 容器编排平台 |
| **用途** | 运行AI Agent的沙箱环境 | 运行微服务的容器集群 |
| **隔离级别** | MicroVM级别（更强） | 容器级别 |
| **安全模型** | 工具权限、网络隔离 | RBAC、NetworkPolicy |
| **生命周期** | Agent会话级别 | 服务长期运行 |

**在CodeAudit中的角色**：
```
Kubernetes: 运行所有微服务（API、数据库、前端等）
    │
    └── OpenShell: 运行DSH Agent（AI代理安全沙箱）
```

---

## 2. 架构设计

### 2.1 整体架构

```
┌─────────────────────────────────────────────────────────────────┐
│                    CodeAudit 系统架构                             │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │                    用户访问层                              │   │
│  │     Web Dashboard │ IDE Plugin │ CLI │ CI/CD Webhook     │   │
│  └─────────────────────────────────────────────────────────┘   │
│                              │                                  │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │                    Kubernetes 集群                        │   │
│  │                                                          │   │
│  │  ┌─────────────────────────────────────────────────┐    │   │
│  │  │  核心微服务                                        │    │   │
│  │  │  Gateway │ User │ Project │ Task │ Result │ Report│    │   │
│  │  └─────────────────────────────────────────────────┘    │   │
│  │                              │                           │   │
│  │  ┌─────────────────────────────────────────────────┐    │   │
│  │  │  AI服务层                                         │    │   │
│  │  │  Code Analysis │ Knowledge Graph │ RAG Retrieval │    │   │
│  │  │  SAST Adapter │ SAST Fusion │ SAST Comparison   │    │   │
│  │  └─────────────────────────────────────────────────┘    │   │
│  │                              │                           │   │
│  │  ┌─────────────────────────────────────────────────┐    │   │
│  │  │  DSH Agent Service (编排层)                       │    │   │
│  │  │  • 管理DSH会话生命周期                            │    │   │
│  │  │  • 协调多Agent执行                                │    │   │
│  │  │  • 聚合Agent结果                                  │    │   │
│  │  └─────────────────────────────────────────────────┘    │   │
│  │              │                                           │   │
│  │              ▼                                           │   │
│  │  ┌─────────────────────────────────────────────────┐    │   │
│  │  │  NVIDIA OpenShell 集群                            │    │   │
│  │  │                                                  │    │   │
│  │  │  ┌──────────┐ ┌──────────┐ ┌──────────┐        │    │   │
│  │  │  │ Sandbox  │ │ Sandbox  │ │ Sandbox  │ ...    │    │   │
│  │  │  │ Agent 1  │ │ Agent 2  │ │ Agent 3  │        │    │   │
│  │  │  │          │ │          │ │          │        │    │   │
│  │  │  │ DSH+MiMo │ │ DSH+MiMo │ │ DSH+MiMo │        │    │   │
│  │  │  └──────────┘ └──────────┘ └──────────┘        │    │   │
│  │  │                                                  │    │   │
│  │  │  安全特性：                                       │    │   │
│  │  │  • MicroVM隔离                                   │    │   │
│  │  │  • 工具权限白名单                                 │    │   │
│  │  │  • 网络访问控制                                   │    │   │
│  │  │  • 资源配额限制                                   │    │   │
│  │  │  • 执行审计日志                                   │    │   │
│  │  └─────────────────────────────────────────────────┘    │   │
│  │                                                          │   │
│  │  ┌─────────────────────────────────────────────────┐    │   │
│  │  │  数据存储层                                       │    │   │
│  │  │  PostgreSQL │ Redis │ MinIO（ADR-197 后无 Neo4j/Milvus）│   │
│  │  └─────────────────────────────────────────────────┘    │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### 2.2 OpenShell与DSH集成

```
┌─────────────────────────────────────────────────────────────────┐
│                    OpenShell + DSH 集成架构                       │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  DSH Agent Service                                              │
│       │                                                         │
│       │ 1. 创建OpenShell沙箱                                    │
│       ▼                                                         │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │  OpenShell Sandbox                                       │   │
│  │                                                          │   │
│  │  ┌─────────────────────────────────────────────────┐    │   │
│  │  │  DSH Runtime                                     │    │   │
│  │  │                                                  │    │   │
│  │  │  ┌──────────┐  ┌──────────┐  ┌──────────┐      │    │   │
│  │  │  │ MiMo LLM │  │  Tools   │  │  Memory  │      │    │   │
│  │  │  │ (API)    │  │ (受限)   │  │ (会话)   │      │    │   │
│  │  │  └──────────┘  └──────────┘  └──────────┘      │    │   │
│  │  │                                                  │    │   │
│  │  │  工具列表（由OpenShell控制）：                    │    │   │
│  │  │  ✓ read: 读取代码文件                            │    │   │
│  │  │  ✓ bash: 执行受限命令                            │    │   │
│  │  │  ✓ grep: 搜索代码                                │    │   │
│  │  │  ✓ glob: 查找文件                                │    │   │
│  │  │  ✗ write: 禁止（或仅允许特定目录）               │    │   │
│  │  │  ✗ network: 禁止（或白名单）                     │    │   │
│  │  │  ✗ sudo: 禁止                                   │    │   │
│  │  └─────────────────────────────────────────────────┘    │   │
│  │                                                          │   │
│  │  挂载点：                                                │   │
│  │  • /workspace (只读): 待审计代码                         │   │
│  │  • /tmp (读写): 临时文件                                │   │
│  │  • /output (只写): 分析结果                             │   │
│  │                                                          │   │
│  └─────────────────────────────────────────────────────────┘   │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

---

## 3. OpenShell 配置设计 <!-- consumers: TP07 -->

### 3.1 沙箱配置模板

```yaml
# openshell-config.yaml
# OpenShell 沙箱配置模板

apiVersion: openshell.nvidia.com/v1
kind: SandboxProfile
metadata:
  name: codeaudit-code-analysis
  namespace: codeaudit
spec:
  # 基础镜像
  runtime:
    image: "nvcr.io/nvidia/openshell:latest"
    microvm: true  # 使用MicroVM隔离
    kernel: "minimal"

  # 资源限制
  resources:
    cpu: "2"
    memory: "4Gi"
    disk: "10Gi"
    gpu: "0"
    timeout: "30m"

  # 文件系统
  filesystem:
    mounts:
      - name: workspace
        hostPath: "/data/projects/{project_id}"
        mountPath: "/workspace"
        readOnly: true

      - name: temp
        emptyDir: {}
        mountPath: "/tmp"
        readOnly: false

      - name: output
        emptyDir: {}
        mountPath: "/output"
        readOnly: false

    restrictions:
      maxFileSize: "10MB"
      maxTotalSize: "1GB"
      blockedPaths: ["/etc", "/root", "/home", "/var", "/proc", "/sys"]

  # 网络配置
  network:
    enabled: false  # 默认禁用网络
    # 白名单（如需要）
    # allowedHosts:
    #   - "api.openai.com"
    #   - "internal-kb.codeaudit.local"

  # 工具权限
  tools:
    # bash工具
    - name: bash
      enabled: true
      config:
        timeout: "30s"
        allowedCommands:
          - "grep"
          - "find"
          - "wc"
          - "head"
          - "tail"
          - "cat"
          - "python3"
          - "node"
        blockedCommands:
          - "rm"
          - "mv"
          - "chmod"
          - "chown"
          - "sudo"
          - "curl"
          - "wget"
          - "pip"
          - "npm"
        maxOutputSize: "1MB"

    # 文件读取
    - name: read
      enabled: true
      config:
        maxFileSize: "10MB"
        allowedExtensions:
          - ".py"
          - ".js"
          - ".ts"
          - ".java"
          - ".go"
          - ".rs"
          - ".c"
          - ".cpp"
          - ".h"
          - ".hpp"
          - ".rb"
          - ".php"

    # 文件写入（受限）
    - name: write
      enabled: true
      config:
        allowedPaths:
          - "/output/*"
          - "/tmp/*"
        blockedPaths:
          - "/workspace/*"
        maxFileSize: "5MB"
        allowedExtensions:
          - ".json"
          - ".txt"
          - ".md"

    # 搜索
    - name: grep
      enabled: true
      config:
        timeout: "10s"
        maxResults: 1000

    # 文件查找
    - name: glob
      enabled: true
      config:
        maxResults: 500

    # 子代理（受限）
    - name: subagent
      enabled: true
      config:
        maxDepth: 2
        maxConcurrent: 3

    # Web搜索（生产禁用）
    - name: web_search
      enabled: false

  # 环境变量
  env:
    - name: "CODEAUDIT_MODE"
      value: "sandbox"
    - name: "LOG_LEVEL"
      value: "INFO"
    - name: "OUTPUT_DIR"
      value: "/output"

  # 审计日志
  audit:
    enabled: true
    logLevel: "detailed"
    captureInputs: true
    captureOutputs: true
    retention: "30d"
```

### 3.2 不同Agent的配置差异

```yaml
# Agent配置差异矩阵
agent_profiles:

  code_analyst:
    extends: "codeaudit-code-analysis"
    resources:
      cpu: "2"
      memory: "4Gi"
      timeout: "30m"
    tools:
      - name: bash
        config:
          allowedCommands: ["grep", "find", "wc", "head", "tail", "cat"]
      - name: read
      - name: grep
      - name: glob

  vuln_detector:
    extends: "codeaudit-code-analysis"
    resources:
      cpu: "4"
      memory: "8Gi"
      timeout: "20m"
    tools:
      - name: bash
        config:
          allowedCommands: ["grep", "find", "wc", "head", "tail", "cat", "python3"]
      - name: read
      - name: grep
      - name: glob
      - name: subagent

  severity_assessor:
    extends: "codeaudit-code-analysis"
    resources:
      cpu: "1"
      memory: "2Gi"
      timeout: "10m"
    tools:
      - name: read
      - name: subagent

  fix_advisor:
    extends: "codeaudit-code-analysis"
    resources:
      cpu: "2"
      memory: "4Gi"
      timeout: "15m"
    filesystem:
      mounts:
        - name: output
          mountPath: "/output"
          readOnly: false
    tools:
      - name: read
      - name: write
        config:
          allowedPaths: ["/output/*", "/tmp/*"]
      - name: edit
      - name: bash
        config:
          allowedCommands: ["grep", "find", "cat"]

  quality_validator:
    extends: "codeaudit-code-analysis"
    resources:
      cpu: "1"
      memory: "2Gi"
      timeout: "10m"
    tools:
      - name: read
      - name: subagent
```

---

## 4. 集成实现 <!-- consumers: TP07 -->

### 4.1 DSH Agent Service 与 OpenShell 集成

```python
# dsh_runtime_service/openshell_manager.py

from openshell import OpenShellClient, SandboxConfig
from typing import Dict, List, Optional
import asyncio

class OpenShellManager:
    """管理OpenShell沙箱生命周期"""

    def __init__(self, config: dict):
        self.client = OpenShellClient(
            endpoint=config["openshell_endpoint"],
            api_key=config["api_key"]
        )
        self.profiles = config["profiles"]

    async def create_sandbox(
        self,
        agent_name: str,
        project_path: str,
        task_id: str
    ) -> str:
        """创建OpenShell沙箱"""

        profile = self.profiles[agent_name]

        sandbox_config = SandboxConfig(
            profile=profile["name"],
            mounts=[
                {
                    "name": "workspace",
                    "hostPath": project_path,
                    "mountPath": "/workspace",
                    "readOnly": True
                }
            ],
            env={
                "TASK_ID": task_id,
                "AGENT_NAME": agent_name
            },
            resources=profile["resources"]
        )

        sandbox = await self.client.create_sandbox(sandbox_config)
        return sandbox.id

    async def execute_in_sandbox(
        self,
        sandbox_id: str,
        command: str,
        timeout: int = 30
    ) -> Dict:
        """在沙箱中执行命令"""

        result = await self.client.execute(
            sandbox_id=sandbox_id,
            command=command,
            timeout=timeout
        )

        return {
            "stdout": result.stdout,
            "stderr": result.stderr,
            "exit_code": result.exit_code,
            "duration_ms": result.duration_ms
        }

    async def destroy_sandbox(self, sandbox_id: str):
        """销毁沙箱"""
        await self.client.destroy_sandbox(sandbox_id)

    async def get_sandbox_logs(self, sandbox_id: str) -> List[Dict]:
        """获取沙箱审计日志"""
        logs = await self.client.get_audit_logs(sandbox_id)
        return [
            {
                "timestamp": log.timestamp,
                "tool": log.tool_name,
                "action": log.action,
                "input": log.input_data,
                "output": log.output_data,
                "duration_ms": log.duration_ms
            }
            for log in logs
        ]
```

### 4.2 DSH Agent 在 OpenShell 中运行

```python
# dsh_runtime_service/agent_runner.py

from dsh import DSHRuntime, DSHConfig
from openshell_manager import OpenShellManager

class AgentRunner:
    """在OpenShell中运行DSH Agent"""

    def __init__(self, openshell_manager: OpenShellManager):
        self.openshell = openshell_manager

    async def run_agent(
        self,
        agent_name: str,
        task_id: str,
        project_path: str,
        input_data: dict
    ) -> dict:
        """运行单个Agent"""

        sandbox_id = None
        try:
            # 1. 创建OpenShell沙箱
            sandbox_id = await self.openshell.create_sandbox(
                agent_name=agent_name,
                project_path=project_path,
                task_id=task_id
            )

            # 2. 在沙箱中初始化DSH
            dsh_config = DSHConfig(
                sandbox_id=sandbox_id,
                model="mimo-v2.5-pro",
                tools=self._get_agent_tools(agent_name)
            )

            agent = DSHRuntime(config=dsh_config)

            # 3. 执行Agent任务
            result = await agent.run(
                prompt=self._build_prompt(agent_name, input_data),
                max_iterations=self._get_max_iterations(agent_name)
            )

            # 4. 获取审计日志
            audit_logs = await self.openshell.get_sandbox_logs(sandbox_id)

            return {
                "agent": agent_name,
                "task_id": task_id,
                "result": result,
                "audit_logs": audit_logs,
                "status": "completed"
            }

        except Exception as e:
            return {
                "agent": agent_name,
                "task_id": task_id,
                "error": str(e),
                "status": "failed"
            }

        finally:
            # 5. 销毁沙箱
            if sandbox_id:
                await self.openshell.destroy_sandbox(sandbox_id)

    def _get_agent_tools(self, agent_name: str) -> List[str]:
        """获取Agent可用工具列表"""
        tool_map = {
            "code_analyst": ["read", "bash", "grep", "glob"],
            "vuln_detector": ["read", "bash", "grep", "glob", "subagent"],
            "severity_assessor": ["read", "subagent"],
            "fix_advisor": ["read", "write", "edit", "bash"],
            "quality_validator": ["read", "subagent"]
        }
        return tool_map.get(agent_name, [])

    def _get_max_iterations(self, agent_name: str) -> int:
        """获取Agent最大迭代次数"""
        iteration_map = {
            "code_analyst": 50,
            "vuln_detector": 30,
            "severity_assessor": 10,
            "fix_advisor": 15,
            "quality_validator": 10
        }
        return iteration_map.get(agent_name, 20)

    def _build_prompt(self, agent_name: str, input_data: dict) -> str:
        """构建Agent提示词"""
        # 根据Agent类型构建不同的提示词
        pass
```

### 4.3 安全约束实现

```python
# dsh_runtime_service/security.py

class OpenShellSecurityPolicy:
    """OpenShell安全策略管理"""

    # 默认安全策略
    DEFAULT_POLICY = {
        # 文件系统访问
        "filesystem": {
            "read": {
                "allowed_paths": ["/workspace/*", "/tmp/*"],
                "blocked_paths": ["/etc/*", "/root/*", "/home/*", "/var/*"],
                "max_file_size": "10MB"
            },
            "write": {
                "allowed_paths": ["/output/*", "/tmp/*"],
                "blocked_paths": ["/workspace/*"],
                "max_file_size": "5MB",
                "allowed_extensions": [".json", ".txt", ".md", ".csv"]
            }
        },

        # 网络访问
        "network": {
            "enabled": False,
            "allowed_hosts": [],
            "blocked_ports": ["22", "80", "443", "3306", "5432", "6379"]
        },

        # 进程执行
        "process": {
            "allowed_commands": ["grep", "find", "wc", "head", "tail", "cat"],
            "blocked_commands": ["rm", "mv", "chmod", "chown", "sudo", "curl", "wget"],
            "max_processes": 5,
            "timeout": "30s"
        },

        # 资源限制
        "resources": {
            "max_cpu": "4",
            "max_memory": "8Gi",
            "max_disk": "10Gi",
            "max_open_files": 100,
            "max_execution_time": "30m"
        }
    }

    # 高权限策略（仅用于特定场景）
    ELEVATED_POLICY = {
        **DEFAULT_POLICY,
        "network": {
            "enabled": True,
            "allowed_hosts": ["api.codeaudit.local", "knowledge-base.codeaudit.local"]
        },
        "process": {
            **DEFAULT_POLICY["process"],
            "allowed_commands": [
                *DEFAULT_POLICY["process"]["allowed_commands"],
                "python3", "pip"
            ]
        }
    }

    @classmethod
    def get_policy(cls, agent_name: str, trust_level: str = "default") -> dict:
        """获取Agent安全策略"""

        if trust_level == "elevated":
            return cls.ELEVATED_POLICY

        # 根据Agent类型微调策略
        policy = cls.DEFAULT_POLICY.copy()

        if agent_name == "fix_advisor":
            # 修复建议Agent需要写入权限
            policy["filesystem"]["write"]["allowed_paths"].extend([
                "/tmp/patches/*"
            ])

        return policy
```

---

## 5. 部署配置

> ↪2026-09-05 插注（ADR-206）：本节 k8s manifests 为**目标形态**示例；现行部署事实源在
> **伞仓 deploy/prod/**（docker compose 全栈，LXC 107，见 12 现状插注）。下文 dsh-runtime 容器
> 注入 MIMO_API_KEY/ENDPOINT 的写法属直调时代残留——现行 LLM 凭据由 OpenShell 网关注入沙箱，
> **永不进入引擎侧容器**（ADR-173/175）。

<!-- §5.1~5.x 逐服务 k8s manifests 已删（ADR-207 瘦身，~190 行；内容见 git 历史）。
     目标形态要点保留如下，现行部署事实源=伞仓 deploy/prod/（compose）与 deploy/sim（模拟栈）： -->

**目标形态要点（K8s，未落地）**：

- 组件：openshell-gateway（Deployment，gRPC 8080/health 8081）+ dsh-runtime（Deployment，
  configMap `dsh-runtime-config` 挂 /etc/dsh-runtime，资源 requests/limits 按 07 §8.1）；
- LLM 凭据：**不注入引擎侧任何容器**——凭据由 OpenShell 网关注入沙箱（ADR-173/175，
  原 MIMO_API_KEY/ENDPOINT secretKeyRef 写法已废）；
- 命名空间/服务发现/存储卷布局沿用本节原 manifests（git 历史），落地前须按 12 现状插注
  与伞仓 deploy/ 口径重新评审。

---

## 6. 监控与审计

### 6.1 OpenShell监控指标

```yaml
# Prometheus监控配置
openshell_metrics:
  # 沙箱指标
  - name: openshell_sandboxes_active
    type: gauge
    description: "当前活跃的沙箱数量"

  - name: openshell_sandboxes_created_total
    type: counter
    description: "创建的沙箱总数"

  - name: openshell_sandbox_duration_seconds
    type: histogram
    description: "沙箱运行时长"
    buckets: [60, 300, 600, 1800, 3600]

  # 工具调用指标
  - name: openshell_tool_calls_total
    type: counter
    labels: ["tool", "status"]
    description: "工具调用总数"

  - name: openshell_tool_call_duration_seconds
    type: histogram
    labels: ["tool"]
    description: "工具调用时长"

  # 安全指标
  - name: openshell_security_violations_total
    type: counter
    labels: ["violation_type"]
    description: "安全违规次数"

  - name: openshell_blocked_commands_total
    type: counter
    description: "被阻止的命令总数"

  # 资源指标
  - name: openshell_cpu_usage
    type: gauge
    labels: ["sandbox_id"]
    description: "沙箱CPU使用率"

  - name: openshell_memory_usage_bytes
    type: gauge
    labels: ["sandbox_id"]
    description: "沙箱内存使用量"
```

### 6.2 审计日志格式

```json
{
  "timestamp": "2025-07-15T10:30:00.123Z",
  "sandbox_id": "sandbox-abc123",
  "agent_name": "vuln_detector",
  "task_id": "task-xyz789",
  "event": "tool_call",
  "tool": "bash",
  "action": "execute",
  "input": {
    "command": "grep -r 'password' /workspace/src/"
  },
  "output": {
    "stdout": "src/config.py:password = 'secret123'",
    "stderr": "",
    "exit_code": 0
  },
  "duration_ms": 150,
  "security": {
    "policy": "codeaudit-restricted",
    "allowed": true,
    "reason": "command in whitelist"
  }
}
```

---

## 7. 安全最佳实践

### 7.1 安全原则

```
┌─────────────────────────────────────────────────────────────────┐
│                    OpenShell 安全原则                            │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  1. 最小权限原则                                                │
│     • 每个Agent只授予必要的工具权限                              │
│     • 默认拒绝，显式允许                                        │
│     • 定期审查和收紧权限                                        │
│                                                                 │
│  2. 深度防御                                                    │
│     • MicroVM级别隔离                                          │
│     • 网络层隔离                                                │
│     • 文件系统隔离                                              │
│     • 进程隔离                                                  │
│                                                                 │
│  3. 完整审计                                                    │
│     • 记录所有工具调用                                          │
│     • 记录所有文件访问                                          │
│     • 保留审计日志30天                                          │
│                                                                 │
│  4. 快速响应                                                    │
│     • 异常行为自动告警                                          │
│     • 安全违规自动阻断                                          │
│     • 支持快速销毁沙箱                                          │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### 7.2 常见安全场景处理

| 场景 | 处理方式 |
|------|---------|
| Agent尝试执行`sudo` | 命令被阻止，记录安全违规 |
| Agent尝试访问`/etc/passwd` | 访问被拒绝，记录违规 |
| Agent尝试下载外部文件 | 网络访问被阻止（默认禁用） |
| Agent执行时间超过限制 | 沙箱自动销毁，返回部分结果 |
| Agent内存使用超过限制 | 沙箱被强制终止 |
| Agent产生大量输出 | 输出被截断，记录警告 |

---

## 附录

### A. OpenShell资源链接

- [GitHub仓库](https://github.com/NVIDIA/OpenShell)
- [官方文档](https://docs.nvidia.com/openshell/)
- [PyPI包](https://pypi.org/project/openshell/)

### B. 相关技术

- **MicroVM**: 轻量级虚拟机，提供VM级别的隔离
- **libkrun**: OpenShell使用的MicroVM实现
- **Kata Containers**: 另一个容器安全运行时选项

---

**文档结束**
