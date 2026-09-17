package toolcache

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestEnsurePublishesOnce(t *testing.T) {
	root := t.TempDir()
	var calls int
	var mu sync.Mutex
	populate := func(_ context.Context, staging string) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return os.WriteFile(filepath.Join(staging, "go"), []byte("toolchain"), 0o644)
	}

	const workers = 8
	errs := make(chan error, workers)
	for range workers {
		go func() {
			_, err := Ensure(context.Background(), root, "go/1.26.0", populate)
			errs <- err
		}()
	}
	for range workers {
		if err := <-errs; err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("populate calls = %d, want 1", calls)
	}
	entry := filepath.Join(root, "go", "1.26.0")
	if !complete(entry) {
		t.Fatalf("cache entry %q was not marked complete", entry)
	}
}

func TestEnsureRejectsUnsafeKey(t *testing.T) {
	_, err := Ensure(context.Background(), t.TempDir(), "../escape", func(context.Context, string) error { return nil })
	if err == nil {
		t.Fatal("Ensure() error = nil, want invalid key error")
	}
}

func TestEnsureRejectsReservedTopLevelKey(t *testing.T) {
	_, err := Ensure(context.Background(), t.TempDir(), "bin/tool", func(context.Context, string) error { return nil })
	if err == nil {
		t.Fatal("Ensure() error = nil, want reserved path error")
	}
}
