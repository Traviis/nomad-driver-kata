package kata

import (
	"context"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"io"
	"os"
	"sync"
	"time"
)

type call struct {
	Method string
	Args   []interface{}
}

type recorder struct {
	mu          sync.Mutex
	calls       []call
	version     string
	versionErr  error
	running     map[string]bool
	metrics     *containerMetrics
	metricsErr  error
	runExit     int
	runErr      error
	runCh       chan struct{}
	configs     []*ContainerConfig
	imageConfig ocispec.ImageConfig

	createContainerErrFor map[string]error
	garbageCollectCount   int
}

func newRecorder() *recorder {
	return &recorder{
		running: make(map[string]bool),
		version: "test-1.0",
	}
}

func (r *recorder) record(method string, args ...interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call{Method: method, Args: args})
}

func (r *recorder) called(method string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c.Method == method {
			return true
		}
	}
	return false
}

func (r *recorder) callCount(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

func (r *recorder) lastConfig() *ContainerConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.configs) == 0 {
		return nil
	}
	return r.configs[len(r.configs)-1]
}

func (r *recorder) configForID(id string) *ContainerConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, cfg := range r.configs {
		if cfg.ID == id {
			return cfg
		}
	}
	return nil
}

func (r *recorder) Close() error { return nil }

func (r *recorder) Version(ctx context.Context) (string, error) {
	r.record("Version")
	if r.versionErr != nil {
		return "", r.versionErr
	}
	return r.version, nil
}

func (r *recorder) EnsureImage(ctx context.Context, ref string, forcePull bool, username, password string) error {
	r.record("EnsureImage", ref, forcePull, username, password)
	return nil
}

func (r *recorder) ImageConfig(ctx context.Context, ref string) (ocispec.ImageConfig, error) {
	r.record("ImageConfig", ref)
	return r.imageConfig, nil
}

func (r *recorder) CreateSandboxMetadata(ctx context.Context, id, runtime string) error {
	r.record("CreateSandboxMetadata", id, runtime)
	return nil
}

func (r *recorder) DeleteSandboxMetadata(ctx context.Context, id string) error {
	r.record("DeleteSandboxMetadata", id)
	return nil
}

func (r *recorder) CreateContainer(ctx context.Context, cfg *ContainerConfig) error {
	r.mu.Lock()
	r.configs = append(r.configs, cfg)
	var errForID error
	if r.createContainerErrFor != nil {
		errForID = r.createContainerErrFor[cfg.ID]
	}
	r.mu.Unlock()
	r.record("CreateContainer", cfg.ID, cfg.Image, cfg.Runtime)
	if errForID != nil {
		return errForID
	}
	return nil
}

func (r *recorder) DeleteContainer(ctx context.Context, id string) error {
	r.record("DeleteContainer", id)
	return nil
}

func (r *recorder) StartTaskDetached(ctx context.Context, id string) error {
	r.record("StartTaskDetached", id)
	r.mu.Lock()
	r.running[id] = true
	r.mu.Unlock()
	return nil
}

func (r *recorder) RunTask(ctx context.Context, id string, stdout, stderr *os.File) (int, error) {
	r.record("RunTask", id)
	if r.runCh != nil {
		<-r.runCh
	}
	return r.runExit, r.runErr
}

func (r *recorder) MonitorTask(ctx context.Context, id string, stdout, stderr *os.File) (int, error) {
	r.record("MonitorTask", id)
	return 0, nil
}

func (r *recorder) KillTask(ctx context.Context, id string, signal string) error {
	r.record("KillTask", id, signal)
	return nil
}

func (r *recorder) DeleteTask(ctx context.Context, id string) error {
	r.record("DeleteTask", id)
	return nil
}

func (r *recorder) TaskRunning(ctx context.Context, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[id]
}

func (r *recorder) Exec(ctx context.Context, id, execID string, cmd []string) (string, int, error) {
	r.record("Exec", id, execID, cmd)
	return "", 0, nil
}

func (r *recorder) ExecStreaming(ctx context.Context, id, execID string, cmd []string, tty bool, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	r.record("ExecStreaming", id, execID, cmd, tty)
	return 0, nil
}

func (r *recorder) Metrics(ctx context.Context, id string) (*containerMetrics, error) {
	r.record("Metrics", id)
	if r.metricsErr != nil {
		return nil, r.metricsErr
	}
	if r.metrics != nil {
		return r.metrics, nil
	}
	return &containerMetrics{Timestamp: time.Now()}, nil
}

func (r *recorder) Cleanup(ctx context.Context, id string) {
	r.record("Cleanup", id)
	r.mu.Lock()
	delete(r.running, id)
	r.mu.Unlock()
}

func (r *recorder) GarbageCollect(ctx context.Context, delay time.Duration) (int, error) {
	r.record("GarbageCollect", delay)
	r.mu.Lock()
	r.garbageCollectCount++
	r.mu.Unlock()
	return 0, nil
}
