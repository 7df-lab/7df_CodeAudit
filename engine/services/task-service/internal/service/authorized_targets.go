// Package service — 授权目标【资源】字段校验与快照（ADR-248 切片4后半）。
//
// 依据: 主仓库 docs/designs/platform-slice4-network-tiers.md §3——每个授权目标
// = host+port+真实 IP/CIDR 三元组（非裸网段：上游模型按主机名授权+真实 IP
// 钉扎，裸 CIDR 无处安放）；拒载给字段路径（D1）。校验发生在创建时（强契约
// 不静默修剪）；快照进 ScanTask.authorized_targets（重试/项目变更不漂移）。
package service

import (
	"fmt"
	"net"
	"regexp"

	pb "github.com/codeaudit/proto-gen"
)

// hostLabelRe — RFC1123 主机名标签（授权目标按主机名访问=policy DNS 模型；
// IP 字面量形态由 allowed_ips 承载，host 面拒收防混淆）。
var hostLabelRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)

// validateAuthorizedTargets — 三元组校验（拒载给字段路径）。
func validateAuthorizedTargets(targets []*pb.AuthorizedTarget) error {
	for i, t := range targets {
		path := fmt.Sprintf("authorized_targets[%d]", i)
		if t == nil {
			return fmt.Errorf("%s: element must not be null", path)
		}
		// IP 字面量是 RFC1123 数字标签特例（正则天然放行）——显式拒：授权目标
		// 按主机名访问（policy DNS 模型），IP 钉扎属 allowed_ips，host 面拒收防混淆。
		if net.ParseIP(t.GetHost()) != nil {
			return fmt.Errorf("%s.host: %q 是 IP 字面量（授权目标按主机名访问，IP 钉扎放 allowed_ips）", path, t.GetHost())
		}
		if !hostLabelRe.MatchString(t.GetHost()) {
			return fmt.Errorf("%s.host: %q 不合法（RFC1123 主机名）", path, t.GetHost())
		}
		if t.GetPort() < 1 || t.GetPort() > 65535 {
			return fmt.Errorf("%s.port: %d 越界（1-65535）", path, t.GetPort())
		}
		if len(t.GetAllowedIps()) == 0 {
			return fmt.Errorf("%s.allowed_ips: 至少一个真实 IP/CIDR（透明中继钉扎面，空=不可达）", path)
		}
		for j, ip := range t.GetAllowedIps() {
			if net.ParseIP(ip) == nil {
				if _, _, err := net.ParseCIDR(ip); err != nil {
					return fmt.Errorf("%s.allowed_ips[%d]: %q 非 IP/CIDR", path, j, ip)
				}
			}
		}
	}
	return nil
}
