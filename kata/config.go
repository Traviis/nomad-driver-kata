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

	taskHandleVersion = 1
)

// PluginConfig holds driver-level settings from the Nomad client config.
type PluginConfig struct {
	ContainerdAddr   string `codec:"containerd_addr"`
	Namespace        string `codec:"namespace"`
	PauseImage       string `codec:"pause_image"`
	Runtime          string `codec:"runtime"`
	ImagePullTimeout string `codec:"image_pull_timeout"`
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
	ForcePull  bool              `codec:"force_pull"`
	Privileged bool              `codec:"privileged"`
	Auth       TaskAuth          `codec:"auth"`
	Ulimit     map[string]string `codec:"ulimit"`
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
	"ulimit": hclspec.NewAttr("ulimit", "map(string)", false),
})
