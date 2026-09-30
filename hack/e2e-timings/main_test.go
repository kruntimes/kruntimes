package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleStream = `{"Time":"2026-01-01T00:00:00Z","Action":"start","Package":"github.com/kruntimes/kruntimes/test/e2e"}
{"Time":"2026-01-01T00:00:00Z","Action":"run","Package":"github.com/kruntimes/kruntimes/test/e2e","Test":"TestFast"}
{"Time":"2026-01-01T00:00:00Z","Action":"output","Package":"github.com/kruntimes/kruntimes/test/e2e","Test":"TestFast","Output":"=== RUN   TestFast\n"}
{"Time":"2026-01-01T00:00:01Z","Action":"pass","Package":"github.com/kruntimes/kruntimes/test/e2e","Test":"TestFast","Elapsed":1.25}
{"Time":"2026-01-01T00:00:02Z","Action":"run","Package":"github.com/kruntimes/kruntimes/test/e2e","Test":"TestSlow"}
{"Time":"2026-01-01T00:00:02Z","Action":"output","Package":"github.com/kruntimes/kruntimes/test/e2e","Test":"TestSlow","Output":"=== RUN   TestSlow\n"}
{"Time":"2026-01-01T00:00:12Z","Action":"fail","Package":"github.com/kruntimes/kruntimes/test/e2e","Test":"TestSlow","Elapsed":10.5}
{"Time":"2026-01-01T00:00:13Z","Action":"skip","Package":"github.com/kruntimes/kruntimes/test/e2e","Test":"TestSkipped","Elapsed":0.01}
{"Time":"2026-01-01T00:00:13Z","Action":"fail","Package":"github.com/kruntimes/kruntimes/test/e2e","Elapsed":13}
`

func TestCollectSummarizesEvents(t *testing.T) {
	var stdout bytes.Buffer
	rep, err := collect(strings.NewReader(sampleStream), &stdout)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	if got, want := len(rep.Tests), 3; got != want {
		t.Fatalf("tests = %d, want %d", got, want)
	}
	if rep.Passed != 1 || rep.Failed != 1 || rep.Skipped != 1 {
		t.Fatalf("counts = pass %d fail %d skip %d, want 1/1/1", rep.Passed, rep.Failed, rep.Skipped)
	}
	if rep.TestDurationSeconds < 11.7 || rep.TestDurationSeconds > 11.8 {
		t.Fatalf("sum of durations = %v, want ~11.76", rep.TestDurationSeconds)
	}
	if rep.WallClockSeconds != 13 {
		t.Fatalf("wall clock = %v, want 13", rep.WallClockSeconds)
	}
	if !strings.Contains(stdout.String(), "=== RUN   TestFast") {
		t.Fatalf("test output was not echoed: %q", stdout.String())
	}
}

func TestRunWritesReportAndMarkdown(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	markdownPath := filepath.Join(dir, "summary.md")
	baselinePath := filepath.Join(dir, "baseline.json")

	baseline := report{SchemaVersion: 1, Tests: []testResult{
		{Name: "TestFast", Status: "pass", DurationSeconds: 0.5},
		{Name: "TestSlow", Status: "pass", DurationSeconds: 1.0},
	}}
	payload, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(baselinePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := run(strings.NewReader(sampleStream), &stdout, reportPath, markdownPath, baselinePath, 2, 25, 2, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	var written report
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if len(written.Tests) != 3 || written.Tests[0].Name != "TestSlow" {
		t.Fatalf("report tests = %#v, want TestSlow first", written.Tests)
	}

	summary := stdout.String()
	if !strings.Contains(summary, "Slowest 2 tests") {
		t.Fatalf("summary missing slowest table: %q", summary)
	}
	if !strings.Contains(summary, "Timing regressions against baseline") {
		t.Fatalf("summary missing regression section: %q", summary)
	}

	markdown, err := os.ReadFile(markdownPath)
	if err != nil {
		t.Fatalf("read markdown: %v", err)
	}
	if !strings.Contains(string(markdown), "| `TestSlow` | fail | 10.50s |") {
		t.Fatalf("markdown missing table row: %s", markdown)
	}
}

func TestCompareBaselineIgnoresSmallChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	baseline := report{SchemaVersion: 1, Tests: []testResult{{Name: "TestFast", Status: "pass", DurationSeconds: 1.0}}}
	payload, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	current := report{Tests: []testResult{{Name: "TestFast", Status: "pass", DurationSeconds: 1.2}}}
	regressions, err := compareBaseline(path, current, 25, 2)
	if err != nil {
		t.Fatalf("compareBaseline: %v", err)
	}
	if len(regressions) != 0 {
		t.Fatalf("regressions = %#v, want none", regressions)
	}
}

func TestCompareBaselineMissingFileIsNotAnError(t *testing.T) {
	regressions, err := compareBaseline(filepath.Join(t.TempDir(), "absent.json"), report{}, 25, 2)
	if err != nil {
		t.Fatalf("compareBaseline: %v", err)
	}
	if len(regressions) != 0 {
		t.Fatalf("regressions = %#v, want none", regressions)
	}
}
