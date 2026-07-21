package kata

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"
)

func newTestHandle(t *testing.T, rec *recorder) *taskHandle {
	t.Helper()
	return &taskHandle{
		containerID: "test-container-1",
		sandboxID:   "sandbox-1",
		allocID:     "alloc-1",
		taskName:    "web",
		ctr:         rec,
		logger:      hclog.NewNullLogger(),
		startedAt:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		doneCh:      make(chan struct{}),
	}
}

func TestSetExitCodeOnly(t *testing.T) {
	rec := newRecorder()
	h := newTestHandle(t, rec)

	h.setExit(42, nil)

	result := h.ExitResult()
	if result == nil {
		t.Fatal("expected non-nil ExitResult")
	}
	if result.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", result.ExitCode)
	}
	if result.Err != nil {
		t.Errorf("expected no error, got %v", result.Err)
	}
}

func TestSetExitWithError(t *testing.T) {
	rec := newRecorder()
	h := newTestHandle(t, rec)

	h.setExit(137, &os.PathError{Op: "kill", Path: "/proc/1", Err: nil})

	result := h.ExitResult()
	if result == nil {
		t.Fatal("expected non-nil ExitResult")
	}
	if result.ExitCode != 137 {
		t.Errorf("ExitCode = %d, want 137", result.ExitCode)
	}
	if result.Err == nil {
		t.Error("expected error in ExitResult")
	}
}

func TestIsRunning(t *testing.T) {
	rec := newRecorder()
	h := newTestHandle(t, rec)

	if !h.IsRunning() {
		t.Error("expected IsRunning = true before exit")
	}

	h.setExit(0, nil)

	if h.IsRunning() {
		t.Error("expected IsRunning = false after exit")
	}
}

func TestExitResultNilBeforeExit(t *testing.T) {
	rec := newRecorder()
	h := newTestHandle(t, rec)

	if h.ExitResult() != nil {
		t.Error("expected nil ExitResult before setExit")
	}
}

func TestTaskStatusRunning(t *testing.T) {
	rec := newRecorder()
	h := newTestHandle(t, rec)

	status := h.TaskStatus()

	if status.State != drivers.TaskStateRunning {
		t.Errorf("State = %q, want %q", status.State, drivers.TaskStateRunning)
	}
	if status.ID != "test-container-1" {
		t.Errorf("ID = %q, want %q", status.ID, "test-container-1")
	}
	if status.Name != "web" {
		t.Errorf("Name = %q, want %q", status.Name, "web")
	}
	if !status.StartedAt.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("StartedAt does not match expected value")
	}
}

func TestTaskStatusExited(t *testing.T) {
	rec := newRecorder()
	h := newTestHandle(t, rec)

	h.setExit(0, nil)

	status := h.TaskStatus()

	if status.State != drivers.TaskStateExited {
		t.Errorf("State = %q, want %q", status.State, drivers.TaskStateExited)
	}
	if status.ExitResult == nil {
		t.Error("expected non-nil ExitResult in TaskStatus after exit")
	}
	if status.CompletedAt.IsZero() {
		t.Error("CompletedAt should be set after exit")
	}
}

func TestOpenLogsCreatesFiles(t *testing.T) {
	rec := newRecorder()
	h := newTestHandle(t, rec)

	dir := t.TempDir()
	stdoutPath := filepath.Join(dir, "stdout.log")
	stderrPath := filepath.Join(dir, "stderr.log")

	stdout, stderr, err := h.openLogs(stdoutPath, stderrPath)
	if err != nil {
		t.Fatalf("openLogs: %v", err)
	}
	if stdout == nil {
		t.Error("expected non-nil stdout file")
	}
	if stderr == nil {
		t.Error("expected non-nil stderr file")
	}

	stdout.Close()
	stderr.Close()

	if _, err := os.Stat(stdoutPath); os.IsNotExist(err) {
		t.Error("stdout log file was not created")
	}
	if _, err := os.Stat(stderrPath); os.IsNotExist(err) {
		t.Error("stderr log file was not created")
	}
}

func TestOpenLogsSinglePath(t *testing.T) {
	rec := newRecorder()
	h := newTestHandle(t, rec)

	dir := t.TempDir()
	stdoutPath := filepath.Join(dir, "stdout.log")

	stdout, stderr, err := h.openLogs(stdoutPath, "")
	if err != nil {
		t.Fatalf("openLogs: %v", err)
	}
	if stdout == nil {
		t.Error("expected non-nil stdout file")
	}
	if stderr != nil {
		t.Error("expected nil stderr when path is empty")
	}

	stdout.Close()
}

func TestOpenLogsBothEmpty(t *testing.T) {
	rec := newRecorder()
	h := newTestHandle(t, rec)

	stdout, stderr, err := h.openLogs("", "")
	if err != nil {
		t.Fatalf("openLogs: %v", err)
	}
	if stdout != nil || stderr != nil {
		t.Error("expected both nil when paths are empty")
	}
}

func TestOpenLogsStdoutPathError(t *testing.T) {
	rec := newRecorder()
	h := newTestHandle(t, rec)

	// Non-writable directory — os.OpenFile should fail
	stdout, stderr, err := h.openLogs("/proc/nonexistent/stdout.log", "")
	if err == nil {
		t.Fatal("expected error for non-writable path")
	}
	if stdout != nil {
		t.Error("expected nil stdout on error")
	}
	if stderr != nil {
		t.Error("expected nil stderr on error")
	}
}

func TestOpenLogsStderrPathErrorCleansStdout(t *testing.T) {
	rec := newRecorder()
	h := newTestHandle(t, rec)

	dir := t.TempDir()
	stdoutPath := filepath.Join(dir, "stdout.log")
	stderrPath := "/proc/nonexistent/stderr.log"

	stdout, stderr, err := h.openLogs(stdoutPath, stderrPath)
	if err == nil {
		t.Fatal("expected error for stderr path")
	}
	if stdout != nil {
		t.Error("expected stdout to be closed on stderr open failure")
	}
	if stderr != nil {
		t.Error("expected nil stderr on error")
	}

	// stdout file should exist but be closed (no panic on stat)
	if _, err := os.Stat(stdoutPath); os.IsNotExist(err) {
		t.Error("stdout log file was not created")
	}
}

func TestRunExitsWithRecorderCode(t *testing.T) {
	rec := newRecorder()
	rec.runExit = 42

	h := newTestHandle(t, rec)

	dir := t.TempDir()
	stdoutPath := filepath.Join(dir, "stdout.log")
	stderrPath := filepath.Join(dir, "stderr.log")

	done := make(chan struct{})
	go func() {
		h.run(stdoutPath, stderrPath)
		close(done)
	}()

	select {
	case <-done:
		// run completed
	case <-time.After(5 * time.Second):
		t.Fatal("run did not complete within timeout")
	}

	result := h.ExitResult()
	if result == nil {
		t.Fatal("expected non-nil ExitResult after run completes")
	}
	if result.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", result.ExitCode)
	}
}

func TestRunExitsOnError(t *testing.T) {
	rec := newRecorder()
	rec.runExit = 1
	rec.runErr = &os.PathError{Op: "write", Path: "/dev/null", Err: nil}

	h := newTestHandle(t, rec)

	dir := t.TempDir()
	stdoutPath := filepath.Join(dir, "stdout.log")
	stderrPath := filepath.Join(dir, "stderr.log")

	done := make(chan struct{})
	go func() {
		h.run(stdoutPath, stderrPath)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not complete within timeout")
	}

	result := h.ExitResult()
	if result == nil {
		t.Fatal("expected non-nil ExitResult after run completes")
	}
	if result.Err == nil {
		t.Error("expected error in ExitResult when RunTask returns error")
	}
}

func TestRunLogOpenFailureExitsWithCode1(t *testing.T) {
	rec := newRecorder()

	h := newTestHandle(t, rec)

	done := make(chan struct{})
	go func() {
		h.run("/proc/nonexistent/stdout.log", "/proc/nonexistent/stderr.log")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not complete within timeout")
	}

	result := h.ExitResult()
	if result == nil {
		t.Fatal("expected non-nil ExitResult after run completes")
	}
	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1 (log open failure)", result.ExitCode)
	}
}

func TestMonitorRecoveredExitsWithRecorder(t *testing.T) {
	rec := newRecorder()
	rec.runExit = 0

	h := newTestHandle(t, rec)

	dir := t.TempDir()
	stdoutPath := filepath.Join(dir, "stdout.log")
	stderrPath := filepath.Join(dir, "stderr.log")

	done := make(chan struct{})
	go func() {
		h.monitorRecovered(stdoutPath, stderrPath)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("monitorRecovered did not complete within timeout")
	}

	result := h.ExitResult()
	if result == nil {
		t.Fatal("expected non-nil ExitResult after monitorRecovered completes")
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

func TestMonitorRecoveredLogOpenFailure(t *testing.T) {
	rec := newRecorder()

	h := newTestHandle(t, rec)

	done := make(chan struct{})
	go func() {
		h.monitorRecovered("/proc/nonexistent/stdout.log", "/proc/nonexistent/stderr.log")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("monitorRecovered did not complete within timeout")
	}

	result := h.ExitResult()
	if result == nil {
		t.Fatal("expected non-nil ExitResult after monitorRecovered completes")
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (monitorRecovered exits 0 on log failure)", result.ExitCode)
	}
}

func TestRunBlocksUntilRecorderUnblocks(t *testing.T) {
	rec := newRecorder()
	ch := make(chan struct{})
	rec.runCh = ch

	h := newTestHandle(t, rec)

	dir := t.TempDir()
	stdoutPath := filepath.Join(dir, "stdout.log")
	stderrPath := filepath.Join(dir, "stderr.log")

	done := make(chan struct{})
	go func() {
		h.run(stdoutPath, stderrPath)
		close(done)
	}()

	// Give run time to block on the channel
	time.Sleep(100 * time.Millisecond)

	select {
	case <-done:
		t.Fatal("run should have blocked on rec.runCh")
	default:
		// expected — still blocked
	}

	close(rec.runCh) // unblock RunTask

	select {
	case <-done:
		// run completed after unblocking
	case <-time.After(5 * time.Second):
		t.Fatal("run did not complete after unblocking")
	}
}
