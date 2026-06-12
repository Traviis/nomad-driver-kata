# nomad-driver-kata

Nomad task driver for [Kata Containers](https://katacontainers.io/) with sandbox-aware VM sharing.

All tasks within a Nomad task group share a single Kata VM, giving you
multi-container-per-VM isolation identical to how Kubernetes pods work
with Kata — but on Nomad.

## How it works

When the first task in an allocation starts, the driver creates and starts a
containerd sandbox using the Kata runtime. Subsequent tasks in the same
allocation are created with containerd's sandbox relationship set to that
sandbox ID, which makes the Kata shim place them in the same microVM. When all
tasks exit, the sandbox is torn down.

```
Nomad Allocation
├── Kata VM (sandbox)  ← one VM per allocation
│   ├── app container  ← task "app"
│   └── sidecar        ← task "sidecar"
└── shared network namespace inside the VM
```

## Requirements

- Linux with KVM (x86_64)
- containerd with `containerd-shim-kata-v2` in PATH
- Kata Containers runtime + guest assets (kernel, rootfs)
- Nomad 1.10+

## Installation

### Nix

```bash
nix build github:Traviis/nomad-driver-kata
```

The binary lands at `result/bin/nomad-driver-kata`. Copy it to your
Nomad plugin directory.

### From source

```bash
git clone https://github.com/Traviis/nomad-driver-kata
cd nomad-driver-kata
nix build  # or: go build -o nomad-driver-kata .
```

## Nomad client configuration

```hcl
plugin "nomad-driver-kata" {
  config {
    # Path to the containerd socket
    containerd_addr = "/run/docker/containerd/containerd.sock"

    # Timeout for pulling container images (default: "5m")
    image_pull_timeout = "5m"

    # Enable image garbage collection (default: true)
    gc_image = true

    # Minimum age before an unused image is removed (default: "3m")
    gc_image_delay = "3m"

    # containerd namespace
    namespace = "default"

    # Kata shimv2 runtime identifier
    runtime = "io.containerd.kata.v2"
  }
}
```

## Job spec

```hcl
job "myapp" {
  group "web" {
    task "app" {
      driver = "kata"

      config {
        image   = "docker.io/myorg/myapp:latest"
        command = "/app/server"
        args    = ["--port", "8080"]
      }
    }

    task "envoy" {
      driver = "kata"

      lifecycle {
        hook    = "prestart"
        sidecar = true
      }

      config {
        image   = "docker.io/envoyproxy/envoy:v1.31-latest"
        command = "envoy"
        args    = ["-c", "/etc/envoy/config.yaml"]
      }
    }
  }
}
```

Both `app` and `envoy` run inside the same Kata VM and share a network
namespace. This is the same topology Kubernetes uses with Kata — the
exact pattern that breaks when using Kata through Nomad's Docker driver
(which creates a separate VM per task).

## Task config reference

| Field             | Type          | Required | Description                              |
|-------------------|---------------|----------|------------------------------------------|
| `image`           | string        | yes      | OCI image reference                      |
| `command`         | string        | no       | Override entrypoint                      |
| `args`            | list(string)  | no       | Arguments to command                     |
| `cwd`             | string        | no       | Working directory inside the container   |
| `hostname`        | string        | no       | Container hostname                       |
| `force_pull`      | bool          | no       | Always pull the image, even if cached    |
| `privileged`      | bool          | no       | Run container in privileged mode         |
| `readonly_rootfs` | bool          | no       | Mount the root filesystem as read-only   |
| `pids_limit`      | number        | no       | Maximum number of processes in the container |
| `cap_add`         | list(string)  | no       | Linux capabilities to add                |
| `cap_drop`        | list(string)  | no       | Linux capabilities to drop               |
| `labels`          | map(string)   | no       | Container labels (metadata)              |
| `extra_hosts`     | list(string)  | no       | Extra /etc/hosts entries (`"host:ip"`)   |
| `devices`         | list(string)  | no       | Device mappings (`"/dev/foo:/dev/foo:rwm"`) |
| `auth`            | block         | no       | Registry credentials (`username`, `password`) |
| `ulimit`          | map(string)   | no       | Resource limits (e.g. `nofile = "1024:65536"`) |

## Development

```bash
cd nomad-driver-kata
nix develop
go build -o nomad-driver-kata .
go test ./...
```

## Testing

You can run the unit tests with:
```bash
nix flake check
# or
go test ./...
```

Integration tests with:
```bash
sudo nix run .#integration-test
```

## License

MIT
