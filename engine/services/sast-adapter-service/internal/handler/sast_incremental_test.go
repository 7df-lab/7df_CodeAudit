package handler

// ADR-225 增量文件清单模式锁定测试：argv 模板展开（{files}/{rules}）/ 占位缺失
// 诚实报错 / {project} 不得混入文件模式。依据=伞仓验收文档 F6。

import (
	"strings"
	"testing"
)

func TestBuildFilesArgv_Expansion(t *testing.T) {
	argv, err := buildFilesArgv(
		[]string{"bandit", "-f", "json", "-q", "{files}"},
		"/rules", []string{"/p/a.py", "/p/b.py"})
	if err != nil {
		t.Fatalf("buildFilesArgv: %v", err)
	}
	got := strings.Join(argv, " ")
	want := "bandit -f json -q /p/a.py /p/b.py"
	if got != want {
		t.Fatalf("argv=%q want %q", got, want)
	}
}

func TestBuildFilesArgv_RulesPlaceholder(t *testing.T) {
	argv, err := buildFilesArgv(
		[]string{"opengrep", "--config", "{rules}/sql-taint.yaml", "{files}"},
		"/rr", []string{"/p/x.py"})
	if err != nil {
		t.Fatalf("buildFilesArgv: %v", err)
	}
	if strings.Join(argv, " ") != "opengrep --config /rr/sql-taint.yaml /p/x.py" {
		t.Fatalf("rules 占位未解析: %v", argv)
	}
}

func TestBuildFilesArgv_MissingFilesPlaceholder(t *testing.T) {
	if _, err := buildFilesArgv([]string{"tool", "{project}"}, "/r", []string{"a.py"}); err == nil {
		t.Fatalf("缺 {files} 占位符必须报配置错误（防静默扫全目录参数）")
	}
}
