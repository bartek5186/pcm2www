package singleinstance

import (
	"path/filepath"
	"testing"
)

func TestAcquireAllowsOnlyOneLocalInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if second, err := Acquire(path); err == nil {
		second.Release()
		t.Fatal("second lock unexpectedly succeeded")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	first = nil
	third, err := Acquire(path)
	if err != nil {
		t.Fatalf("lock was not reusable after release: %v", err)
	}
	if err := third.Release(); err != nil {
		t.Fatal(err)
	}
}
