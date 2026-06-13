# Codebase layout

This document explains how the code is organized and where to look for
specific functionality. It assumes no prior experience writing Nomad
task drivers.

## How Nomad drivers work

A Nomad task driver is a Go plugin that implements the `DriverPlugin`
interface from `github.com/hashicorp/nomad/plugins/drivers`. Nomad's
client agent loads the plugin binary, calls `SetConfig` to hand it
cluster-level configuration, then calls methods like `StartTask`,
`StopTask`, and `DestroyTask` as allocations are scheduled.

The plugin runs as a separate process and communicates with Nomad over
gRPC. The entry point is `main.go`, which registers the plugin factory
with Nomad's plugin framework. The factory creates a `Driver` from the
`kata` package — everything else lives there.

## File map

```
main.go              Plugin entry point. Registers kata.NewDriver as
                     the plugin factory. You should never need to
                     change this.

kata/
  config.go          All configuration types and HCL specs.
                     - PluginConfig: driver-level settings (containerd
                       address, namespace, runtime, GC options)
                     - TaskConfig: per-task settings from the job spec
                       (image, command, caps, devices, etc.)
                     - TaskState: serialized into task handles for
                       recovery after driver restart
                     - pluginConfigSpec / taskConfigSpec: HCL schema
                       definitions that tell Nomad what config fields
                       the driver accepts

  driver.go          The Driver struct and all DriverPlugin methods.
                     This is the main file. Key sections:
                     - SetConfig: called once at startup, connects to
                       containerd, starts the image GC goroutine
                     - Fingerprint: periodic health checks reported to
                       the Nomad server
                     - StartTask: the primary code path — pulls the
                       image, creates the sandbox VM (if first task in
                       the allocation), writes resolv.conf and hosts,
                       builds mounts and env vars, creates the
                       container, and launches the task goroutine
                     - StopTask / DestroyTask: signal, kill, clean up
                     - ExecTask / ExecTaskStreaming: run commands inside
                       a running container
                     - Helper functions at the bottom: DNS resolution
                       (writeResolvConf, hostResolvConf, filterResolvConf),
                       hosts file generation (writeHosts), mount
                       building (buildMounts, bindMount), network
                       config (buildDriverNetwork, addPortEnv)

  containerd.go      The containerd integration layer.
                     - Containerd interface: all operations the driver
                       needs from containerd (image pulls, container
                       CRUD, task lifecycle, exec, metrics, GC). The
                       driver only talks to containerd through this
                       interface.
                     - containerdClient: the real implementation using
                       containerd's v2 Go SDK over gRPC
                     - CreateContainer: builds the OCI spec from a
                       ContainerConfig — resource limits, capabilities,
                       devices, mounts, namespaces, etc.
                     - Docker auth: credential resolution supporting
                       explicit username/password, Docker config.json
                       static credentials, and credential helpers
                       (e.g. docker-credential-ecr-login)

  handle.go          Per-task runtime state.
                     - taskHandle: tracks a single running container —
                       its IDs, start/completion times, exit result
                     - run(): launches the containerd task with log
                       file IO, blocks until exit
                     - monitorRecovered(): re-attaches to a task after
                       driver restart
                     - IsRunning / ExitResult / TaskStatus: read task
                       state under a lock

  sandbox.go         Kata VM lifecycle management.
                     - SandboxManager: maintains a map from allocation
                       ID to Sandbox. Each sandbox is a Kata microVM
                       anchored by a pause container and registered in
                       containerd's sandbox metadata store.
                     - GetOrCreate: boots a new VM for the first task
                       in an allocation, reuses it for subsequent tasks
                       (reference counted)
                     - Release: decrements the ref count; tears down
                       the VM when it hits zero
                     - Recover: rebuilds the sandbox map after driver
                       restart without creating new VMs

  stats.go           Metrics collection and conversion.
                     - containerMetrics: intermediate representation of
                       cgroup v2 stats from containerd
                     - parseMetricProto: unmarshals containerd's
                       protobuf metrics into containerMetrics
                     - ResourceUsage: converts to Nomad's
                       TaskResourceUsage format with CPU percentage
                       calculated from consecutive samples
```

## Test files

```
kata/
  recorder_test.go   Test double implementing the Containerd interface.
                     Records every method call for assertions. Used by
                     all test files — the driver never talks to a real
                     containerd in tests.

  driver_test.go     Tests for driver logic: config handling, DNS/hosts
                     file generation, mount building, port env vars,
                     network construction, fingerprinting, and the
                     StartTask/DestroyTask integration paths.

  sandbox_test.go    Tests for sandbox lifecycle: creation, reuse,
                     release, recovery, hostname/netNS passthrough.

  stats_test.go      Tests for CPU percentage calculation, memory stat
                     mapping, and edge cases in the percent function.
```

## Key concepts

**One VM per allocation.** When the first task in a Nomad allocation
starts, the driver starts a Kata-backed pause container and records it in
containerd's sandbox metadata store. All subsequent tasks in the same
allocation are created with that sandbox ID, so containerd can reuse the
existing Kata shim and place them in the same microVM.

**Interface-based containerd access.** The `Containerd` interface in
`containerd.go` is the boundary between driver logic and the container
runtime. The real client uses gRPC; tests use a recording fake. If you
need a new containerd operation, add it to the interface first.

**Nomad owns networking.** The driver declares support for
`NetIsolationModeGroup`, which means Nomad sets up CNI network
namespaces and passes the path to the driver. The driver just joins
the namespace — it never configures networking itself.

**Nomad owns volumes.** CSI volumes and host volumes appear as entries
in `cfg.Mounts` by the time the driver sees them. The driver bind-mounts
them into the container without needing to know about CSI.

## Where to look for common tasks

| I want to...                        | Look at                          |
|-------------------------------------|----------------------------------|
| Add a new task config field         | `config.go` (struct + HCL spec), then wire through `driver.go:StartTask` → `ContainerConfig` → `containerd.go:CreateContainer` |
| Add a new plugin config field       | `config.go` (struct + HCL spec), then parse in `driver.go:SetConfig` |
| Change how containers are created   | `containerd.go:CreateContainer`  |
| Change sandbox (VM) behavior        | `sandbox.go`                     |
| Fix DNS or /etc/hosts generation    | `driver.go` (bottom half)        |
| Fix mount handling                  | `driver.go:buildMounts`          |
| Change how stats are reported       | `stats.go`                       |
| Add a new containerd operation      | Add to `Containerd` interface, implement in `containerdClient`, add stub to `recorder_test.go` |
| Understand Docker auth              | `containerd.go` (bottom section) |
