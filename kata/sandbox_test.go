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
	if !rec.called("EnsureImage") {
		t.Error("expected EnsureImage call")
	}
	if !rec.called("CreateContainer") {
		t.Error("expected CreateContainer call")
	}
	if !rec.called("StartTaskDetached") {
		t.Error("expected StartTaskDetached call")
	}
	if !rec.called("CreateSandboxMetadata") {
		t.Error("expected CreateSandboxMetadata call")
	}
}

func TestSandboxHostname(t *testing.T) {
	rec := newRecorder()
	mgr := NewSandboxManager(rec, hclog.NewNullLogger())
	ctx := context.Background()

	_, err := mgr.GetOrCreate(ctx, "alloc-1", "pause:3.9", "io.containerd.kata.v2", "", "my-group")
	if err != nil {
		t.Fatal(err)
	}

	cfg := rec.lastConfig()
	if cfg == nil {
		t.Fatal("no container config recorded")
	}
	if cfg.Hostname != "my-group" {
		t.Errorf("Hostname = %q, want %q", cfg.Hostname, "my-group")
	}
}

func TestSandboxNetNS(t *testing.T) {
	rec := newRecorder()
	mgr := NewSandboxManager(rec, hclog.NewNullLogger())
	ctx := context.Background()

	_, err := mgr.GetOrCreate(ctx, "alloc-1", "pause:3.9", "io.containerd.kata.v2", "/var/run/netns/test", "")
	if err != nil {
		t.Fatal(err)
	}

	cfg := rec.lastConfig()
	if cfg == nil {
		t.Fatal("no container config recorded")
	}
	if cfg.NetNS != "/var/run/netns/test" {
		t.Errorf("NetNS = %q, want %q", cfg.NetNS, "/var/run/netns/test")
	}
}

func TestSandboxCreatesContainerdSandboxMetadata(t *testing.T) {
	rec := newRecorder()
	mgr := NewSandboxManager(rec, hclog.NewNullLogger())
	ctx := context.Background()

	_, err := mgr.GetOrCreate(ctx, "alloc-1", "pause:3.9", "io.containerd.kata.v2", "", "")
	if err != nil {
		t.Fatal(err)
	}

	if !rec.called("CreateSandboxMetadata") {
		t.Fatal("expected sandbox metadata to be registered")
	}
	cfg := rec.configForID("kata-alloc-1-sandbox")
	if cfg == nil {
		t.Fatal("no sandbox container config recorded")
	}
	if cfg.Image != "pause:3.9" {
		t.Errorf("sandbox image = %q, want %q", cfg.Image, "pause:3.9")
	}
	if cfg.Runtime != "io.containerd.kata.v2" {
		t.Errorf("runtime = %q, want %q", cfg.Runtime, "io.containerd.kata.v2")
	}
}

func TestSandboxReuse(t *testing.T) {
	rec := newRecorder()
	mgr := NewSandboxManager(rec, hclog.NewNullLogger())
	ctx := context.Background()

	sb1, _ := mgr.GetOrCreate(ctx, "alloc-1", "pause:3.9", "io.containerd.kata.v2", "", "")
	sb2, _ := mgr.GetOrCreate(ctx, "alloc-1", "pause:3.9", "io.containerd.kata.v2", "", "")

	if sb1.ID != sb2.ID {
		t.Fatalf("expected same sandbox, got %q and %q", sb1.ID, sb2.ID)
	}
	if sb1.refCount.Load() != 2 {
		t.Fatalf("refCount = %d, want 2", sb1.refCount.Load())
	}
	if rec.callCount("CreateContainer") != 1 {
		t.Errorf("CreateContainer called %d times, want 1", rec.callCount("CreateContainer"))
	}
	if rec.callCount("CreateSandboxMetadata") != 1 {
		t.Errorf("CreateSandboxMetadata called %d times, want 1", rec.callCount("CreateSandboxMetadata"))
	}
}

func TestSandboxRelease(t *testing.T) {
	rec := newRecorder()
	mgr := NewSandboxManager(rec, hclog.NewNullLogger())
	ctx := context.Background()

	mgr.GetOrCreate(ctx, "alloc-1", "pause:3.9", "io.containerd.kata.v2", "", "")
	mgr.GetOrCreate(ctx, "alloc-1", "pause:3.9", "io.containerd.kata.v2", "", "")

	mgr.Release(ctx, "alloc-1")
	if rec.called("Cleanup") || rec.called("DeleteSandboxMetadata") {
		t.Error("sandbox should not be cleaned up while refs remain")
	}

	mgr.Release(ctx, "alloc-1")
	if !rec.called("Cleanup") {
		t.Error("expected Cleanup call when last ref released")
	}
	if !rec.called("DeleteSandboxMetadata") {
		t.Error("expected DeleteSandboxMetadata call when last ref released")
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
