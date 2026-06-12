package kata

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/go-hclog"
)

// Sandbox tracks a running Kata VM that hosts one or more containers.
type Sandbox struct {
	ID       string
	AllocID  string
	refCount atomic.Int32
}

// SandboxManager maintains the mapping from allocation ID to Kata VM sandbox.
type SandboxManager struct {
	mu        sync.Mutex
	sandboxes map[string]*Sandbox
	ctr       Containerd
	logger    hclog.Logger
}

func NewSandboxManager(ctr Containerd, logger hclog.Logger) *SandboxManager {
	return &SandboxManager{
		sandboxes: make(map[string]*Sandbox),
		ctr:       ctr,
		logger:    logger.Named("sandbox"),
	}
}

func sandboxID(allocID string) string {
	return fmt.Sprintf("kata-%s-sandbox", allocID)
}

// GetOrCreate returns an existing sandbox for the allocation or boots a new
// Kata VM. The caller must eventually call Release for each GetOrCreate.
func (sm *SandboxManager) GetOrCreate(ctx context.Context, allocID, runtime, netNS, hostname string) (*Sandbox, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sb, ok := sm.sandboxes[allocID]; ok {
		sb.refCount.Add(1)
		sm.logger.Info("reusing sandbox", "alloc_id", allocID, "sandbox_id", sb.ID, "refs", sb.refCount.Load())
		return sb, nil
	}

	id := sandboxID(allocID)
	sm.logger.Info("creating sandbox VM", "alloc_id", allocID, "sandbox_id", id)

	if err := sm.ctr.CreateSandbox(ctx, &SandboxConfig{
		ID:       id,
		Runtime:  runtime,
		NetNS:    netNS,
		Hostname: hostname,
	}); err != nil {
		return nil, fmt.Errorf("creating sandbox: %w", err)
	}

	if err := sm.ctr.StartSandbox(ctx, id); err != nil {
		sm.ctr.DeleteSandbox(ctx, id)
		return nil, fmt.Errorf("starting sandbox: %w", err)
	}

	sb := &Sandbox{ID: id, AllocID: allocID}
	sb.refCount.Store(1)
	sm.sandboxes[allocID] = sb

	sm.logger.Info("sandbox VM running", "alloc_id", allocID, "sandbox_id", id)
	return sb, nil
}

// Release decrements the sandbox reference count and tears down the VM
// when no more tasks are using it.
func (sm *SandboxManager) Release(ctx context.Context, allocID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sb, ok := sm.sandboxes[allocID]
	if !ok {
		return
	}

	remaining := sb.refCount.Add(-1)
	if remaining > 0 {
		sm.logger.Info("sandbox still in use", "alloc_id", allocID, "refs", remaining)
		return
	}

	sm.logger.Info("destroying sandbox VM", "alloc_id", allocID, "sandbox_id", sb.ID)
	sm.ctr.DeleteSandbox(ctx, sb.ID)
	delete(sm.sandboxes, allocID)
}

// Recover rebuilds sandbox state from a recovered task handle, without
// creating anything in containerd. Used after driver restart.
func (sm *SandboxManager) Recover(allocID, sbID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sb, ok := sm.sandboxes[allocID]; ok {
		sb.refCount.Add(1)
		return
	}

	sb := &Sandbox{ID: sbID, AllocID: allocID}
	sb.refCount.Store(1)
	sm.sandboxes[allocID] = sb
	sm.logger.Info("recovered sandbox", "alloc_id", allocID, "sandbox_id", sbID)
}
