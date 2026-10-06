// Command e2e-timings consumes the JSON event stream produced by
// `go test -json` for the E2E suite and turns it into two artifacts:
//
//   - a JSON report with per-test durations, suitable for CI artifacts and
//     later regression tooling, and
//   - a human-readable summary (stdout and optional Markdown) listing the
//     slowest tests.
//
// It also passes the original test output through unchanged so the suite log
// keeps the same diagnostics as a plain `go test -v` run.
//
// Usage:
//
//	go test ./test/e2e/... -json | go run ./hack/e2e-timings -report e2e-timings.json
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// maxEventBytes bounds a single JSON event line. Test output lines can be large
// (for example a dumped YAML manifest), so the scanner buffer is generous.
const maxEventBytes = 4 << 20

// testEvent mirrors the subset of `go test -json` events this reporter needs.
type testEvent struct {
	Time    time.Time `json:"Time"`
	Action  string    `json:"Action"`
	Package string    `json:"Package"`
	Test    string    `json:"Test"`
	Elapsed float64   `json:"Elapsed"`
	Output  string    `json:"Output"`
}

// testResult is one test case in the report.
type testResult struct {
	Name            string  `json:"name"`
	Status          string  `json:"status"`
	DurationSeconds float64 `json:"durationSeconds"`
}

// report is the machine-readable timing artifact.
type report struct {
	SchemaVersion       int          `json:"schemaVersion"`
	GeneratedAt         time.Time    `json:"generatedAt"`
	Packages            []string     `json:"packages"`
	Parallelism         int          `json:"parallelism,omitempty"`
	Passed              int          `json:"passed"`
	Failed              int          `json:"failed"`
	Skipped             int          `json:"skipped"`
	WallClockSeconds    float64      `json:"wallClockSeconds"`
	TestDurationSeconds float64      `json:"testDurationSeconds"`
	Tests               []testResult `json:"tests"`
}

// slowdown describes how a test's duration changed against a baseline.
type slowdown struct {
	Name            string  `json:"name"`
	BaselineSeconds float64 `json:"baselineSeconds"`
	CurrentSeconds  float64 `json:"currentSeconds"`
	ChangePercent   float64 `json:"changePercent"`
}

// baselineComparison summarizes the current run against a previous report.
// Wall-clock comparison is only meaningful when both runs used the same
// parallel fan-out, so it is tracked separately from the advisory per-test
// slowdowns, whose durations move with scheduling.
type baselineComparison struct {
	Path                string     `json:"path"`
	BaselineParallelism int        `json:"baselineParallelism,omitempty"`
	CurrentParallelism  int        `json:"currentParallelism,omitempty"`
	BaselineWallClock   float64    `json:"baselineWallClockSeconds"`
	WallClockChange     float64    `json:"wallClockChangePercent"`
	BaselineTotal       float64    `json:"baselineTestDurationSeconds"`
	TotalChange         float64    `json:"testDurationChangePercent"`
	Slowdowns           []slowdown `json:"slowdowns,omitempty"`
}

func main() {
	reportPath := flag.String("report", "", "write the JSON timing report to this path")
	markdownPath := flag.String("markdown", "", "write a Markdown summary table to this path")
	baselinePath := flag.String("baseline", "", "compare against a previous JSON report at this path")
	parallel := flag.Int("parallel", 0, "parallel fan-out the suite ran with, recorded in the report")
	top := flag.Int("top", 15, "number of slowest tests to print")
	regressionPercent := flag.Float64("regression-percent", 50, "flag tests slower than their baseline by at least this percentage")
	regressionSeconds := flag.Float64("regression-seconds", 5, "minimum absolute slowdown in seconds before flagging a test")
	quiet := flag.Bool("quiet", false, "suppress the summary printed to stdout")
	flag.Parse()

	if err := run(os.Stdin, os.Stdout, *reportPath, *markdownPath, *baselinePath, *parallel, *top, *regressionPercent, *regressionSeconds, *quiet); err != nil {
		fmt.Fprintf(os.Stderr, "e2e-timings: %v\n", err)
		os.Exit(1)
	}
}

func run(stdin io.Reader, stdout io.Writer, reportPath, markdownPath, baselinePath string, parallel, top int, regressionPercent, regressionSeconds float64, quiet bool) error {
	rep, err := collect(stdin, stdout)
	if err != nil {
		return err
	}
	rep.Parallelism = parallel
	if top <= 0 || top > len(rep.Tests) {
		top = len(rep.Tests)
	}

	// Tests are reported slowest first; that ordering is also the report order.
	sort.SliceStable(rep.Tests, func(i, j int) bool {
		if rep.Tests[i].DurationSeconds != rep.Tests[j].DurationSeconds {
			return rep.Tests[i].DurationSeconds > rep.Tests[j].DurationSeconds
		}
		return rep.Tests[i].Name < rep.Tests[j].Name
	})

	var comparison *baselineComparison
	if baselinePath != "" {
		comparison = compareBaseline(baselinePath, rep, regressionPercent, regressionSeconds)
	}

	if !quiet {
		writeSummary(stdout, rep, top, comparison)
	}
	if reportPath != "" {
		if err := writeJSON(reportPath, rep); err != nil {
			return err
		}
	}
	if markdownPath != "" {
		if err := writeMarkdown(markdownPath, rep, top, comparison, regressionPercent, regressionSeconds); err != nil {
			return err
		}
	}
	return nil
}

// collect reads the JSON event stream, echoes test output to stdout, and folds
// the terminal events into a report.
func collect(stdin io.Reader, stdout io.Writer) (report, error) {
	rep := report{SchemaVersion: 1, GeneratedAt: time.Now().UTC()}
	tests := map[string]testResult{}
	packages := map[string]struct{}{}
	var first, last time.Time

	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 64<<10), maxEventBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		var event testEvent
		if err := json.Unmarshal(line, &event); err != nil {
			// Not a JSON event (for example output written directly to the
			// terminal). Echo it so nothing is lost.
			fmt.Fprintf(stdout, "%s\n", line)
			continue
		}
		if event.Output != "" {
			fmt.Fprint(stdout, event.Output)
		}
		if !event.Time.IsZero() {
			if first.IsZero() || event.Time.Before(first) {
				first = event.Time
			}
			if event.Time.After(last) {
				last = event.Time
			}
		}
		if event.Package != "" {
			packages[event.Package] = struct{}{}
		}
		if event.Test == "" {
			continue
		}
		switch event.Action {
		case "pass", "fail", "skip":
			tests[event.Test] = testResult{Name: event.Test, Status: event.Action, DurationSeconds: event.Elapsed}
		}
	}
	if err := scanner.Err(); err != nil {
		return report{}, fmt.Errorf("read test events: %w", err)
	}

	for name := range packages {
		rep.Packages = append(rep.Packages, name)
	}
	sort.Strings(rep.Packages)
	for _, result := range tests {
		rep.Tests = append(rep.Tests, result)
		rep.TestDurationSeconds += result.DurationSeconds
		switch result.Status {
		case "pass":
			rep.Passed++
		case "fail":
			rep.Failed++
		case "skip":
			rep.Skipped++
		}
	}
	rep.WallClockSeconds = last.Sub(first).Seconds()
	if rep.WallClockSeconds < 0 {
		rep.WallClockSeconds = 0
	}
	return rep, nil
}

func writeJSON(path string, rep report) error {
	payload, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	payload = append(payload, '\n')
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

func writeSummary(w io.Writer, rep report, top int, comparison *baselineComparison) {
	fmt.Fprintf(w, "\nE2E timing summary: %d tests (%d passed, %d failed, %d skipped)\n",
		len(rep.Tests), rep.Passed, rep.Failed, rep.Skipped)
	if rep.Parallelism > 0 {
		fmt.Fprintf(w, "  approx wall clock:        %.2fs (parallelism %d)\n", rep.WallClockSeconds, rep.Parallelism)
	} else {
		fmt.Fprintf(w, "  approx wall clock:        %.2fs\n", rep.WallClockSeconds)
	}
	fmt.Fprintf(w, "  sum of test durations:    %.2fs\n", rep.TestDurationSeconds)

	if comparison != nil {
		fmt.Fprintf(w, "\nBaseline comparison (%s):\n", comparison.Path)
		if comparison.CurrentParallelism == comparison.BaselineParallelism {
			fmt.Fprintf(w, "  wall clock:   %.2fs -> %.2fs (%+.1f%%)\n", comparison.BaselineWallClock, rep.WallClockSeconds, comparison.WallClockChange)
		} else {
			fmt.Fprintf(w, "  wall clock:   not comparable (baseline parallelism %d, current %d)\n", comparison.BaselineParallelism, comparison.CurrentParallelism)
		}
		fmt.Fprintf(w, "  test totals:  %.2fs -> %.2fs (%+.1f%%)\n", comparison.BaselineTotal, rep.TestDurationSeconds, comparison.TotalChange)
		if len(comparison.Slowdowns) == 0 {
			fmt.Fprintf(w, "  per-test slowdowns: none above the configured threshold\n")
		} else {
			fmt.Fprintf(w, "  per-test slowdowns (advisory; parallel scheduling adds noise):\n")
			for _, s := range comparison.Slowdowns {
				fmt.Fprintf(w, "    %+.0f%%  %6.2fs -> %6.2fs  %s\n", s.ChangePercent, s.BaselineSeconds, s.CurrentSeconds, s.Name)
			}
		}
	}

	fmt.Fprintf(w, "\nSlowest %d tests:\n", top)
	for _, t := range rep.Tests[:top] {
		fmt.Fprintf(w, "  %7.2fs  %-4s  %s\n", t.DurationSeconds, t.Status, t.Name)
	}
}

func writeMarkdown(path string, rep report, top int, comparison *baselineComparison, regressionPercent, regressionSeconds float64) error {
	var b strings.Builder
	b.WriteString("## E2E timings\n\n")
	fmt.Fprintf(&b, "- Tests: %d (passed %d, failed %d, skipped %d)\n", len(rep.Tests), rep.Passed, rep.Failed, rep.Skipped)
	if rep.Parallelism > 0 {
		fmt.Fprintf(&b, "- Approx wall clock: %.2fs (parallelism %d)\n", rep.WallClockSeconds, rep.Parallelism)
	} else {
		fmt.Fprintf(&b, "- Approx wall clock: %.2fs\n", rep.WallClockSeconds)
	}
	fmt.Fprintf(&b, "- Sum of test durations: %.2fs\n", rep.TestDurationSeconds)

	if comparison != nil {
		b.WriteString("\n### Baseline comparison\n\n| Metric | Baseline | Current | Change |\n| --- | ---: | ---: | ---: |\n")
		if comparison.CurrentParallelism == comparison.BaselineParallelism {
			fmt.Fprintf(&b, "| Test-phase wall clock | %.2fs | %.2fs | %+.1f%% |\n", comparison.BaselineWallClock, rep.WallClockSeconds, comparison.WallClockChange)
		} else {
			fmt.Fprintf(&b, "| Test-phase wall clock | %.2fs (parallelism %d) | %.2fs (parallelism %d) | n/a |\n",
				comparison.BaselineWallClock, comparison.BaselineParallelism, rep.WallClockSeconds, comparison.CurrentParallelism)
		}
		fmt.Fprintf(&b, "| Sum of test durations | %.2fs | %.2fs | %+.1f%% |\n", comparison.BaselineTotal, rep.TestDurationSeconds, comparison.TotalChange)
		if len(comparison.Slowdowns) > 0 {
			fmt.Fprintf(&b, "\nPer-test slowdowns above +%.0f%% and +%.1fs (advisory; parallel scheduling adds noise):\n\n",
				regressionPercent, regressionSeconds)
			b.WriteString("| Test | Baseline | Current | Change |\n| --- | ---: | ---: | ---: |\n")
			for _, s := range comparison.Slowdowns {
				fmt.Fprintf(&b, "| `%s` | %.2fs | %.2fs | %+.0f%% |\n", s.Name, s.BaselineSeconds, s.CurrentSeconds, s.ChangePercent)
			}
		}
	}

	fmt.Fprintf(&b, "\n### Slowest %d tests\n\n| Test | Status | Duration |\n| --- | --- | ---: |\n", top)
	for _, t := range rep.Tests[:top] {
		fmt.Fprintf(&b, "| `%s` | %s | %.2fs |\n", t.Name, t.Status, t.DurationSeconds)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write markdown summary: %w", err)
	}
	return nil
}

// compareBaseline loads a previous report and summarizes how the current run
// compares. Wall clock is compared directly; per-test changes above the
// configured thresholds are listed as advisory slowdowns. A missing or
// unreadable baseline is ignored: the comparison must never fail the run.
func compareBaseline(path string, current report, regressionPercent, regressionSeconds float64) *baselineComparison {
	payload, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "e2e-timings: ignoring baseline %s: %v\n", path, err)
		}
		return nil
	}
	var baseline report
	if err := json.Unmarshal(payload, &baseline); err != nil {
		// A stale or hand-edited baseline must never fail the E2E run.
		fmt.Fprintf(os.Stderr, "e2e-timings: ignoring baseline %s: %v\n", path, err)
		return nil
	}

	comparison := &baselineComparison{
		Path:                path,
		BaselineParallelism: baseline.Parallelism,
		CurrentParallelism:  current.Parallelism,
		BaselineWallClock:   baseline.WallClockSeconds,
		BaselineTotal:       baseline.TestDurationSeconds,
	}
	comparison.WallClockChange = changePercent(baseline.WallClockSeconds, current.WallClockSeconds)
	comparison.TotalChange = changePercent(baseline.TestDurationSeconds, current.TestDurationSeconds)

	previous := make(map[string]float64, len(baseline.Tests))
	for _, t := range baseline.Tests {
		previous[t.Name] = t.DurationSeconds
	}
	for _, t := range current.Tests {
		before, ok := previous[t.Name]
		if !ok || before <= 0 {
			continue
		}
		delta := t.DurationSeconds - before
		percent := delta / before * 100
		if delta >= regressionSeconds && percent >= regressionPercent {
			comparison.Slowdowns = append(comparison.Slowdowns, slowdown{
				Name:            t.Name,
				BaselineSeconds: before,
				CurrentSeconds:  t.DurationSeconds,
				ChangePercent:   percent,
			})
		}
	}
	sort.SliceStable(comparison.Slowdowns, func(i, j int) bool {
		return comparison.Slowdowns[i].ChangePercent > comparison.Slowdowns[j].ChangePercent
	})
	return comparison
}

func changePercent(before, after float64) float64 {
	if before <= 0 {
		return 0
	}
	return (after - before) / before * 100
}
