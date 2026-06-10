package kata

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"
)

type taskHandle struct {
	containerID string
	sandboxID   string
	allocID     string
	taskName    string

	ctr    *CtrClient
	cmd    *exec.Cmd
	logger hclog.Logger

	startedAt   time.Time
	completedAt time.Time
	exitResult  *drivers.ExitResult

	doneCh chan struct{}
	mu     sync.RWMutex
}

// run starts the containerd task in detached mode, then attaches for log
// streaming. Blocks until the task exits.
func (h *taskHandle) run(stdoutPath, stderrPath string) {
	defer close(h.doneCh)

	ctx := context.Background()
	if err := h.ctr.StartTaskDetached(ctx, h.containerID); err != nil {
		h.logger.Error("failed to start task", "error", err)
		h.setExit(1, err)
		return
	}

	h.streamAndWait(stdoutPath, stderrPath)
}

// monitorRecovered re-attaches to a running task for log streaming after
// driver restart. Blocks until the task exits.
func (h *taskHandle) monitorRecovered(stdoutPath, stderrPath string) {
	defer close(h.doneCh)
	h.streamAndWait(stdoutPath, stderrPath)
}

// streamAndWait attaches to the task's stdio and waits for it to exit.
// Falls back to polling if attach fails (e.g. task already exited).
func (h *taskHandle) streamAndWait(stdoutPath, stderrPath string) {
	stdout, stderr, err := h.openLogs(stdoutPath, stderrPath)
	if err != nil {
		h.logger.Error("failed to open log files", "error", err)
		h.waitForExit()
		return
	}
	if stdout != nil {
		defer stdout.Close()
	}
	if stderr != nil {
		defer stderr.Close()
	}

	cmd, err := h.ctr.AttachTask(h.containerID, stdout, stderr)
	if err != nil {
		h.logger.Warn("failed to attach to task, falling back to polling", "error", err)
		h.waitForExit()
		return
	}

	h.mu.Lock()
	h.cmd = cmd
	h.mu.Unlock()

	waitErr := cmd.Wait()

	exitCode := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	h.setExit(exitCode, waitErr)
}

// waitForExit polls containerd until the task is no longer running.
func (h *taskHandle) waitForExit() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if !h.ctr.TaskRunning(context.Background(), h.containerID) {
			h.setExit(0, nil)
			return
		}
	}
}

func (h *taskHandle) openLogs(stdoutPath, stderrPath string) (*os.File, *os.File, error) {
	var stdout, stderr *os.File
	var err error

	if stdoutPath != "" {
		stdout, err = os.OpenFile(stdoutPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			return nil, nil, err
		}
	}

	if stderrPath != "" {
		stderr, err = os.OpenFile(stderrPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			if stdout != nil {
				stdout.Close()
			}
			return nil, nil, err
		}
	}

	return stdout, stderr, nil
}

func (h *taskHandle) setExit(code int, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.completedAt = time.Now()
	h.exitResult = &drivers.ExitResult{ExitCode: code}
	if err != nil {
		h.exitResult.Err = err
	}
}

func (h *taskHandle) IsRunning() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.exitResult == nil
}

func (h *taskHandle) ExitResult() *drivers.ExitResult {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.exitResult
}

func (h *taskHandle) TaskStatus() *drivers.TaskStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()

	state := drivers.TaskStateRunning
	if h.exitResult != nil {
		state = drivers.TaskStateExited
	}

	return &drivers.TaskStatus{
		ID:          h.containerID,
		Name:        h.taskName,
		State:       state,
		StartedAt:   h.startedAt,
		CompletedAt: h.completedAt,
		ExitResult:  h.exitResult,
	}
}
