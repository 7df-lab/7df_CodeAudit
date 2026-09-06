package fusion_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/sast-adapter-service/internal/fusion"
)

// GoldenFindingInput is a JSON-friendly representation for test input.
type GoldenFindingInput struct {
	FindingID    string  `json:"finding_id"`
	TaskID       string  `json:"task_id"`
	SourceTool   string  `json:"source_tool"`
	FilePath     string  `json:"file_path"`
	StartLine    int32   `json:"start_line"`
	EndLine      int32   `json:"end_line"`
	CWEID        string  `json:"cwe_id"`
	Severity     string  `json:"severity"`
	Confidence   float32 `json:"confidence"`
	AIVerdict    string  `json:"ai_verdict"`
	AIConfidence float32 `json:"ai_confidence"`
}

// GoldenTestInput represents the input for golden test.
type GoldenTestInput struct {
	TaskID       string               `json:"task_id"`
	SASTFindings []GoldenFindingInput `json:"sast_findings"`
	AIFindings   []GoldenFindingInput `json:"ai_findings"`
}

// GoldenExpectedMetrics is the expected metrics.
type GoldenExpectedMetrics struct {
	InputSASTCount        int32 `json:"input_sast_count"`
	InputAICount          int32 `json:"input_ai_count"`
	OutputCount           int32 `json:"output_count"`
	MergedCount           int32 `json:"merged_count"`
	RemovedFalsePositives int32 `json:"removed_false_positives"`
	AddedAIFindings       int32 `json:"added_ai_findings"`
}

// GoldenExpectedOutput is the expected output.
type GoldenExpectedOutput struct {
	FusedFindingCount     int32                 `json:"fused_finding_count"`
	TotalCount            int32                 `json:"total_count"`
	RemovedFalsePositives int32                 `json:"removed_false_positives"`
	AddedAIFindings       int32                 `json:"added_ai_findings"`
	Metrics               GoldenExpectedMetrics `json:"metrics"`
	MergeGroupCount       int32                 `json:"merge_group_count"`
	FilteredCount         int32                 `json:"filtered_count"`
}

// GoldenTestCase represents a golden test case.
type GoldenTestCase struct {
	Description    string               `json:"description"`
	TestCase       string               `json:"test_case"`
	Input          GoldenTestInput      `json:"input"`
	ExpectedOutput GoldenExpectedOutput `json:"expected_output"`
}

// severityFromString converts a severity string to proto enum.
func severityFromString(s string) pb.Severity {
	switch s {
	case "SEVERITY_CRITICAL":
		return pb.Severity_SEVERITY_CRITICAL
	case "SEVERITY_HIGH":
		return pb.Severity_SEVERITY_HIGH
	case "SEVERITY_MEDIUM":
		return pb.Severity_SEVERITY_MEDIUM
	case "SEVERITY_LOW":
		return pb.Severity_SEVERITY_LOW
	case "SEVERITY_INFO":
		return pb.Severity_SEVERITY_INFO
	default:
		return pb.Severity_SEVERITY_UNSPECIFIED
	}
}

// verdictFromString converts a verdict string to proto enum.
func verdictFromString(s string) pb.AIVerdict {
	switch s {
	case "AI_VERDICT_TRUE_POSITIVE":
		return pb.AIVerdict_AI_VERDICT_TRUE_POSITIVE
	case "AI_VERDICT_FALSE_POSITIVE":
		return pb.AIVerdict_AI_VERDICT_FALSE_POSITIVE
	case "AI_VERDICT_LIKELY_TRUE":
		return pb.AIVerdict_AI_VERDICT_LIKELY_TRUE
	case "AI_VERDICT_LIKELY_FALSE":
		return pb.AIVerdict_AI_VERDICT_LIKELY_FALSE
	case "AI_VERDICT_UNCERTAIN":
		return pb.AIVerdict_AI_VERDICT_UNCERTAIN
	case "AI_VERDICT_NEEDS_MANUAL":
		return pb.AIVerdict_AI_VERDICT_NEEDS_MANUAL
	default:
		return pb.AIVerdict_AI_VERDICT_UNSPECIFIED
	}
}

// convertFinding converts a golden input to proto UnifiedFinding.
func convertFinding(gf GoldenFindingInput) *pb.UnifiedFinding {
	return &pb.UnifiedFinding{
		FindingId:  gf.FindingID,
		TaskId:     gf.TaskID,
		SourceTool: gf.SourceTool,
		Location: &pb.LocationInfo{
			FilePath:  gf.FilePath,
			StartLine: gf.StartLine,
			EndLine:   gf.EndLine,
		},
		CweId:        gf.CWEID,
		Severity:     severityFromString(gf.Severity),
		Confidence:   gf.Confidence,
		AiVerdict:    verdictFromString(gf.AIVerdict),
		AiConfidence: gf.AIConfidence,
	}
}

// TestFusionPipeline_Golden tests fusion results against golden files.
// 依据: test-gates.md §7 黄金文件纪律
func TestFusionPipeline_Golden(t *testing.T) {
	goldenFiles := []string{
		"../../../../tests/golden/fusion_result_basic.json",
		"../../../../tests/golden/fusion_result_filter.json",
	}

	for _, goldenFile := range goldenFiles {
		t.Run(goldenFile, func(t *testing.T) {
			// Read golden file
			data, err := os.ReadFile(goldenFile)
			if err != nil {
				t.Fatalf("failed to read golden file: %v", err)
			}

			var testCase GoldenTestCase
			if err := json.Unmarshal(data, &testCase); err != nil {
				t.Fatalf("failed to unmarshal golden file: %v", err)
			}

			// Create pipeline
			pipeline := fusion.NewFusionPipeline()

			// Convert inputs to proto structs
			sastFindings := make([]*pb.UnifiedFinding, 0, len(testCase.Input.SASTFindings))
			for _, sf := range testCase.Input.SASTFindings {
				sastFindings = append(sastFindings, convertFinding(sf))
			}
			aiFindings := make([]*pb.UnifiedFinding, 0, len(testCase.Input.AIFindings))
			for _, af := range testCase.Input.AIFindings {
				aiFindings = append(aiFindings, convertFinding(af))
			}

			// Prepare request
			req := &pb.FuseResultsRequest{
				Metadata: &pb.RequestMetadata{
					RequestId: "golden-test-" + testCase.TestCase,
				},
				TaskId: testCase.Input.TaskID,
			}

			// Execute fusion
			result, err := pipeline.Execute(
				context.Background(),
				req,
				sastFindings,
				aiFindings,
			)
			if err != nil {
				t.Fatalf("pipeline.Execute failed: %v", err)
			}

			// Compare key fields against expected
			expected := testCase.ExpectedOutput

			if result.GetTotalCount() != expected.TotalCount {
				t.Errorf("total_count mismatch: expected %d, got %d",
					expected.TotalCount, result.GetTotalCount())
			}

			if result.GetRemovedFalsePositives() != expected.RemovedFalsePositives {
				t.Errorf("removed_false_positives mismatch: expected %d, got %d",
					expected.RemovedFalsePositives, result.GetRemovedFalsePositives())
			}

			if result.GetAddedAiFindings() != expected.AddedAIFindings {
				t.Errorf("added_ai_findings mismatch: expected %d, got %d",
					expected.AddedAIFindings, result.GetAddedAiFindings())
			}

			if result.GetMetrics().GetMergedCount() != expected.Metrics.MergedCount {
				t.Errorf("metrics.merged_count mismatch: expected %d, got %d",
					expected.Metrics.MergedCount, result.GetMetrics().GetMergedCount())
			}

			if result.GetMetrics().GetRemovedFalsePositives() != expected.Metrics.RemovedFalsePositives {
				t.Errorf("metrics.removed_false_positives mismatch: expected %d, got %d",
					expected.Metrics.RemovedFalsePositives, result.GetMetrics().GetRemovedFalsePositives())
			}

			if int32(len(result.GetReport().GetMergeGroups())) != expected.MergeGroupCount {
				t.Errorf("merge_groups count mismatch: expected %d, got %d",
					expected.MergeGroupCount, len(result.GetReport().GetMergeGroups()))
			}

			if int32(len(result.GetReport().GetFiltered())) != expected.FilteredCount {
				t.Errorf("filtered count mismatch: expected %d, got %d",
					expected.FilteredCount, len(result.GetReport().GetFiltered()))
			}
		})
	}
}
