// Package codeauditcfg — CodeAudit 全局配置加载器（ADR-137）。
//
// 规则（人类裁决 2026-08-29）：全项目非密钥可调值统一承载于 configs/codeaudit.yaml，
// 代码内不保留业务缺省值；优先级 = 环境变量（CODEAUDIT_*）> 配置文件；
// 缺键 = 错误（fail-fast），杜绝代码默认值与部署环境漂移。
// 密钥不入配置文件（ADR-115），配置只记录"读取哪个环境变量"。
//
// 配置文件定位：CODEAUDIT_CONFIG 指定精确路径；否则从 CWD 逐级向上查找
// configs/codeaudit.yaml（兼容服务目录内直跑与仓库根目录运行两种形态）。
package codeauditcfg

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// DefaultFile — 配置文件在仓库内的相对位置。
const DefaultFile = "configs/codeaudit.yaml"

type Config struct {
	root map[string]interface{}
	file string
}

var (
	once   sync.Once
	shared *Config
	sharedErr error
)

// Load 显式加载（CODEAUDIT_CONFIG 或向上查找），每次返回新实例。
func Load() (*Config, error) {
	path := os.Getenv("CODEAUDIT_CONFIG")
	if path == "" {
		found, err := walkUp()
		if err != nil {
			return nil, err
		}
		path = found
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read global config %s: %w", path, err)
	}
	var root map[string]interface{}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse global config %s: %w", path, err)
	}
	return &Config{root: root, file: path}, nil
}

// Default 进程级共享实例（sync.Once 缓存）。
func Default() (*Config, error) {
	once.Do(func() {
		shared, sharedErr = Load()
	})
	return shared, sharedErr
}

func walkUp() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, DefaultFile)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("global config not found: %s not reachable from CWD (set CODEAUDIT_CONFIG)", DefaultFile)
		}
		dir = parent
	}
}

func (c *Config) File() string { return c.file }

// lookup 按点路径导航（如 "task.orchestrator.step_timeouts_s.analyze"）。
func (c *Config) lookup(path string) (interface{}, bool) {
	cur := interface{}(c.root)
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func (c *Config) missing(path string) error {
	return fmt.Errorf("global config %s: missing key %q (ADR-137: 代码不留业务缺省，请补全配置文件)", c.file, path)
}

// Str 取字符串；envs 中第一个非空环境变量优先（部署覆盖口）。
func (c *Config) Str(path string, envs ...string) (string, error) {
	for _, e := range envs {
		if v := os.Getenv(e); v != "" {
			return v, nil
		}
	}
	raw, ok := c.lookup(path)
	if !ok {
		return "", c.missing(path)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("global config %s: key %q is not a string", c.file, path)
	}
	return s, nil
}

// Int 取整数（env 覆盖值需可解析）。
func (c *Config) Int(path string, envs ...string) (int, error) {
	for _, e := range envs {
		if v := os.Getenv(e); v != "" {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return 0, fmt.Errorf("env %s=%q not an int: %w", e, v, err)
			}
			return n, nil
		}
	}
	raw, ok := c.lookup(path)
	if !ok {
		return 0, c.missing(path)
	}
	switch n := raw.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	default:
		return 0, fmt.Errorf("global config %s: key %q is not an int", c.file, path)
	}
}

// Bool 取布尔（env 覆盖值需为 true/false）。
func (c *Config) Bool(path string, envs ...string) (bool, error) {
	for _, e := range envs {
		if v := os.Getenv(e); v != "" {
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return false, fmt.Errorf("env %s=%q not a bool: %w", e, v, err)
			}
			return b, nil
		}
	}
	raw, ok := c.lookup(path)
	if !ok {
		return false, c.missing(path)
	}
	b, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("global config %s: key %q is not a bool", c.file, path)
	}
	return b, nil
}

// Float 取浮点。
func (c *Config) Float(path string, envs ...string) (float64, error) {
	for _, e := range envs {
		if v := os.Getenv(e); v != "" {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return 0, fmt.Errorf("env %s=%q not a float: %w", e, v, err)
			}
			return f, nil
		}
	}
	raw, ok := c.lookup(path)
	if !ok {
		return 0, c.missing(path)
	}
	switch n := raw.(type) {
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case float64:
		return n, nil
	default:
		return 0, fmt.Errorf("global config %s: key %q is not a float", c.file, path)
	}
}

// StrSlice 取字符串列表（env 覆盖值按逗号切分）。
func (c *Config) StrSlice(path string, envs ...string) ([]string, error) {
	for _, e := range envs {
		if v := os.Getenv(e); v != "" {
			parts := strings.Split(v, ",")
			out := make([]string, 0, len(parts))
			for _, p := range parts {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
			return out, nil
		}
	}
	raw, ok := c.lookup(path)
	if !ok {
		return nil, c.missing(path)
	}
	list, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("global config %s: key %q is not a list", c.file, path)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("global config %s: key %q has non-string item", c.file, path)
		}
		out = append(out, s)
	}
	return out, nil
}

// Seconds 取"秒数"配置并转为 Duration 便捷值。
func (c *Config) Seconds(path string, envs ...string) (int, error) {
	return c.Int(path, envs...)
}
