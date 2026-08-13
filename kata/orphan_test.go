package kata

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSandboxProcessIDsSelectsOnlyExactSandboxRuntimeProcesses(t *testing.T) {
	procRoot := t.TempDir()
	sandboxID := "kata-alloc-1-sandbox"

	writeProcessFixture(t, procRoot, "101", "/nix/store/qemu/bin/.qemu-system-x86_64-wrapped", []string{"qemu-system-x86_64", "-name", "sandbox-" + sandboxID})
	writeProcessFixture(t, procRoot, "102", "/nix/store/virtiofsd/bin/virtiofsd", []string{"virtiofsd", "--shared-dir=/run/" + sandboxID + "/shared"})
	writeProcessFixture(t, procRoot, "103", "/bin/bash", []string{"bash", "cleanup", sandboxID, "qemu-system-x86_64"})
	writeProcessFixture(t, procRoot, "104", "/nix/store/qemu/bin/qemu-system-x86_64", []string{"qemu-system-x86_64", "-name", "sandbox-kata-other-sandbox"})

	got, err := sandboxProcessIDs(procRoot, sandboxID)
	if err != nil {
		t.Fatalf("sandboxProcessIDs: %v", err)
	}
	slices.Sort(got)
	want := []int{101, 102}
	if !slices.Equal(got, want) {
		t.Fatalf("process IDs = %v, want %v", got, want)
	}
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
