package kata

import (
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
)

const (
	pluginName    = "kata"
	pluginVersion = "0.1.0"

	defaultContainerdAddr    = "/run/docker/containerd/containerd.sock"
	defaultNamespace         = "default"
	defaultPauseImage        = "registry.k8s.io/pause:3.9"
	defaultRuntime           = "io.containerd.kata.v2"
	defaultImagePullTimeout  = "5m"
	defaultGCImageDelay      = "3m"
	taskHandleVersion = 1
)

// PluginConfig holds driver-level settings from the Nomad client config.
type PluginConfig struct {
	ContainerdAddr   string `codec:"containerd_addr"`
	Namespace        string `codec:"namespace"`
	PauseImage       string `codec:"pause_image"`
	Runtime          string `codec:"runtime"`
	ImagePullTimeout string `codec:"image_pull_timeout"`
	GCImage          bool   `codec:"gc_image"`
	GCImageDelay     string `codec:"gc_image_delay"`
}

// TaskAuth holds credentials for pulling from private registries.
type TaskAuth struct {
	Username string `codec:"username"`
	Password string `codec:"password"`
}

// TaskConfig holds per-task settings from the job spec.
type TaskConfig struct {
	Image      string            `codec:"image"`
	Command    string            `codec:"command"`
	Args       []string          `codec:"args"`
	Cwd        string            `codec:"cwd"`
	Hostname   string            `codec:"hostname"`
	ForcePull  bool              `codec:"force_pull"`
	Privileged      bool              `codec:"privileged"`
	ReadonlyRootfs  bool              `codec:"readonly_rootfs"`
	PidsLimit       int64             `codec:"pids_limit"`
	CapAdd     []string          `codec:"cap_add"`
	CapDrop    []string          `codec:"cap_drop"`
	ExtraHosts []string          `codec:"extra_hosts"`
	Auth       TaskAuth          `codec:"auth"`
	Ulimit     map[string]string `codec:"ulimit"`
	Labels     map[string]string `codec:"labels"`
}

// TaskState is serialized into the task handle for recovery after driver restart.
type TaskState struct {
	ContainerID string `codec:"container_id"`
	SandboxID   string `codec:"sandbox_id"`
	AllocID     string `codec:"alloc_id"`
	TaskName    string `codec:"task_name"`
	StartedAt   int64  `codec:"started_at"`
}

var pluginConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	"containerd_addr": hclspec.NewDefault(
		hclspec.NewAttr("containerd_addr", "string", false),
		hclspec.NewLiteral(`"`+defaultContainerdAddr+`"`),
	),
	"namespace": hclspec.NewDefault(
		hclspec.NewAttr("namespace", "string", false),
		hclspec.NewLiteral(`"`+defaultNamespace+`"`),
	),
	"pause_image": hclspec.NewDefault(
		hclspec.NewAttr("pause_image", "string", false),
		hclspec.NewLiteral(`"`+defaultPauseImage+`"`),
	),
	"runtime": hclspec.NewDefault(
		hclspec.NewAttr("runtime", "string", false),
		hclspec.NewLiteral(`"`+defaultRuntime+`"`),
	),
	"image_pull_timeout": hclspec.NewDefault(
		hclspec.NewAttr("image_pull_timeout", "string", false),
		hclspec.NewLiteral(`"`+defaultImagePullTimeout+`"`),
	),
	"gc_image": hclspec.NewDefault(
		hclspec.NewAttr("gc_image", "bool", false),
		hclspec.NewLiteral(`true`),
	),
	"gc_image_delay": hclspec.NewDefault(
		hclspec.NewAttr("gc_image_delay", "string", false),
		hclspec.NewLiteral(`"`+defaultGCImageDelay+`"`),
	),
})

var taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	"image":      hclspec.NewAttr("image", "string", true),
	"command":    hclspec.NewAttr("command", "string", false),
	"args":       hclspec.NewAttr("args", "list(string)", false),
	"cwd":        hclspec.NewAttr("cwd", "string", false),
	"force_pull": hclspec.NewAttr("force_pull", "bool", false),
	"privileged": hclspec.NewAttr("privileged", "bool", false),
	"auth": hclspec.NewBlock("auth", false, hclspec.NewObject(map[string]*hclspec.Spec{
		"username": hclspec.NewAttr("username", "string", false),
		"password": hclspec.NewAttr("password", "string", false),
	})),
	"ulimit":    hclspec.NewAttr("ulimit", "map(string)", false),
	"pids_limit": hclspec.NewAttr("pids_limit", "number", false),
	"cap_add":   hclspec.NewAttr("cap_add", "list(string)", false),
	"cap_drop":  hclspec.NewAttr("cap_drop", "list(string)", false),
	"labels":   hclspec.NewAttr("labels", "map(string)", false),
	"readonly_rootfs": hclspec.NewAttr("readonly_rootfs", "bool", false),
	"hostname": hclspec.NewAttr("hostname", "string", false),
	"extra_hosts": hclspec.NewAttr("extra_hosts", "list(string)", false),
})
