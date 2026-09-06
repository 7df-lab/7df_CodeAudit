package adapters

import (
	"testing"
)

func TestJSONAdapterEmptyRaw(t *testing.T) {
	p, _ := GetParser("json")
	_, err := p.Parse("t", "p", []byte(""))
	if err == nil {
		t.Fatalf("expected error on empty raw")
	}
}

func TestJSONAdapterInvalidJSON(t *testing.T) {
	p, _ := GetParser("json")
	_, err := p.Parse("t", "p", []byte("{"))
	if err == nil {
		t.Fatalf("expected parse error on invalid json")
	}
}

func TestJSONAdapterMissingSourceTool(t *testing.T) {
	raw := []byte(`{"tool":"","findings":[{"rule_id":"r","title":"t","severity":"high","confidence":"high","location":{"path":"app.py","start_line":1}}]}`)
	p, _ := GetParser("json")
	_, err := p.Parse("t", "p", raw)
	if err == nil {
		t.Fatalf("expected validation error for empty tool")
	}
}

func TestESLintAdapterEmptyRaw(t *testing.T) {
	p, _ := GetParser("eslint")
	_, err := p.Parse("t", "p", []byte(""))
	if err == nil {
		t.Fatalf("expected error on empty eslint output")
	}
}

func TestSpotBugsAdapterInvalidXML(t *testing.T) {
	p, _ := GetParser("spotbugs")
	_, err := p.Parse("t", "p", []byte("<BugCollection"))
	if err == nil {
		t.Fatalf("expected parse error on invalid xml")
	}
}
