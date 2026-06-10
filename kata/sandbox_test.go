package kata

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
)

func TestSandboxID(t *testing.T) {
	got := sandboxID("abc-123")
	if got != "kata-abc-123-sandbox" {
		t.Fatalf("sandboxID(%q) = %q, want %q", "abc-123", got, "kata-abc-123-sandbox")
	}
}

func testCtrClient(t *testing.T) (*CtrClient, string) {
	t.Helper()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "args.log")

	script := filepath.Join(dir, "ctr")
	content := "#!/bin/sh\necho \"$@\" >> " + logFile + "\n"
	if err := os.WriteFile(script, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}

	logger := hclog.NewNullLogger()
	return NewCtrClient(script, "/test.sock", "test-ns", logger), logFile
}

func TestSandboxGetOrCreate(t *testing.T) {
	ctr, logFile := testCtrClient(t)
	mgr := NewSandboxManager(ctr, hclog.NewNullLogger())
	ctx := context.Background()

	sb, err := mgr.GetOrCreate(ctx, "alloc-1", "pause:3.9", "io.containerd.kata.v2", "", "")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if sb.ID != "kata-alloc-1-sandbox" {
		t.Fatalf("sandbox ID = %q, want %q", sb.ID, "kata-alloc-1-sandbox")
	}
	if sb.refCount.Load() != 1 {
		t.Fatalf("refCount = %d, want 1", sb.refCount.Load())
	}

	logs, _ := os.ReadFile(logFile)
	lines := strings.Split(strings.TrimSpace(string(logs)), "\n")
	foundCreate := false
	foundStart := false
	for _, line := range lines {
		if strings.Contains(line, "container create") && strings.Contains(line, "kata-alloc-1-sandbox") {
			foundCreate = true
		}
		if strings.Contains(line, "task start -d") && strings.Contains(line, "kata-alloc-1-sandbox") {
			foundStart = true
		}
	}
	if !foundCreate {
		t.Error("expected ctr container create call for sandbox")
	}
	if !foundStart {
		t.Error("expected ctr task start -d call for sandbox")
	}
}

func TestSandboxHostname(t *testing.T) {
	ctr, logFile := testCtrClient(t)
	mgr := NewSandboxManager(ctr, hclog.NewNullLogger())
	ctx := context.Background()

	mgr.GetOrCreate(ctx, "alloc-host", "pause:3.9", "kata", "", "myapp")

	logs, _ := os.ReadFile(logFile)
	if !strings.Contains(string(logs), "--hostname myapp") {
		t.Errorf("expected --hostname myapp in sandbox create args, got: %s", string(logs))
	}
}

func TestSandboxReuse(t *testing.T) {
	ctr, _ := testCtrClient(t)
	mgr := NewSandboxManager(ctr, hclog.NewNullLogger())
	ctx := context.Background()

	sb1, err := mgr.GetOrCreate(ctx, "alloc-2", "pause:3.9", "kata", "", "")
	if err != nil {
		t.Fatal(err)
	}

	sb2, err := mgr.GetOrCreate(ctx, "alloc-2", "pause:3.9", "kata", "", "")
	if err != nil {
		t.Fatal(err)
	}

	if sb1.ID != sb2.ID {
		t.Fatalf("expected same sandbox, got %q and %q", sb1.ID, sb2.ID)
	}
	if sb2.refCount.Load() != 2 {
		t.Fatalf("refCount = %d, want 2", sb2.refCount.Load())
	}
}

func TestSandboxRelease(t *testing.T) {
	ctr, _ := testCtrClient(t)
	mgr := NewSandboxManager(ctr, hclog.NewNullLogger())
	ctx := context.Background()

	mgr.GetOrCreate(ctx, "alloc-3", "pause:3.9", "kata", "", "")
	mgr.GetOrCreate(ctx, "alloc-3", "pause:3.9", "kata", "", "")

	mgr.Release(ctx, "alloc-3")

	mgr.mu.Lock()
	sb := mgr.sandboxes["alloc-3"]
	mgr.mu.Unlock()
	if sb == nil {
		t.Fatal("sandbox should still exist after first release")
	}
	if sb.refCount.Load() != 1 {
		t.Fatalf("refCount = %d, want 1", sb.refCount.Load())
	}

	mgr.Release(ctx, "alloc-3")

	mgr.mu.Lock()
	_, exists := mgr.sandboxes["alloc-3"]
	mgr.mu.Unlock()
	if exists {
		t.Fatal("sandbox should be removed after final release")
	}
}

func TestSandboxRecover(t *testing.T) {
	ctr, _ := testCtrClient(t)
	mgr := NewSandboxManager(ctr, hclog.NewNullLogger())

	mgr.Recover("alloc-r", "kata-alloc-r-sandbox")

	mgr.mu.Lock()
	sb := mgr.sandboxes["alloc-r"]
	mgr.mu.Unlock()
	if sb == nil {
		t.Fatal("expected sandbox after Recover")
	}
	if sb.ID != "kata-alloc-r-sandbox" {
		t.Fatalf("sandbox ID = %q, want %q", sb.ID, "kata-alloc-r-sandbox")
	}
	if sb.refCount.Load() != 1 {
		t.Fatalf("refCount = %d, want 1", sb.refCount.Load())
	}

	mgr.Recover("alloc-r", "kata-alloc-r-sandbox")
	if sb.refCount.Load() != 2 {
		t.Fatalf("refCount after second Recover = %d, want 2", sb.refCount.Load())
	}
}
