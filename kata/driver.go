package kata

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/drivers/fsisolation"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
)

var (
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		PluginApiVersions: []string{drivers.ApiVersion010},
		PluginVersion:     pluginVersion,
		Name:              pluginName,
	}

	driverCapabilities = &drivers.Capabilities{
		SendSignals:  true,
		Exec:         true,
		FSIsolation:  fsisolation.Image,
		MountConfigs: drivers.MountConfigSupportAll,
		NetIsolationModes: []drivers.NetIsolationMode{
			drivers.NetIsolationModeHost,
			drivers.NetIsolationModeGroup,
		},
	}
)

// Driver implements drivers.DriverPlugin for Kata Containers.
type Driver struct {
	logger     hclog.Logger
	eventer    *eventer.Eventer
	config     *PluginConfig
	ctr        *CtrClient
	sandboxMgr *SandboxManager
	tasks      *taskStore
	ctx        context.Context
	cancel     context.CancelFunc
}

type taskStore struct {
	mu    sync.RWMutex
	store map[string]*taskHandle
}

func newTaskStore() *taskStore {
	return &taskStore{store: make(map[string]*taskHandle)}
}

func (ts *taskStore) Set(id string, h *taskHandle) {
	ts.mu.Lock()
	ts.store[id] = h
	ts.mu.Unlock()
}

func (ts *taskStore) Get(id string) (*taskHandle, bool) {
	ts.mu.RLock()
	h, ok := ts.store[id]
	ts.mu.RUnlock()
	return h, ok
}

func (ts *taskStore) Delete(id string) {
	ts.mu.Lock()
	delete(ts.store, id)
	ts.mu.Unlock()
}

func NewDriver(logger hclog.Logger) drivers.DriverPlugin {
	ctx, cancel := context.WithCancel(context.Background())
	return &Driver{
		logger: logger.Named(pluginName),
		ctx:    ctx,
		cancel: cancel,
		tasks:  newTaskStore(),
	}
}

// --- base.BasePlugin ---

func (d *Driver) PluginInfo() (*base.PluginInfoResponse, error) {
	return pluginInfo, nil
}

func (d *Driver) ConfigSchema() (*hclspec.Spec, error) {
	return pluginConfigSpec, nil
}

func (d *Driver) SetConfig(cfg *base.Config) error {
	var config PluginConfig
	if len(cfg.PluginConfig) > 0 {
		if err := base.MsgPackDecode(cfg.PluginConfig, &config); err != nil {
			return fmt.Errorf("decoding plugin config: %w", err)
		}
	}

	if config.ContainerdAddr == "" {
		config.ContainerdAddr = defaultContainerdAddr
	}
	if config.CtrPath == "" {
		config.CtrPath = defaultCtrPath
	}
	if config.Namespace == "" {
		config.Namespace = defaultNamespace
	}
	if config.PauseImage == "" {
		config.PauseImage = defaultPauseImage
	}
	if config.Runtime == "" {
		config.Runtime = defaultRuntime
	}

	d.config = &config
	d.ctr = NewCtrClient(config.CtrPath, config.ContainerdAddr, config.Namespace, d.logger)
	d.sandboxMgr = NewSandboxManager(d.ctr, d.logger)
	d.eventer = eventer.NewEventer(d.ctx, d.logger)

	return nil
}

// --- drivers.DriverPlugin ---

func (d *Driver) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

func (d *Driver) Capabilities() (*drivers.Capabilities, error) {
	return driverCapabilities, nil
}

func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.fingerprint(ctx, ch)
	return ch, nil
}

func (d *Driver) fingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
	defer close(ch)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ch <- d.buildFingerprint():
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (d *Driver) buildFingerprint() *drivers.Fingerprint {
	attrs := map[string]*pstructs.Attribute{
		"driver.kata.version": pstructs.NewStringAttribute(pluginVersion),
	}

	health := drivers.HealthStateHealthy
	desc := "ready"

	if d.ctr == nil {
		health = drivers.HealthStateUndetected
		desc = "driver not configured"
	} else if _, err := d.ctr.Version(context.Background()); err != nil {
		health = drivers.HealthStateUnhealthy
		desc = fmt.Sprintf("containerd unavailable: %v", err)
	}

	return &drivers.Fingerprint{
		Attributes:        attrs,
		Health:            health,
		HealthDescription: desc,
	}
}

func (d *Driver) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	if cfg.AllocID == "" {
		return nil, nil, fmt.Errorf("alloc ID is required")
	}

	var taskCfg TaskConfig
	if err := cfg.DecodeDriverConfig(&taskCfg); err != nil {
		return nil, nil, fmt.Errorf("decoding task config: %w", err)
	}

	if taskCfg.Image == "" {
		return nil, nil, fmt.Errorf("image is required")
	}

	d.logger.Info("starting task", "alloc_id", cfg.AllocID, "task", cfg.Name, "image", taskCfg.Image)

	ctx := context.Background()
	containerID := fmt.Sprintf("kata-%s-%s", cfg.AllocID, cfg.Name)

	// Clean up any leftover state from a previous attempt
	d.ctr.Cleanup(ctx, containerID)

	var netNS string
	if cfg.NetworkIsolation != nil && cfg.NetworkIsolation.Path != "" {
		netNS = cfg.NetworkIsolation.Path
		d.logger.Info("using network namespace", "path", netNS)
	}

	sandbox, err := d.sandboxMgr.GetOrCreate(ctx, cfg.AllocID, d.config.PauseImage, d.config.Runtime, netNS, cfg.TaskGroupName)
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox setup: %w", err)
	}

	if err := d.ctr.EnsureImage(ctx, taskCfg.Image, taskCfg.ForcePull, taskCfg.Auth.Username, taskCfg.Auth.Password); err != nil {
		d.sandboxMgr.Release(ctx, cfg.AllocID)
		return nil, nil, fmt.Errorf("pulling image %s: %w", taskCfg.Image, err)
	}

	var command []string
	if taskCfg.Command != "" {
		command = append(command, taskCfg.Command)
		command = append(command, taskCfg.Args...)
	}

	annotations := map[string]string{
		"io.kubernetes.cri-o.ContainerType": "container",
		"io.kubernetes.cri-o.SandboxID":     sandbox.ID,
	}

	configDir := filepath.Join(os.TempDir(), "kata-driver", cfg.AllocID, cfg.Name)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		d.sandboxMgr.Release(ctx, cfg.AllocID)
		return nil, nil, fmt.Errorf("creating config dir: %w", err)
	}

	resolvPath := filepath.Join(configDir, "resolv.conf")
	if err := d.writeResolvConf(resolvPath, cfg.DNS); err != nil {
		d.sandboxMgr.Release(ctx, cfg.AllocID)
		return nil, nil, fmt.Errorf("writing resolv.conf: %w", err)
	}

	hostsPath := filepath.Join(configDir, "hosts")
	if err := d.writeHosts(hostsPath, hostsConfig(cfg)); err != nil {
		d.sandboxMgr.Release(ctx, cfg.AllocID)
		return nil, nil, fmt.Errorf("writing hosts: %w", err)
	}

	var envVars []string
	for k, v := range cfg.Env {
		if k == "PATH" {
			continue
		}
		envVars = append(envVars, fmt.Sprintf("%s=%s", k, v))
	}
	envVars = addPortEnv(envVars, cfg)
	for k, v := range cfg.DeviceEnv {
		envVars = append(envVars, fmt.Sprintf("%s=%s", k, v))
	}

	mounts, err := buildMounts(cfg, resolvPath, hostsPath)
	if err != nil {
		d.sandboxMgr.Release(ctx, cfg.AllocID)
		return nil, nil, fmt.Errorf("building mounts: %w", err)
	}

	var memLimit int64
	var cpuQuota, cpuPeriod int64
	if cfg.Resources != nil && cfg.Resources.LinuxResources != nil {
		lr := cfg.Resources.LinuxResources
		memLimit = lr.MemoryLimitBytes
		cpuQuota = lr.CPUQuota
		cpuPeriod = lr.CPUPeriod
	}

	if err := d.ctr.CreateContainer(ctx, &ContainerConfig{
		ID:               containerID,
		Image:            taskCfg.Image,
		Runtime:          d.config.Runtime,
		Annotations:      annotations,
		Command:          command,
		Env:              envVars,
		Mounts:           mounts,
		User:             cfg.User,
		Cwd:              taskCfg.Cwd,
		MemoryLimitBytes: memLimit,
		CPUQuota:         cpuQuota,
		CPUPeriod:        cpuPeriod,
		Privileged:       taskCfg.Privileged,
		Ulimit:           taskCfg.Ulimit,
	}); err != nil {
		d.sandboxMgr.Release(ctx, cfg.AllocID)
		return nil, nil, fmt.Errorf("creating container: %w", err)
	}

	h := &taskHandle{
		containerID: containerID,
		sandboxID:   sandbox.ID,
		allocID:     cfg.AllocID,
		taskName:    cfg.Name,
		ctr:         d.ctr,
		logger:      d.logger.With("container_id", containerID),
		startedAt:   time.Now(),
		doneCh:      make(chan struct{}),
	}

	go h.run(cfg.StdoutPath, cfg.StderrPath)

	d.tasks.Set(cfg.ID, h)

	state := &TaskState{
		ContainerID: containerID,
		SandboxID:   sandbox.ID,
		AllocID:     cfg.AllocID,
		TaskName:    cfg.Name,
		StartedAt:   h.startedAt.UnixNano(),
	}

	handle := drivers.NewTaskHandle(taskHandleVersion)
	handle.Config = cfg
	if err := handle.SetDriverState(state); err != nil {
		d.ctr.Cleanup(ctx, containerID)
		d.sandboxMgr.Release(ctx, cfg.AllocID)
		return nil, nil, fmt.Errorf("setting driver state: %w", err)
	}

	return handle, buildDriverNetwork(cfg), nil
}

func (d *Driver) RecoverTask(handle *drivers.TaskHandle) error {
	if handle == nil {
		return fmt.Errorf("nil task handle")
	}

	var state TaskState
	if err := handle.GetDriverState(&state); err != nil {
		return fmt.Errorf("reading driver state: %w", err)
	}

	d.logger.Info("recovering task", "container_id", state.ContainerID, "sandbox_id", state.SandboxID)

	if !d.ctr.TaskRunning(context.Background(), state.ContainerID) {
		return fmt.Errorf("container %s no longer running", state.ContainerID)
	}

	d.sandboxMgr.Recover(state.AllocID, state.SandboxID)

	h := &taskHandle{
		containerID: state.ContainerID,
		sandboxID:   state.SandboxID,
		allocID:     state.AllocID,
		taskName:    state.TaskName,
		ctr:         d.ctr,
		logger:      d.logger.With("container_id", state.ContainerID),
		startedAt:   time.Unix(0, state.StartedAt),
		doneCh:      make(chan struct{}),
	}

	go h.monitorRecovered(handle.Config.StdoutPath, handle.Config.StderrPath)

	d.tasks.Set(handle.Config.ID, h)
	return nil
}

func (d *Driver) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	h, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, fmt.Errorf("task %s not found", taskID)
	}

	ch := make(chan *drivers.ExitResult)
	go func() {
		defer close(ch)
		select {
		case <-h.doneCh:
			ch <- h.ExitResult()
		case <-ctx.Done():
			ch <- &drivers.ExitResult{Err: ctx.Err()}
		}
	}()

	return ch, nil
}

func (d *Driver) StopTask(taskID string, timeout time.Duration, signal string) error {
	h, ok := d.tasks.Get(taskID)
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}

	d.logger.Info("stopping task", "task_id", taskID, "container_id", h.containerID, "signal", signal)

	if signal == "" {
		signal = "SIGTERM"
	}

	if err := d.ctr.KillTask(context.Background(), h.containerID, signal); err != nil {
		d.logger.Warn("signal failed, force killing", "error", err)
		_ = d.ctr.KillTask(context.Background(), h.containerID, "SIGKILL")
	}

	select {
	case <-h.doneCh:
	case <-time.After(timeout):
		d.logger.Warn("timeout waiting for task, force killing", "timeout", timeout)
		_ = d.ctr.KillTask(context.Background(), h.containerID, "SIGKILL")
	}

	return nil
}

func (d *Driver) DestroyTask(taskID string, force bool) error {
	h, ok := d.tasks.Get(taskID)
	if !ok {
		return nil
	}

	d.logger.Info("destroying task", "task_id", taskID, "container_id", h.containerID)

	ctx := context.Background()

	if h.IsRunning() {
		if !force {
			return fmt.Errorf("task %s still running", taskID)
		}
		_ = d.ctr.KillTask(ctx, h.containerID, "SIGKILL")
		select {
		case <-h.doneCh:
		case <-time.After(5 * time.Second):
		}
	}

	_ = d.ctr.DeleteTask(ctx, h.containerID)
	_ = d.ctr.DeleteContainer(ctx, h.containerID)
	d.sandboxMgr.Release(ctx, h.allocID)
	configDir := filepath.Join(os.TempDir(), "kata-driver", h.allocID, h.taskName)
	os.RemoveAll(configDir)
	d.tasks.Delete(taskID)

	return nil
}

func (d *Driver) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	h, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, fmt.Errorf("task %s not found", taskID)
	}
	return h.TaskStatus(), nil
}

func (d *Driver) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	h, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, fmt.Errorf("task %s not found", taskID)
	}

	d.logger.Debug("TaskStats called", "task_id", taskID, "container_id", h.containerID, "interval", interval)

	ch := make(chan *drivers.TaskResourceUsage)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var previous *containerMetrics
		for {
			select {
			case <-ctx.Done():
				d.logger.Debug("TaskStats context done", "task_id", taskID)
				return
			case <-ticker.C:
				metrics, err := d.ctr.Metrics(ctx, h.containerID)
				if err != nil {
					d.logger.Warn("failed to read task metrics", "task_id", taskID, "container_id", h.containerID, "error", err)
					continue
				}
				d.logger.Debug("TaskStats collected", "task_id", taskID, "rss", metrics.MemoryRSSBytes, "usage", metrics.MemoryUsageBytes, "cpu_ns", metrics.CPUUsageNanos)
				usage := metrics.ResourceUsage(previous)
				previous = metrics
				ch <- usage
			}
		}
	}()
	return ch, nil
}

func (d *Driver) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.eventer.TaskEvents(ctx)
}

func (d *Driver) SignalTask(taskID string, signal string) error {
	h, ok := d.tasks.Get(taskID)
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	return d.ctr.KillTask(context.Background(), h.containerID, signal)
}

func (d *Driver) ExecTask(taskID string, cmd []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	h, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, fmt.Errorf("task %s not found", taskID)
	}

	execID := fmt.Sprintf("exec-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out, exitCode, err := d.ctr.Exec(ctx, h.containerID, execID, cmd)
	if err != nil {
		return nil, err
	}

	return &drivers.ExecTaskResult{
		Stdout: []byte(out),
		ExitResult: &drivers.ExitResult{
			ExitCode: exitCode,
		},
	}, nil
}

func (d *Driver) ExecTaskStreaming(ctx context.Context, taskID string, execOptions *drivers.ExecOptions) (*drivers.ExitResult, error) {
	h, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, fmt.Errorf("task %s not found", taskID)
	}

	execID := fmt.Sprintf("exec-%d", time.Now().UnixNano())
	exitCode, err := d.ctr.ExecStreaming(ctx, h.containerID, execID, execOptions.Command, execOptions.Tty, execOptions.Stdin, execOptions.Stdout, execOptions.Stderr)
	if err != nil {
		return nil, err
	}

	return &drivers.ExitResult{ExitCode: exitCode}, nil
}

func (d *Driver) writeResolvConf(path string, dns *drivers.DNSConfig) error {
	var lines []string
	if dns != nil && len(dns.Servers) > 0 {
		for _, s := range dns.Servers {
			lines = append(lines, "nameserver "+s)
		}
		if len(dns.Searches) > 0 {
			lines = append(lines, "search "+strings.Join(dns.Searches, " "))
		}
		if len(dns.Options) > 0 {
			lines = append(lines, "options "+strings.Join(dns.Options, " "))
		}
	} else {
		data, err := os.ReadFile("/etc/resolv.conf")
		if err != nil {
			lines = append(lines, "nameserver 8.8.8.8", "nameserver 1.1.1.1")
		} else {
			hasNonLoopback := false
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "nameserver") {
					fields := strings.Fields(line)
					if len(fields) >= 2 && !strings.HasPrefix(fields[1], "127.") {
						lines = append(lines, line)
						hasNonLoopback = true
					}
				} else if line != "" && !strings.HasPrefix(line, "#") {
					lines = append(lines, line)
				}
			}
			if !hasNonLoopback {
				lines = append([]string{"nameserver 8.8.8.8", "nameserver 1.1.1.1"}, lines...)
			}
		}
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

func (d *Driver) writeHosts(path string, hosts *drivers.HostsConfig) error {
	lines := []string{
		"127.0.0.1 localhost",
		"::1 localhost ip6-localhost ip6-loopback",
	}
	if hosts != nil && hosts.Address != "" && hosts.Hostname != "" {
		lines = append(lines, hosts.Address+" "+hosts.Hostname)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644)
}

func hostsConfig(cfg *drivers.TaskConfig) *drivers.HostsConfig {
	if cfg.NetworkIsolation == nil || cfg.NetworkIsolation.HostsConfig == nil {
		return nil
	}
	return cfg.NetworkIsolation.HostsConfig
}

func buildMounts(cfg *drivers.TaskConfig, resolvPath, hostsPath string) ([]string, error) {
	taskDirs := cfg.TaskDir()
	mounts := []string{
		mountString(taskDirs.SharedAllocDir, allocdir.SharedAllocContainerPath, false, ""),
		mountString(taskDirs.LocalDir, allocdir.TaskLocalContainerPath, false, ""),
		mountString(taskDirs.SecretsDir, allocdir.TaskSecretsContainerPath, false, ""),
		mountString(resolvPath, "/etc/resolv.conf", true, "file"),
		mountString(hostsPath, "/etc/hosts", true, "file"),
	}

	for _, m := range cfg.Mounts {
		if m == nil {
			continue
		}
		if m.HostPath == "" || m.TaskPath == "" {
			return nil, fmt.Errorf("mount requires host path and task path")
		}
		mounts = append(mounts, mountString(m.HostPath, m.TaskPath, m.Readonly, m.PropagationMode))
	}
	return mounts, nil
}

func mountString(src, dst string, readonly bool, propagation string) string {
	options := []string{"rbind", propagationOption(propagation)}
	if propagation == "file" {
		options = []string{"bind"}
	}
	if readonly {
		options = append(options, "ro")
	}
	return fmt.Sprintf("type=bind,src=%s,dst=%s,options=%s", src, dst, strings.Join(options, ":"))
}

func propagationOption(mode string) string {
	switch mode {
	case "", structs.VolumeMountPropagationPrivate:
		return "rprivate"
	case structs.VolumeMountPropagationHostToTask:
		return "rslave"
	case structs.VolumeMountPropagationBidirectional:
		return "rshared"
	default:
		return mode
	}
}

func buildDriverNetwork(cfg *drivers.TaskConfig) *drivers.DriverNetwork {
	if cfg.Resources == nil || cfg.Resources.Ports == nil || len(*cfg.Resources.Ports) == 0 {
		return nil
	}
	portMap := make(map[string]int, len(*cfg.Resources.Ports))
	for _, port := range *cfg.Resources.Ports {
		if port.To > 0 {
			portMap[port.Label] = port.To
		} else {
			portMap[port.Label] = port.Value
		}
	}
	if len(portMap) == 0 {
		return nil
	}

	network := &drivers.DriverNetwork{PortMap: portMap}
	if cfg.NetworkIsolation != nil && cfg.NetworkIsolation.HostsConfig != nil {
		network.IP = cfg.NetworkIsolation.HostsConfig.Address
		network.AutoAdvertise = network.IP != ""
	}
	return network
}

func addPortEnv(envVars []string, cfg *drivers.TaskConfig) []string {
	if cfg.Resources == nil || cfg.Resources.Ports == nil {
		return envVars
	}
	for _, port := range *cfg.Resources.Ports {
		value := port.Value
		if port.To > 0 {
			value = port.To
		}
		envVars = append(envVars, fmt.Sprintf("%s%s=%d", taskenv.PortPrefix, port.Label, value))
		envVars = append(envVars, fmt.Sprintf("%s%s=%d", taskenv.AllocPortPrefix, port.Label, value))
	}
	return envVars
}
