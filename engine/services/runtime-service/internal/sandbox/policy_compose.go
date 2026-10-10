// Package sandbox — targeted 网络分型 spec.policy 组合器（ADR-248 切片4后半）。
//
// 依据: 主仓库 docs/designs/platform-slice4-network-tiers.md §3 + dsh-pentest-sse
// docs/internal-interfaces.md §I8——spec.policy = targeted.yaml 模板全文 +
// authorized_targets.endpoints ←任务【资源】字段，整档携带（整档替换语义）；
// wire-name 特例 filesystem_policy→filesystem；endpoints 只来自任务资源声明
// （B1 单向收紧的构造性保证：本函数是唯一写入点）；fail-closed（模板缺失/形状
// 断言失败=error，不静默回落 restricted）。
package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// policyTemplateShape — targeted 模板加载与形状断言（模板是部署树受版本控制
// 文件，engine 仓 configs/policy/targeted.yaml；跨仓与 dsh-pentest-sse 原件
// 一致性由条件门禁钉）。
type policyTemplate struct {
	doc      map[string]any // 整档（含 filesystem_policy/landlock/process/network_policies）
	digest   string         // 组合时内容锚（launch 事件留痕，C3 任务级对账）
	endpoint map[string]any // authorized_targets 规则（endpoints 注入点）
}

// loadPolicyTemplate — 读模板 YAML → 断言两规则在（codeaudit_inference 基础档
// + authorized_targets envelope）→ 摘出 authorized_targets 规则引用。
func loadPolicyTemplate(path string) (*policyTemplate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("targeted 模板不可读（%v）——targeted 档 fail-closed，不静默回落", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("targeted 模板解析失败: %w", err)
	}
	sum := sha256.Sum256(raw)
	nps, _ := doc["network_policies"].(map[string]any)
	base, okBase := nps["codeaudit_inference"]
	env, okEnv := nps["authorized_targets"]
	if !okBase || !okEnv {
		return nil, fmt.Errorf("targeted 模板形状断言失败：network_policies 须含 codeaudit_inference 与 authorized_targets（got base=%v env=%v）", okBase, okEnv)
	}
	rule, _ := env.(map[string]any)
	if rule == nil {
		return nil, fmt.Errorf("targeted 模板 authorized_targets 非规则对象")
	}
	_ = base // 基础档逐字保留：组合器不触碰模板其余内容（只写 rule["endpoints"]）
	return &policyTemplate{doc: doc, digest: hex.EncodeToString(sum[:]), endpoint: rule}, nil
}

// wireName — YAML 段名 → spec.policy JSON 面键名（§I8 wire-name 特例：
// filesystem_policy 必须写作 filesystem——网关 400 报文枚举实证；其余原样）。
var wireNames = map[string]string{"filesystem_policy": "filesystem"}

// ComposeTargetedPolicy — 组合 spec.policy（整档替换载荷）。
// targets 为空=endpoints 恒空（烘焙态 fail-closed：targeted 沙箱零目标可达）。
// 返回 (policy, digest, error)：digest=组合时模板内容 sha256（审计留痕）。
func ComposeTargetedPolicy(path string, targets []targetEndpoint) (map[string]any, string, error) {
	tmpl, err := loadPolicyTemplate(path)
	if err != nil {
		return nil, "", err
	}
	eps := make([]any, 0, len(targets)) // 空=恒空 slice（非 nil）：JSON 序列化为 []
	for _, t := range targets {
		eps = append(eps, map[string]any{
			"host":        t.Host,
			"port":        t.Port,
			"protocol":    "tcp", // §I8 激活配方恒值（透明中继）；不接受调用方指定
			"allowed_ips": t.AllowedIPs,
		})
	}
	tmpl.endpoint["endpoints"] = eps // 唯一写入点（B1 单向收紧构造性保证）

	out := make(map[string]any, len(tmpl.doc))
	for k, v := range tmpl.doc {
		if wn, ok := wireNames[k]; ok {
			out[wn] = v // wire-name 特例
		} else {
			out[k] = v
		}
	}
	return out, tmpl.digest, nil
}

// targetEndpoint — 组合器输入（AuthorizedTarget 的 sandbox 包内投影；pb 依赖
// 留在 service 层装配处，sandbox 包保持传输层零 proto 依赖的既有分层）。
type targetEndpoint struct {
	Host       string
	Port       uint32
	AllowedIPs []string
}

// TargetEndpoint — targetEndpoint 构造器（service 层从 pb.AuthorizedTarget 投影；
// sandbox 包保持传输层零 proto 依赖的既有分层）。
func TargetEndpoint(host string, port uint32, ips []string) targetEndpoint {
	return targetEndpoint{Host: host, Port: port, AllowedIPs: ips}
}
