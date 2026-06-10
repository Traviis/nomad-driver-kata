package kata

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/hashicorp/go-hclog"
)

// ContainerConfig holds parameters for creating a containerd container.
type ContainerConfig struct {
	ID               string
	Image            string
	Runtime          string
	Annotations      map[string]string
	Command          []string
	NetNS            string
	Env              []string
	Mounts           []string
	User             string
	Hostname         string
	Cwd              string
	MemoryLimitBytes int64
	CPUQuota         int64
	CPUPeriod        int64
	Privileged       bool
	Ulimit           map[string]string
}

// CtrClient wraps the containerd ctr CLI binary.
type CtrClient struct {
	binary    string
	address   string
	namespace string
	logger    hclog.Logger
}

func NewCtrClient(binary, address, namespace string, logger hclog.Logger) *CtrClient {
	return &CtrClient{
		binary:    binary,
		address:   address,
		namespace: namespace,
		logger:    logger.Named("ctr"),
	}
}

func (c *CtrClient) baseArgs() []string {
	return []string{"-a", c.address, "-n", c.namespace}
}

func (c *CtrClient) run(ctx context.Context, args ...string) (string, error) {
	fullArgs := append(c.baseArgs(), args...)
	cmd := exec.CommandContext(ctx, c.binary, fullArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	c.logger.Debug("executing", "args", args)

	if err := cmd.Run(); err != nil {
		label := strings.Join(args[:min(len(args), 3)], " ")
		return "", fmt.Errorf("ctr %s: %w\nstderr: %s", label, err, strings.TrimSpace(stderr.String()))
	}

	return stdout.String(), nil
}

func (c *CtrClient) Version(ctx context.Context) (string, error) {
	return c.run(ctx, "version")
}

func (c *CtrClient) EnsureImage(ctx context.Context, ref string, forcePull bool, username, password string) error {
	if !forcePull {
		out, _ := c.run(ctx, "image", "ls", "-q")
		if strings.Contains(out, ref) {
			return nil
		}
	}
	c.logger.Info("pulling image", "ref", ref)
	args := []string{"image", "pull"}
	if username != "" {
		args = append(args, "--user", username+":"+password)
	}
	args = append(args, ref)
	_, err := c.run(ctx, args...)
	return err
}

func (c *CtrClient) CreateContainer(ctx context.Context, cfg *ContainerConfig) error {
	args := []string{"container", "create", "--runtime", cfg.Runtime}
	if cfg.NetNS != "" {
		args = append(args, "--with-ns", fmt.Sprintf("network:%s", cfg.NetNS))
	}
	for k, v := range cfg.Annotations {
		args = append(args, "--annotation", fmt.Sprintf("%s=%s", k, v))
	}
	for _, env := range cfg.Env {
		args = append(args, "--env", env)
	}
	for _, mount := range cfg.Mounts {
		args = append(args, "--mount", mount)
	}
	if cfg.User != "" {
		args = append(args, "--user", cfg.User)
	}
	if cfg.Hostname != "" {
		args = append(args, "--hostname", cfg.Hostname)
	}
	if cfg.Cwd != "" {
		args = append(args, "--cwd", cfg.Cwd)
	}
	if cfg.MemoryLimitBytes > 0 {
		args = append(args, "--memory-limit", fmt.Sprintf("%d", cfg.MemoryLimitBytes))
	}
	if cfg.CPUQuota > 0 && cfg.CPUPeriod > 0 {
		args = append(args, "--cpu-quota", fmt.Sprintf("%d", cfg.CPUQuota))
		args = append(args, "--cpu-period", fmt.Sprintf("%d", cfg.CPUPeriod))
	}
	if cfg.Privileged {
		args = append(args, "--privileged")
	}
	for name, value := range cfg.Ulimit {
		args = append(args, "--rlimit", fmt.Sprintf("RLIMIT_%s=%s", strings.ToUpper(name), value))
	}
	args = append(args, cfg.Image, cfg.ID)
	args = append(args, cfg.Command...)
	_, err := c.run(ctx, args...)
	return err
}

func (c *CtrClient) StartTaskDetached(ctx context.Context, containerID string) error {
	_, err := c.run(ctx, "task", "start", "-d", containerID)
	return err
}

// AttachTask spawns a ctr task attach process, piping the container's
// stdout/stderr to the provided files. Returns the exec.Cmd so the
// caller can wait for exit. The task must already be running (started detached).
func (c *CtrClient) AttachTask(containerID string, stdout, stderr *os.File) (*exec.Cmd, error) {
	args := append(c.baseArgs(), "task", "attach", containerID)
	cmd := exec.Command(c.binary, args...)
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("attaching to task %s: %w", containerID, err)
	}
	return cmd, nil
}

func (c *CtrClient) KillTask(ctx context.Context, containerID, signal string) error {
	_, err := c.run(ctx, "task", "kill", "--signal", signal, containerID)
	return err
}

func (c *CtrClient) DeleteTask(ctx context.Context, containerID string) error {
	_, err := c.run(ctx, "task", "delete", containerID)
	return err
}

func (c *CtrClient) DeleteContainer(ctx context.Context, containerID string) error {
	_, err := c.run(ctx, "container", "delete", containerID)
	return err
}

func (c *CtrClient) TaskRunning(ctx context.Context, containerID string) bool {
	out, err := c.run(ctx, "task", "ls")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == containerID && fields[2] == "RUNNING" {
			return true
		}
	}
	return false
}

func (c *CtrClient) Exec(ctx context.Context, containerID, execID string, cmd []string) (string, int, error) {
	args := append(c.baseArgs(), "task", "exec", "--exec-id", execID, containerID)
	args = append(args, cmd...)
	execCmd := exec.CommandContext(ctx, c.binary, args...)

	var stdout, stderr bytes.Buffer
	execCmd.Stdout = &stdout
	execCmd.Stderr = &stderr

	err := execCmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return stdout.String(), exitErr.ExitCode(), nil
		}
		return "", -1, fmt.Errorf("ctr exec: %w\nstderr: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), 0, nil
}

func (c *CtrClient) ExecStreaming(ctx context.Context, containerID, execID string, cmd []string, tty bool, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	args := append(c.baseArgs(), "task", "exec", "--exec-id", execID)
	if tty {
		args = append(args, "--tty")
	}
	args = append(args, containerID)
	args = append(args, cmd...)

	execCmd := exec.CommandContext(ctx, c.binary, args...)
	execCmd.Stdin = stdin
	execCmd.Stdout = stdout
	execCmd.Stderr = stderr

	err := execCmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, fmt.Errorf("ctr exec streaming: %w", err)
	}
	return 0, nil
}

// Cleanup removes a container's task and container, ignoring errors.
func (c *CtrClient) Cleanup(ctx context.Context, containerID string) {
	_ = c.KillTask(ctx, containerID, "SIGKILL")
	_ = c.DeleteTask(ctx, containerID)
	_ = c.DeleteContainer(ctx, containerID)
}
