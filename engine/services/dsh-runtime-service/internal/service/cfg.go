// Package service — 全局配置访问助手（ADR-137）。
// 值承载于 configs/codeaudit.yaml（dsh_runtime 段），env 可覆盖，代码不留业务缺省。
package service

import (
	"fmt"
	"time"

	codeauditcfg "github.com/codeaudit/go-config"
)

// cfgDurationSec — 取 dsh_runtime.timeouts_s.<key>（秒→Duration）。
func cfgDurationSec(key string, envs ...string) time.Duration {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		panic(fmt.Sprintf("dsh-runtime config: %v (ADR-137)", err))
	}
	v, err := cfg.Int("dsh_runtime.timeouts_s."+key, envs...)
	if err != nil {
		panic(fmt.Sprintf("dsh-runtime config: %v (ADR-137)", err))
	}
	return time.Duration(v) * time.Second
}

// cfgAddr — 取 addresses.<key>（env 覆盖）。
func cfgAddr(key string, envs ...string) string {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		panic(fmt.Sprintf("dsh-runtime config: %v (ADR-137)", err))
	}
	v, err := cfg.Str("addresses."+key, envs...)
	if err != nil {
		panic(fmt.Sprintf("dsh-runtime config: %v (ADR-137)", err))
	}
	return v
}
