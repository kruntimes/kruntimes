package main

import (
	"testing"
	"time"
)

func TestSummarize(t *testing.T) {
	got := summarize([]time.Duration{
		100 * time.Millisecond,
		10 * time.Millisecond,
		50 * time.Millisecond,
	})

	if got.Count != 3 {
		t.Fatalf("Count = %d, want 3", got.Count)
	}
	if got.MinMS != 10 {
		t.Fatalf("MinMS = %v, want 10", got.MinMS)
	}
	if got.P50MS != 50 {
		t.Fatalf("P50MS = %v, want 50", got.P50MS)
	}
	if got.MaxMS != 100 {
		t.Fatalf("MaxMS = %v, want 100", got.MaxMS)
	}
}

func TestPercentileInterpolates(t *testing.T) {
	values := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		30 * time.Millisecond,
	}
	got := percentile(values, 0.95)
	want := 29 * time.Millisecond
	if got != want {
		t.Fatalf("percentile = %v, want %v", got, want)
	}
}
