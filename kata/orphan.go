package kata

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type sandboxProcessHandle interface {
	Signal(syscall.Signal) error
	Exited(time.Duration) (bool, error)
	Close() error
}

type sandboxProcessManager interface {
	Open(int) (sandboxProcessHandle, error)
}

type pidfdProcessManager struct{}

type pidfdProcessHandle struct {
	fd int
}

func (pidfdProcessManager) Open(processID int) (sandboxProcessHandle, error) {
	fd, err := unix.PidfdOpen(processID, 0)
	if err != nil {
		return nil, err
	}
	return &pidfdProcessHandle{fd: fd}, nil
}

func (h *pidfdProcessHandle) Signal(signal syscall.Signal) error {
	return unix.PidfdSendSignal(h.fd, unix.Signal(signal), nil, 0)
}

func (h *pidfdProcessHandle) Exited(timeout time.Duration) (bool, error) {
	milliseconds := int(timeout.Milliseconds())
	if timeout > 0 && milliseconds == 0 {
		milliseconds = 1
	}
	fds := []unix.PollFd{{Fd: int32(h.fd), Events: unix.POLLIN}}
	_, err := unix.Poll(fds, milliseconds)
	if err != nil {
		return false, err
	}
	return fds[0].Revents&unix.POLLIN != 0, nil
}

func (h *pidfdProcessHandle) Close() error {
	return unix.Close(h.fd)
}

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
		commandLine, err := os.ReadFile(filepath.Join(processDir, "cmdline"))
		if err != nil {
			continue
		}
		if sandboxProcessMatches(filepath.Base(executable), commandLine, sandboxID) {
			processIDs = append(processIDs, processID)
		}
	}
	return processIDs, nil
}

func sandboxProcessMatches(executable string, commandLine []byte, sandboxID string) bool {
	arguments := strings.Split(strings.TrimSuffix(string(commandLine), "\x00"), "\x00")
	switch {
	case strings.Contains(executable, "qemu-system-x86_64"):
		return hasArgumentValue(arguments, "-name", "sandbox-"+sandboxID)
	case executable == "virtiofsd":
		return bytes.Contains(commandLine, []byte("/"+sandboxID+"/"))
	case executable == "containerd-shim-kata-v2":
		return hasArgumentValue(arguments, "-id", sandboxID)
	default:
		return false
	}
}

func hasArgumentValue(arguments []string, option, value string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == option && arguments[index+1] == value {
			return true
		}
	}
	return false
}

func cleanupSandboxProcesses(procRoot, sandboxID string) error {
	if err := cleanupSandboxProcessesWithManager(procRoot, sandboxID, pidfdProcessManager{}); err != nil {
		return fmt.Errorf("cleaning sandbox processes: %w", err)
	}
	return nil
}

func cleanupSandboxProcessesWithManager(procRoot, sandboxID string, manager sandboxProcessManager) error {
	processIDs, err := sandboxProcessIDs(procRoot, sandboxID)
	if err != nil {
		return fmt.Errorf("finding sandbox processes: %w", err)
	}

	var handles []sandboxProcessHandle
	for _, processID := range processIDs {
		handle, err := manager.Open(processID)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return fmt.Errorf("opening sandbox process %d: %w", processID, err)
		}
		handles = append(handles, handle)
	}
	defer func() {
		for _, handle := range handles {
			_ = handle.Close()
		}
	}()

	for _, handle := range handles {
		if err := handle.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("terminating sandbox process: %w", err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for _, handle := range handles {
		remaining := max(time.Until(deadline), 0)
		exited, err := handle.Exited(remaining)
		if err != nil {
			return fmt.Errorf("waiting for sandbox process: %w", err)
		}
		if !exited {
			if err := handle.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
				return fmt.Errorf("killing sandbox process: %w", err)
			}
		}
	}
	return nil
}
