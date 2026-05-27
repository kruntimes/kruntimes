package controller

import (
	"reflect"
	"testing"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestTopoSort_NoDeps(t *testing.T) {
	jobs := map[string]v1alpha1.JobSpec{
		"a": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "s1", Run: "echo a"}}},
		"b": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "s1", Run: "echo b"}}},
	}
	sorted, err := topoSort(jobs)
	if err != nil {
		t.Fatal(err)
	}
	// Both jobs have no deps, order between them is arbitrary.
	if len(sorted) != 2 {
		t.Errorf("expected 2, got %d", len(sorted))
	}
}

func TestTopoSort_LinearDeps(t *testing.T) {
	jobs := map[string]v1alpha1.JobSpec{
		"a": {RunsOn: "bash", Needs: []string{}, Steps: []v1alpha1.StepSpec{{Name: "s1", Run: "echo a"}}},
		"b": {RunsOn: "bash", Needs: []string{"a"}, Steps: []v1alpha1.StepSpec{{Name: "s1", Run: "echo b"}}},
		"c": {RunsOn: "bash", Needs: []string{"b"}, Steps: []v1alpha1.StepSpec{{Name: "s1", Run: "echo c"}}},
	}
	sorted, err := topoSort(jobs)
	if err != nil {
		t.Fatal(err)
	}
	// Must be a, b, c in that order.
	if !reflect.DeepEqual(sorted, []string{"a", "b", "c"}) {
		t.Errorf("expected [a b c], got %v", sorted)
	}
}

func TestTopoSort_DiamondDeps(t *testing.T) {
	// a -> b, a -> c, b -> d, c -> d
	jobs := map[string]v1alpha1.JobSpec{
		"a": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "s1", Run: "echo a"}}},
		"b": {RunsOn: "bash", Needs: []string{"a"}, Steps: []v1alpha1.StepSpec{{Name: "s1", Run: "echo b"}}},
		"c": {RunsOn: "bash", Needs: []string{"a"}, Steps: []v1alpha1.StepSpec{{Name: "s1", Run: "echo c"}}},
		"d": {RunsOn: "bash", Needs: []string{"b", "c"}, Steps: []v1alpha1.StepSpec{{Name: "s1", Run: "echo d"}}},
	}
	sorted, err := topoSort(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if len(sorted) != 4 {
		t.Fatalf("expected 4, got %d", len(sorted))
	}
	// a must be first.
	if sorted[0] != "a" {
		t.Errorf("expected a first, got %s", sorted[0])
	}
	// d must be last (has two deps).
	if sorted[3] != "d" {
		t.Errorf("expected d last, got %s", sorted[3])
	}
	// b and c come after a, before d.
	aIdx := indexOf(sorted, "a")
	bIdx := indexOf(sorted, "b")
	cIdx := indexOf(sorted, "c")
	dIdx := indexOf(sorted, "d")
	if bIdx <= aIdx || cIdx <= aIdx || dIdx <= bIdx || dIdx <= cIdx {
		t.Errorf("order violation: %v", sorted)
	}
}

func TestTopoSort_Cycle(t *testing.T) {
	jobs := map[string]v1alpha1.JobSpec{
		"a": {RunsOn: "bash", Needs: []string{"b"}, Steps: []v1alpha1.StepSpec{{Name: "s1", Run: "echo a"}}},
		"b": {RunsOn: "bash", Needs: []string{"a"}, Steps: []v1alpha1.StepSpec{{Name: "s1", Run: "echo b"}}},
	}
	_, err := topoSort(jobs)
	if err == nil {
		t.Error("expected cycle error, got nil")
	}
}

func TestTopoSort_SelfCycle(t *testing.T) {
	jobs := map[string]v1alpha1.JobSpec{
		"a": {RunsOn: "bash", Needs: []string{"a"}, Steps: []v1alpha1.StepSpec{{Name: "s1", Run: "echo a"}}},
	}
	_, err := topoSort(jobs)
	if err == nil {
		t.Error("expected cycle error for self-reference")
	}
}

func TestDetectImplicitNeeds_CrossJobRef(t *testing.T) {
	jobs := map[string]v1alpha1.JobSpec{
		"build": {
			RunsOn: "bash",
			Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "echo done >> outputs"}},
		},
		"deploy": {
			RunsOn: "bash",
			Steps: []v1alpha1.StepSpec{
				{Name: "deploy", Run: "echo deploying ${{ jobs.build.outputs.artifact }}"},
			},
		},
	}
	merged := detectImplicitNeeds(jobs)
	deployJob := merged["deploy"]
	if len(deployJob.Needs) == 0 || deployJob.Needs[0] != "build" {
		t.Errorf("deploy should implicitly need build, got needs=%v", deployJob.Needs)
	}
}

func TestDetectImplicitNeeds_ExplicitAndImplicit(t *testing.T) {
	jobs := map[string]v1alpha1.JobSpec{
		"a": {
			RunsOn: "bash",
			Steps:  []v1alpha1.StepSpec{{Name: "s1", Run: "echo a >> outputs"}},
		},
		"b": {
			RunsOn: "bash",
			Steps:  []v1alpha1.StepSpec{{Name: "s1", Run: "echo b >> outputs"}},
		},
		"c": {
			RunsOn: "bash",
			Needs:  []string{"a"},
			Steps: []v1alpha1.StepSpec{
				{Name: "s1", Run: "echo ${{ jobs.b.outputs.result }}"},
			},
		},
	}
	merged := detectImplicitNeeds(jobs)
	cJob := merged["c"]
	needs := make(map[string]bool)
	for _, n := range cJob.Needs {
		needs[n] = true
	}
	if !needs["a"] || !needs["b"] {
		t.Errorf("c should have both explicit need 'a' and implicit need 'b', got %v", cJob.Needs)
	}
	if len(cJob.Needs) != 2 {
		t.Errorf("c should have exactly 2 needs, got %d", len(cJob.Needs))
	}
}

func indexOf(s []string, v string) int {
	for i, e := range s {
		if e == v {
			return i
		}
	}
	return -1
}
