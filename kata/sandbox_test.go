package kata

import (
	"context"
	"testing"

	"github.com/hashicorp/go-hclog"
)

func TestSandboxID(t *testing.T) {
	got := sandboxID("abc-123")
	if got != "kata-abc-123-sandbox" {
		t.Fatalf("sandboxID(%q) = %q, want %q", "abc-123", got, "kata-abc-123-sandbox")
	}
}

func TestSandboxGetOrCreate(t *testing.T) {
	rec := newRecorder()
	mgr := NewSandboxManager(rec, hclog.NewNullLogger())
	ctx := context.Background()

	sb, err := mgr.GetOrCreate(ctx, "alloc-1", "io.containerd.kata.v2", "", "")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if sb.ID != "kata-alloc-1-sandbox" {
		t.Fatalf("sandbox ID = %q, want %q", sb.ID, "kata-alloc-1-sandbox")
	}
	if sb.refCount.Load() != 1 {
		t.Fatalf("refCount = %d, want 1", sb.refCount.Load())
	}
	if !rec.called("CreateSandbox") {
		t.Error("expected CreateSandbox call")
	}
	if !rec.called("StartSandbox") {
		t.Error("expected StartSandbox call")
	}
}

func TestSandboxHostname(t *testing.T) {
	rec := newRecorder()
	mgr := NewSandboxManager(rec, hclog.NewNullLogger())
	ctx := context.Background()

	_, err := mgr.GetOrCreate(ctx, "alloc-1", "io.containerd.kata.v2", "", "my-group")
	if err != nil {
		t.Fatal(err)
	}

	cfg := rec.lastSandboxConfig()
	if cfg == nil {
		t.Fatal("no sandbox config recorded")
	}
	if cfg.Hostname != "my-group" {
		t.Errorf("Hostname = %q, want %q", cfg.Hostname, "my-group")
	}
}

func TestSandboxNetNS(t *testing.T) {
	rec := newRecorder()
	mgr := NewSandboxManager(rec, hclog.NewNullLogger())
	ctx := context.Background()

	_, err := mgr.GetOrCreate(ctx, "alloc-1", "io.containerd.kata.v2", "/var/run/netns/test", "")
	if err != nil {
		t.Fatal(err)
	}

	cfg := rec.lastSandboxConfig()
	if cfg == nil {
		t.Fatal("no sandbox config recorded")
	}
	if cfg.NetNS != "/var/run/netns/test" {
		t.Errorf("NetNS = %q, want %q", cfg.NetNS, "/var/run/netns/test")
	}
}

func TestSandboxUsesContainerdSandboxAPI(t *testing.T) {
	rec := newRecorder()
	mgr := NewSandboxManager(rec, hclog.NewNullLogger())
	ctx := context.Background()

	_, err := mgr.GetOrCreate(ctx, "alloc-1", "io.containerd.kata.v2", "", "")
	if err != nil {
		t.Fatal(err)
	}

	if rec.called("CreateContainer") {
		t.Fatal("sandbox must use containerd sandbox API, not a pause container")
	}
	cfg := rec.lastSandboxConfig()
	if cfg == nil {
		t.Fatal("no sandbox config recorded")
	}
	if cfg.ID != "kata-alloc-1-sandbox" {
		t.Errorf("sandbox ID = %q, want %q", cfg.ID, "kata-alloc-1-sandbox")
	}
	if cfg.Runtime != "io.containerd.kata.v2" {
		t.Errorf("runtime = %q, want %q", cfg.Runtime, "io.containerd.kata.v2")
	}
}

func TestSandboxReuse(t *testing.T) {
	rec := newRecorder()
	mgr := NewSandboxManager(rec, hclog.NewNullLogger())
	ctx := context.Background()

	sb1, _ := mgr.GetOrCreate(ctx, "alloc-1", "io.containerd.kata.v2", "", "")
	sb2, _ := mgr.GetOrCreate(ctx, "alloc-1", "io.containerd.kata.v2", "", "")

	if sb1.ID != sb2.ID {
		t.Fatalf("expected same sandbox, got %q and %q", sb1.ID, sb2.ID)
	}
	if sb1.refCount.Load() != 2 {
		t.Fatalf("refCount = %d, want 2", sb1.refCount.Load())
	}
	if rec.callCount("CreateSandbox") != 1 {
		t.Errorf("CreateSandbox called %d times, want 1", rec.callCount("CreateSandbox"))
	}
}

func TestSandboxRelease(t *testing.T) {
	rec := newRecorder()
	mgr := NewSandboxManager(rec, hclog.NewNullLogger())
	ctx := context.Background()

	mgr.GetOrCreate(ctx, "alloc-1", "io.containerd.kata.v2", "", "")
	mgr.GetOrCreate(ctx, "alloc-1", "io.containerd.kata.v2", "", "")

	mgr.Release(ctx, "alloc-1")
	if rec.called("DeleteSandbox") {
		t.Error("DeleteSandbox should not be called while refs remain")
	}

	mgr.Release(ctx, "alloc-1")
	if !rec.called("DeleteSandbox") {
		t.Error("expected DeleteSandbox call when last ref released")
	}
}

func TestSandboxRecover(t *testing.T) {
	rec := newRecorder()
	mgr := NewSandboxManager(rec, hclog.NewNullLogger())

	mgr.Recover("alloc-1", "kata-alloc-1-sandbox")

	mgr.mu.Lock()
	sb, ok := mgr.sandboxes["alloc-1"]
	mgr.mu.Unlock()

	if !ok {
		t.Fatal("sandbox not found after Recover")
	}
	if sb.ID != "kata-alloc-1-sandbox" {
		t.Errorf("sandbox ID = %q, want %q", sb.ID, "kata-alloc-1-sandbox")
	}
	if sb.refCount.Load() != 1 {
		t.Errorf("refCount = %d, want 1", sb.refCount.Load())
	}

	mgr.Recover("alloc-1", "kata-alloc-1-sandbox")
	if sb.refCount.Load() != 2 {
		t.Errorf("refCount = %d, want 2", sb.refCount.Load())
	}
}
