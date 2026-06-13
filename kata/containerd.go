package kata

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	sandboxstore "github.com/containerd/containerd/v2/core/sandbox"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/hashicorp/go-hclog"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// Containerd abstracts containerd operations for the driver.
type Containerd interface {
	Close() error
	Version(ctx context.Context) (string, error)

	EnsureImage(ctx context.Context, ref string, forcePull bool, username, password string) error
	CreateSandboxMetadata(ctx context.Context, id, runtime string) error
	DeleteSandboxMetadata(ctx context.Context, id string) error
	CreateContainer(ctx context.Context, cfg *ContainerConfig) error
	DeleteContainer(ctx context.Context, id string) error

	StartTaskDetached(ctx context.Context, id string) error
	RunTask(ctx context.Context, id string, stdout, stderr *os.File) (int, error)
	MonitorTask(ctx context.Context, id string, stdout, stderr *os.File) (int, error)

	KillTask(ctx context.Context, id string, signal string) error
	DeleteTask(ctx context.Context, id string) error
	TaskRunning(ctx context.Context, id string) bool

	Exec(ctx context.Context, id, execID string, cmd []string) (string, int, error)
	ExecStreaming(ctx context.Context, id, execID string, cmd []string, tty bool, stdin io.Reader, stdout, stderr io.Writer) (int, error)

	Metrics(ctx context.Context, id string) (*containerMetrics, error)
	Cleanup(ctx context.Context, id string)
	GarbageCollect(ctx context.Context, delay time.Duration) (int, error)
}

// Mount describes a bind mount for a container.
type Mount struct {
	Source      string
	Destination string
	Type        string
	Options     []string
}

// ContainerConfig holds parameters for creating a containerd container.
type ContainerConfig struct {
	ID               string
	Image            string
	Runtime          string
	Annotations      map[string]string
	SandboxID        string
	Command          []string
	NetNS            string
	Env              []string
	Mounts           []Mount
	User             string
	Hostname         string
	Cwd              string
	MemoryLimitBytes int64
	CPUQuota         int64
	CPUPeriod        int64
	PidsLimit        int64
	Privileged       bool
	ReadonlyRootfs   bool
	CapAdd           []string
	CapDrop          []string
	Ulimit           map[string]string
	Labels           map[string]string
	Devices          []string
}

type containerdClient struct {
	client    *containerd.Client
	namespace string
	logger    hclog.Logger
}

func NewContainerdClient(address, namespace string, logger hclog.Logger) (Containerd, error) {
	c, err := containerd.New(address)
	if err != nil {
		return nil, fmt.Errorf("connecting to containerd at %s: %w", address, err)
	}
	return &containerdClient{
		client:    c,
		namespace: namespace,
		logger:    logger.Named("containerd"),
	}, nil
}

func (c *containerdClient) nsCtx(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, c.namespace)
}

func (c *containerdClient) Close() error {
	return c.client.Close()
}

func (c *containerdClient) Version(ctx context.Context) (string, error) {
	ctx = c.nsCtx(ctx)
	v, err := c.client.Version(ctx)
	if err != nil {
		return "", err
	}
	return v.Version, nil
}

func (c *containerdClient) EnsureImage(ctx context.Context, ref string, forcePull bool, username, password string) error {
	ctx = c.nsCtx(ctx)

	if !forcePull {
		_, err := c.client.GetImage(ctx, ref)
		if err == nil {
			c.logger.Debug("image already present", "ref", ref)
			return nil
		}
		c.logger.Warn("image not found locally, will pull", "ref", ref, "namespace", c.namespace, "error", err)
	}

	c.logger.Info("pulling image", "ref", ref)

	pullOpts := []containerd.RemoteOpt{
		containerd.WithPullUnpack,
	}

	if username != "" {
		pullOpts = append(pullOpts, containerd.WithResolver(docker.NewResolver(docker.ResolverOptions{
			Credentials: func(host string) (string, string, error) {
				return username, password, nil
			},
		})))
	} else {
		creds := dockerCredentialFunc()
		if creds != nil {
			pullOpts = append(pullOpts, containerd.WithResolver(docker.NewResolver(docker.ResolverOptions{
				Credentials: creds,
			})))
		}
	}

	_, err := c.client.Pull(ctx, ref, pullOpts...)
	return err
}

func (c *containerdClient) CreateSandboxMetadata(ctx context.Context, id, runtime string) error {
	ctx = c.nsCtx(ctx)
	now := time.Now().UTC()
	_, err := c.client.SandboxStore().Create(ctx, sandboxstore.Sandbox{
		ID:        id,
		Runtime:   sandboxstore.RuntimeOpts{Name: runtime},
		Sandboxer: "shim",
		CreatedAt: now,
		UpdatedAt: now,
	})
	if errdefs.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (c *containerdClient) DeleteSandboxMetadata(ctx context.Context, id string) error {
	ctx = c.nsCtx(ctx)
	err := c.client.SandboxStore().Delete(ctx, id)
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (c *containerdClient) CreateContainer(ctx context.Context, cfg *ContainerConfig) error {
	ctx = c.nsCtx(ctx)

	image, err := c.client.GetImage(ctx, cfg.Image)
	if err != nil {
		return fmt.Errorf("getting image %s: %w", cfg.Image, err)
	}

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(image),
	}

	if len(cfg.Command) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(cfg.Command...))
	}
	if cfg.Cwd != "" {
		specOpts = append(specOpts, oci.WithProcessCwd(cfg.Cwd))
	}
	if len(cfg.Env) > 0 {
		specOpts = append(specOpts, oci.WithEnv(cfg.Env))
	}
	if cfg.User != "" {
		specOpts = append(specOpts, oci.WithUser(cfg.User))
	}
	if cfg.Hostname != "" {
		specOpts = append(specOpts, oci.WithHostname(cfg.Hostname))
	}
	if cfg.MemoryLimitBytes > 0 {
		specOpts = append(specOpts, oci.WithMemoryLimit(uint64(cfg.MemoryLimitBytes)))
	}
	if cfg.Privileged {
		specOpts = append(specOpts, oci.WithPrivileged)
	}
	if cfg.ReadonlyRootfs {
		specOpts = append(specOpts, oci.WithRootFSReadonly())
	}
	if len(cfg.Annotations) > 0 {
		specOpts = append(specOpts, oci.WithAnnotations(cfg.Annotations))
	}
	if cfg.NetNS != "" {
		specOpts = append(specOpts, oci.WithLinuxNamespace(specs.LinuxNamespace{
			Type: specs.NetworkNamespace,
			Path: cfg.NetNS,
		}))
	}
	if len(cfg.Mounts) > 0 {
		var mounts []specs.Mount
		for _, m := range cfg.Mounts {
			mounts = append(mounts, specs.Mount{
				Source:      m.Source,
				Destination: m.Destination,
				Type:        m.Type,
				Options:     m.Options,
			})
		}
		specOpts = append(specOpts, oci.WithMounts(mounts))
	}
	if cfg.CPUQuota > 0 && cfg.CPUPeriod > 0 {
		specOpts = append(specOpts, oci.WithCPUCFS(cfg.CPUQuota, uint64(cfg.CPUPeriod)))
	}
	if cfg.PidsLimit > 0 {
		specOpts = append(specOpts, oci.WithPidsLimit(cfg.PidsLimit))
	}
	if len(cfg.CapAdd) > 0 {
		specOpts = append(specOpts, oci.WithAddedCapabilities(cfg.CapAdd))
	}
	if len(cfg.CapDrop) > 0 {
		specOpts = append(specOpts, oci.WithDroppedCapabilities(cfg.CapDrop))
	}
	for _, dev := range cfg.Devices {
		host, ctr, perms := parseDevice(dev)
		specOpts = append(specOpts, oci.WithDevices(host, ctr, perms))
	}
	for name, value := range cfg.Ulimit {
		parts := strings.SplitN(value, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("ulimit %s: expected soft:hard, got %q", name, value)
		}
		soft, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			return fmt.Errorf("ulimit %s soft value: %w", name, err)
		}
		hard, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			return fmt.Errorf("ulimit %s hard value: %w", name, err)
		}
		rlimit := specs.POSIXRlimit{
			Type: "RLIMIT_" + strings.ToUpper(name),
			Soft: soft,
			Hard: hard,
		}
		specOpts = append(specOpts, func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
			s.Process.Rlimits = append(s.Process.Rlimits, rlimit)
			return nil
		})
	}

	containerOpts := []containerd.NewContainerOpts{
		containerd.WithImage(image),
		containerd.WithNewSnapshot(cfg.ID, image),
		containerd.WithRuntime(cfg.Runtime, nil),
		containerd.WithNewSpec(specOpts...),
	}
	if cfg.SandboxID != "" {
		containerOpts = append(containerOpts, containerd.WithSandbox(cfg.SandboxID))
	}
	if len(cfg.Labels) > 0 {
		containerOpts = append(containerOpts, containerd.WithContainerLabels(cfg.Labels))
	}

	_, err = c.client.NewContainer(ctx, cfg.ID, containerOpts...)
	return err
}

func (c *containerdClient) DeleteContainer(ctx context.Context, id string) error {
	ctx = c.nsCtx(ctx)
	container, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	return container.Delete(ctx, containerd.WithSnapshotCleanup)
}

func (c *containerdClient) StartTaskDetached(ctx context.Context, id string) error {
	ctx = c.nsCtx(ctx)
	container, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		return fmt.Errorf("loading container %s: %w", id, err)
	}

	task, err := container.NewTask(ctx, cio.NullIO)
	if err != nil {
		return fmt.Errorf("creating task for %s: %w", id, err)
	}

	if err := task.Start(ctx); err != nil {
		task.Delete(ctx)
		return fmt.Errorf("starting task %s: %w", id, err)
	}
	return nil
}

func (c *containerdClient) RunTask(ctx context.Context, id string, stdout, stderr *os.File) (int, error) {
	ctx = c.nsCtx(ctx)
	container, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		return -1, fmt.Errorf("loading container %s: %w", id, err)
	}

	creator := cio.NullIO
	if stdout != nil || stderr != nil {
		creator = cio.NewCreator(cio.WithStreams(nil, stdout, stderr))
	}

	task, err := container.NewTask(ctx, creator)
	if err != nil {
		return -1, fmt.Errorf("creating task for %s: %w", id, err)
	}

	exitCh, err := task.Wait(ctx)
	if err != nil {
		task.Delete(ctx)
		return -1, fmt.Errorf("waiting for task %s: %w", id, err)
	}

	if err := task.Start(ctx); err != nil {
		task.Delete(ctx)
		return -1, fmt.Errorf("starting task %s: %w", id, err)
	}

	status := <-exitCh
	code, _, err := status.Result()
	return int(code), err
}

func (c *containerdClient) MonitorTask(ctx context.Context, id string, stdout, stderr *os.File) (int, error) {
	ctx = c.nsCtx(ctx)
	container, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		return -1, fmt.Errorf("loading container %s: %w", id, err)
	}

	attach := cio.NewAttach(cio.WithStreams(nil, stdout, stderr))
	task, err := container.Task(ctx, attach)
	if err != nil {
		return -1, fmt.Errorf("attaching to task %s: %w", id, err)
	}

	exitCh, err := task.Wait(ctx)
	if err != nil {
		return -1, fmt.Errorf("waiting for task %s: %w", id, err)
	}

	status := <-exitCh
	code, _, err := status.Result()
	return int(code), err
}

func (c *containerdClient) KillTask(ctx context.Context, id string, signal string) error {
	ctx = c.nsCtx(ctx)
	container, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		return err
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		return err
	}

	sig := parseSignal(signal)
	return task.Kill(ctx, sig)
}

func (c *containerdClient) DeleteTask(ctx context.Context, id string) error {
	ctx = c.nsCtx(ctx)
	container, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}

	_, err = task.Delete(ctx)
	return err
}

func (c *containerdClient) TaskRunning(ctx context.Context, id string) bool {
	ctx = c.nsCtx(ctx)
	container, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		return false
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		return false
	}

	status, err := task.Status(ctx)
	if err != nil {
		return false
	}
	return status.Status == containerd.Running
}

func (c *containerdClient) Exec(ctx context.Context, id, execID string, cmd []string) (string, int, error) {
	ctx = c.nsCtx(ctx)
	container, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		return "", -1, err
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		return "", -1, err
	}

	spec, err := container.Spec(ctx)
	if err != nil {
		return "", -1, err
	}

	pspec := *spec.Process
	pspec.Args = cmd
	pspec.Terminal = false

	var stdout bytes.Buffer
	process, err := task.Exec(ctx, execID, &pspec, cio.NewCreator(cio.WithStreams(nil, &stdout, &stdout)))
	if err != nil {
		return "", -1, fmt.Errorf("exec in %s: %w", id, err)
	}

	exitCh, err := process.Wait(ctx)
	if err != nil {
		process.Delete(ctx)
		return "", -1, err
	}

	if err := process.Start(ctx); err != nil {
		process.Delete(ctx)
		return "", -1, err
	}

	status := <-exitCh
	code, _, _ := status.Result()
	process.Delete(ctx)
	return stdout.String(), int(code), nil
}

func (c *containerdClient) ExecStreaming(ctx context.Context, id, execID string, cmd []string, tty bool, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	ctx = c.nsCtx(ctx)
	container, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		return -1, err
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		return -1, err
	}

	spec, err := container.Spec(ctx)
	if err != nil {
		return -1, err
	}

	pspec := *spec.Process
	pspec.Args = cmd
	pspec.Terminal = tty

	process, err := task.Exec(ctx, execID, &pspec, cio.NewCreator(cio.WithStreams(stdin, stdout, stderr)))
	if err != nil {
		return -1, fmt.Errorf("exec streaming in %s: %w", id, err)
	}

	exitCh, err := process.Wait(ctx)
	if err != nil {
		process.Delete(ctx)
		return -1, err
	}

	if err := process.Start(ctx); err != nil {
		process.Delete(ctx)
		return -1, err
	}

	status := <-exitCh
	code, _, _ := status.Result()
	process.Delete(ctx)
	return int(code), nil
}

func (c *containerdClient) Metrics(ctx context.Context, id string) (*containerMetrics, error) {
	ctx = c.nsCtx(ctx)
	container, err := c.client.LoadContainer(ctx, id)
	if err != nil {
		return nil, err
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		return nil, err
	}

	metric, err := task.Metrics(ctx)
	if err != nil {
		return nil, err
	}

	return parseMetricProto(metric)
}

func (c *containerdClient) Cleanup(ctx context.Context, id string) {
	ctx = c.nsCtx(ctx)
	_ = c.KillTask(ctx, id, "SIGKILL")
	_ = c.DeleteTask(ctx, id)
	_ = c.DeleteContainer(ctx, id)
}

func (c *containerdClient) GarbageCollect(ctx context.Context, delay time.Duration) (int, error) {
	ctx = c.nsCtx(ctx)

	ctrs, err := c.client.Containers(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing containers: %w", err)
	}

	usedImages := make(map[string]bool)
	for _, ctr := range ctrs {
		img, err := ctr.Image(ctx)
		if err == nil {
			usedImages[img.Name()] = true
		}
	}

	imgs, err := c.client.ListImages(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing images: %w", err)
	}

	cutoff := time.Now().Add(-delay)
	removed := 0
	for _, img := range imgs {
		if usedImages[img.Name()] {
			continue
		}
		meta := img.Metadata()
		if meta.UpdatedAt.After(cutoff) {
			continue
		}
		if err := c.client.ImageService().Delete(ctx, img.Name()); err != nil {
			c.logger.Warn("failed to remove image", "image", img.Name(), "error", err)
			continue
		}
		c.logger.Info("removed unused image", "image", img.Name())
		removed++
	}

	return removed, nil
}

func parseDevice(s string) (hostPath, containerPath, permissions string) {
	hostPath, permissions = s, "rwm"
	parts := strings.SplitN(s, ":", 3)
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2]
	case 2:
		return parts[0], parts[1], permissions
	default:
		return
	}
}

func parseSignal(name string) syscall.Signal {
	name = strings.ToUpper(name)
	if !strings.HasPrefix(name, "SIG") {
		name = "SIG" + name
	}
	if sig := unix.SignalNum(name); sig != 0 {
		return sig
	}
	return syscall.SIGTERM
}

// --- Docker auth ---

type dockerConfig struct {
	Auths       map[string]dockerAuthEntry `json:"auths"`
	CredHelpers map[string]string          `json:"credHelpers"`
	CredsStore  string                     `json:"credsStore"`
}

type dockerAuthEntry struct {
	Auth string `json:"auth"`
}

func dockerCredentialFunc() func(string) (string, string, error) {
	path := os.Getenv("DOCKER_CONFIG")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		path = filepath.Join(home, ".docker")
	}
	configPath := filepath.Join(path, "config.json")

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}

	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil
	}

	return func(host string) (string, string, error) {
		if helper, ok := cfg.CredHelpers[host]; ok {
			return credHelperGet(helper, host)
		}

		if cfg.CredsStore != "" {
			user, pass, err := credHelperGet(cfg.CredsStore, host)
			if err == nil && user != "" {
				return user, pass, nil
			}
		}

		for registry, entry := range cfg.Auths {
			if strings.Contains(registry, host) || strings.Contains(host, registry) {
				if entry.Auth != "" {
					decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
					if err == nil {
						parts := strings.SplitN(string(decoded), ":", 2)
						if len(parts) == 2 {
							return parts[0], parts[1], nil
						}
					}
				}
			}
		}

		return "", "", nil
	}
}

func credHelperGet(helper, host string) (string, string, error) {
	cmd := exec.Command("docker-credential-"+helper, "get")
	cmd.Stdin = strings.NewReader(host)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", "", err
	}

	var result struct {
		Username string `json:"Username"`
		Secret   string `json:"Secret"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return "", "", err
	}
	return result.Username, result.Secret, nil
}
