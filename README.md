# nomad-driver-kata

Nomad task driver for [Kata Containers](https://katacontainers.io/) with sandbox-aware VM sharing.

All tasks within a Nomad task group share a single Kata VM, giving you
multi-container-per-VM isolation identical to how Kubernetes pods work
with Kata — but on Nomad.

## Critical failure semantics

> [!WARNING]
> **Normal single-task restarts are supported.** If one task exits or Nomad
> restarts it while the shared Kata VM, shim, and agent remain healthy, the
> driver recreates only that task inside the existing VM. Sibling tasks keep
> running.
>
> The failure boundary differs from Nomad's standard Docker driver when the
> **shared sandbox** fails. Every Kata task in an allocation shares one VM and
> one Kata shim. If the VM dies, the shim dies, or the Kata runtime control
> plane becomes irrecoverably unavailable, all tasks in the allocation lose
> their runtime together. QEMU may still be running after shim death; that VM
> is nevertheless operationally unrecoverable.
>
> The driver treats confirmed shared-sandbox death as an allocation-terminal
> failure.
> It records the allocation as poisoned, rejects every later `StartTask` call
> for that allocation with an unrecoverable error, cleans exact orphaned QEMU
> and `virtiofsd` processes, and lets Nomad create a replacement allocation.
> It will not recreate a VM under the same allocation ID.

In short:

```text
One task exits + shared sandbox healthy     → restart that task
Shared VM/shim/control plane becomes dead   → replace the allocation
```

Sandbox death is different from an ordinary task exit: Kata may leave live
QEMU, `virtiofsd`, QMP sockets, network qdiscs, and other runtime state after
abrupt shim failure.
Starting another sandbox with the same ID can fail on stale resources or create
a runtime state that disagrees with Nomad and containerd.

Allocation replacement is not live recovery. Running processes, VM memory,
container connections, and unflushed state are lost. Allocation-local files
must also be treated as disposable unless the surrounding Nomad storage design
explicitly preserves them across replacement. Jobs using this driver must have
a reschedule policy that permits a replacement allocation.

By contrast, Nomad's Docker driver normally isolates task runtime failures to an
individual container. Do not assume Docker-driver restart behavior when moving
a multi-task group to this driver.

## How it works

When the first task in an allocation starts, the driver creates a pause
container with the Kata runtime and records it as the allocation sandbox in
containerd metadata. Subsequent tasks in the same allocation are created with
containerd's sandbox relationship set to that sandbox ID, which lets containerd
reuse the existing Kata shim and place them in the same microVM. When all tasks
exit, the sandbox container and metadata are torn down.

```
Nomad Allocation
├── Kata VM (sandbox)  ← one VM per allocation
│   ├── pause container (keeps VM alive)
│   ├── app container   ← task "app"
│   └── sidecar         ← task "sidecar"
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

    # Image used for the sandbox container (default: "registry.k8s.io/pause:3.9")
    pause_image = "registry.k8s.io/pause:3.9"

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
