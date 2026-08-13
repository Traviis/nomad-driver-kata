package kata

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func sandboxProcessIDs(procRoot, sandboxID string) ([]int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}

	var processIDs []int
	for _, entry := range entries {
		processID, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		processDir := filepath.Join(procRoot, entry.Name())
		executable, err := os.Readlink(filepath.Join(processDir, "exe"))
		if err != nil {
			continue
		}
		name := filepath.Base(executable)
		if !strings.Contains(name, "qemu-system-x86_64") && name != "virtiofsd" {
			continue
		}

		commandLine, err := os.ReadFile(filepath.Join(processDir, "cmdline"))
		if err != nil {
			continue
		}
		if bytes.Contains(commandLine, []byte(sandboxID)) {
			processIDs = append(processIDs, processID)
		}
	}
	return processIDs, nil
}

func cleanupSandboxProcesses(procRoot, sandboxID string) error {
	processIDs, err := sandboxProcessIDs(procRoot, sandboxID)
	if err != nil {
		return fmt.Errorf("finding sandbox processes: %w", err)
	}
	for _, processID := range processIDs {
		_ = syscall.Kill(processID, syscall.SIGTERM)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		remaining, err := sandboxProcessIDs(procRoot, sandboxID)
		if err != nil || len(remaining) == 0 {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}

	remaining, err := sandboxProcessIDs(procRoot, sandboxID)
	if err != nil {
		return err
	}
	for _, processID := range remaining {
		_ = syscall.Kill(processID, syscall.SIGKILL)
	}
	return nil
}
