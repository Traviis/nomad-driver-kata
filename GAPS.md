# Differences from Nomad's Docker driver

This driver aims to be a drop-in replacement for `driver = "docker"` when
running workloads under Kata Containers. The core workflow (pull image,
create container, run task, collect logs, exec, signal, destroy) is
functionally equivalent. The gaps below are inherent to the Kata/containerd
architecture, inapplicable Docker concepts, or not yet implemented.

## Architectural differences

These cannot be closed — they follow from running inside a microVM.

- **Host networking (`network_mode = "host"`)** — Kata VMs have their own
  kernel. Host-mode networking is not possible; all networking goes through
  a bridged CNI namespace. Use Nomad's `bridge` network mode.

- **Host PID/IPC namespace** — Same reason: the VM has its own PID and IPC
  namespaces. `pid_mode = "host"` and `ipc_mode = "host"` are not
  supported.

- **`shm_size`** — Kata VMs have their own `/dev/shm` sized by the guest
  kernel defaults. The host cannot control this from outside the VM.

- **`mac_address`** — Kata VM networking is managed by the hypervisor; MAC
  addresses are assigned by the CNI plugin, not the container runtime.

## Not applicable

Docker-specific concepts that do not map to containerd or Kata.

- **Docker logging drivers** — Docker supports `json-file`, `syslog`,
  `journald`, etc. This driver uses Nomad's built-in log capture
  (stdout/stderr written to the alloc's `logs/` directory), which is the
  standard approach for Nomad drivers.

- **`storage_opt`** — Docker storage driver options. Containerd uses a
  snapshotter model with no equivalent knobs.

- **Docker health checks** — Docker's native `HEALTHCHECK` instruction.
  Use Nomad's `check` blocks in the `service` stanza instead.

- **`entrypoint`** — Docker distinguishes `ENTRYPOINT` from `CMD`. This
  driver has `command` and `args` which override the image's process args.
  If neither is set, the image default is used. This matches how
  containerd's OCI spec works.

## Not yet implemented

These could be added if needed.

- **Device passthrough** — Docker's `devices` option maps host devices into
- **Device passthrough** — Docker's `devices` option maps host devices into
  the container. Kata supports device passthrough via VFIO but this driver
  does not yet expose it.
