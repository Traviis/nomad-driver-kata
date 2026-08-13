package kata

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestSandboxProcessIDsSelectsOnlyExactSandboxRuntimeProcesses(t *testing.T) {
	procRoot := t.TempDir()
	sandboxID := "kata-alloc-1-sandbox"

	writeProcessFixture(t, procRoot, "101", "/nix/store/qemu/bin/.qemu-system-x86_64-wrapped", []string{"qemu-system-x86_64", "-name", "sandbox-" + sandboxID})
	writeProcessFixture(t, procRoot, "102", "/nix/store/virtiofsd/bin/virtiofsd", []string{"virtiofsd", "--shared-dir=/run/" + sandboxID + "/shared"})
	writeProcessFixture(t, procRoot, "103", "/nix/store/kata/bin/containerd-shim-kata-v2", []string{"containerd-shim-kata-v2", "-namespace", "default", "-id", sandboxID})
	writeProcessFixture(t, procRoot, "104", "/bin/bash", []string{"bash", "cleanup", sandboxID, "qemu-system-x86_64"})
	writeProcessFixture(t, procRoot, "105", "/nix/store/qemu/bin/qemu-system-x86_64", []string{"qemu-system-x86_64", "-name", "sandbox-kata-other-sandbox"})
	writeProcessFixture(t, procRoot, "106", "/nix/store/kata/bin/containerd-shim-kata-v2", []string{"containerd-shim-kata-v2", "-id", sandboxID + "-extra"})
	writeProcessFixture(t, procRoot, "107", "/nix/store/kata/bin/containerd-shim-kata-v2", []string{"containerd-shim-kata-v2", "--log=/tmp/" + sandboxID, "-id", "kata-other-sandbox"})
	writeProcessFixture(t, procRoot, "108", "/nix/store/containerd/bin/containerd-shim-runc-v2", []string{"containerd-shim-runc-v2", "-id", sandboxID})
	writeProcessFixture(t, procRoot, "109", "/nix/store/kata/bin/containerd-shim-kata-v2", []string{"containerd-shim-kata-v2", "-id"})

	got, err := sandboxProcessIDs(procRoot, sandboxID)
	if err != nil {
		t.Fatalf("sandboxProcessIDs: %v", err)
	}
	slices.Sort(got)
	want := []int{101, 102, 103}
	if !slices.Equal(got, want) {
		t.Fatalf("process IDs = %v, want %v", got, want)
	}
}

func TestCleanupSandboxProcessesUsesPidfdsAndKillsOnlySurvivors(t *testing.T) {
	procRoot := t.TempDir()
	sandboxID := "kata-alloc-1-sandbox"
	writeProcessFixture(t, procRoot, "101", "/nix/store/qemu/bin/qemu-system-x86_64", []string{"qemu-system-x86_64", "-name", "sandbox-" + sandboxID})
	writeProcessFixture(t, procRoot, "102", "/nix/store/kata/bin/containerd-shim-kata-v2", []string{"containerd-shim-kata-v2", "-id", sandboxID})
	writeProcessFixture(t, procRoot, "103", "/nix/store/kata/bin/containerd-shim-kata-v2", []string{"containerd-shim-kata-v2", "-id", "kata-other-sandbox"})

	qemu := &fakeSandboxProcessHandle{exits: true}
	shim := &fakeSandboxProcessHandle{}
	manager := &fakeSandboxProcessManager{handles: map[int]*fakeSandboxProcessHandle{101: qemu, 102: shim}}

	if err := cleanupSandboxProcessesWithManager(procRoot, sandboxID, manager); err != nil {
		t.Fatalf("cleanupSandboxProcessesWithManager: %v", err)
	}
	if !reflect.DeepEqual(manager.opened, []int{101, 102}) {
		t.Fatalf("opened PIDs = %v, want [101 102]", manager.opened)
	}
	if !reflect.DeepEqual(qemu.signals, []syscall.Signal{syscall.SIGTERM}) {
		t.Fatalf("QEMU signals = %v, want [SIGTERM]", qemu.signals)
	}
	if !reflect.DeepEqual(shim.signals, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
		t.Fatalf("shim signals = %v, want [SIGTERM SIGKILL]", shim.signals)
	}
	if !qemu.closed || !shim.closed {
		t.Fatal("process handles were not closed")
	}
}

type fakeSandboxProcessManager struct {
	handles map[int]*fakeSandboxProcessHandle
	opened  []int
}

func (m *fakeSandboxProcessManager) Open(processID int) (sandboxProcessHandle, error) {
	m.opened = append(m.opened, processID)
	return m.handles[processID], nil
}

type fakeSandboxProcessHandle struct {
	exits   bool
	signals []syscall.Signal
	closed  bool
}

func (h *fakeSandboxProcessHandle) Signal(signal syscall.Signal) error {
	h.signals = append(h.signals, signal)
	return nil
}

func (h *fakeSandboxProcessHandle) Exited(time.Duration) (bool, error) {
	return h.exits, nil
}

func (h *fakeSandboxProcessHandle) Close() error {
	h.closed = true
	return nil
}

func writeProcessFixture(t *testing.T, procRoot, pid, executable string, args []string) {
	t.Helper()
	dir := filepath.Join(procRoot, pid)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(executable, filepath.Join(dir, "exe")); err != nil {
		t.Fatalf("symlink executable: %v", err)
	}
	var cmdline []byte
	for _, arg := range args {
		cmdline = append(cmdline, arg...)
		cmdline = append(cmdline, 0)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), cmdline, 0600); err != nil {
		t.Fatalf("write cmdline: %v", err)
	}
}
