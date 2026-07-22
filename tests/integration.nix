# Sudo-based integration test. Starts its own containerd + Nomad under
# /tmp/kata-driver-test and runs the shared assertion body (tests/verify.nix)
# against them. Requires root (containerd/Nomad need it) and a usable /dev/kvm.
#
# The job specifications (tests/jobs.nix) and the assertion body
# (tests/verify.nix) are shared with the NixOS VM test (tests/integration-vm.nix).
{ pkgs, driverPkg }:

let
  jobs = import ./jobs.nix { inherit pkgs; };
  verify = import ./verify.nix { inherit pkgs; };

  containerdConfig = pkgs.writeText "containerd-test.toml" ''
    version = 3
    root = "/tmp/kata-driver-test/containerd"
    state = "/tmp/kata-driver-test/run"

    [grpc]
      address = "/tmp/kata-driver-test/containerd.sock"

    [plugins."io.containerd.cri.v1.runtime".containerd.runtimes."kata"]
      runtime_type = "io.containerd.kata.v2"
      privileged_without_host_devices = true
  '';

  nomadConfig = pkgs.writeText "nomad-test.hcl" ''
    log_level = "INFO"
    data_dir  = "/tmp/kata-driver-test/nomad"
    bind_addr = "127.0.0.1"

    ports {
      http = 14646
      rpc  = 14647
      serf = 14648
    }

    advertise {
      http = "127.0.0.1"
      rpc  = "127.0.0.1"
      serf = "127.0.0.1"
    }

    server {
      enabled          = true
      bootstrap_expect = 1
    }

    client {
      enabled  = true
      cni_path = "${pkgs.cni-plugins}/bin"
      cni_config_dir = "/tmp/kata-driver-test/cni"
    }

    plugin_dir = "/tmp/kata-driver-test/plugins"

    plugin "nomad-driver-kata" {
      config {
        containerd_addr = "/tmp/kata-driver-test/containerd.sock"
        namespace       = "default"
        pause_image     = "registry.k8s.io/pause:3.9"
        runtime         = "io.containerd.kata.v2"
      }
    }
  '';

in
pkgs.writeShellScriptBin "kata-driver-test" ''
  set -euo pipefail

  if [ "$(id -u)" -ne 0 ]; then
    echo "ERROR: must run as root (needs KVM + containerd)"
    exit 1
  fi

  # Runtime KVM probe. Kata boots each task in a real KVM microVM, so a usable
  # /dev/kvm is mandatory. When it is absent or unusable (e.g. a host or nested
  # environment without virtualization support), skip rather than fail — the
  # test simply cannot exercise Kata here, and that is not a driver defect.
  if [ ! -e /dev/kvm ] || [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then
    echo "=== Kata Driver Integration Test ==="
    echo ""
    echo "[SKIP] /dev/kvm is not available or not usable on this host."
    echo "       Kata requires KVM (nested virtualization when running inside a VM)"
    echo "       to boot task microVMs. Skipping integration test — not a failure."
    exit 0
  fi

  TESTDIR="/tmp/kata-driver-test"
  CONTAINERD_SOCK="$TESTDIR/containerd.sock"
  NOMAD_ADDR="http://127.0.0.1:14646"
  NOMAD_TOKEN=""
  NOMAD_NAMESPACE=""
  unset NOMAD_TOKEN NOMAD_NAMESPACE

  export NOMAD_ADDR

  remove_testdir() {
    if [ ! -e "$TESTDIR" ]; then
      echo "[OK] no previous test directory"
      return
    fi

    echo "Removing previous test directory: $TESTDIR"
    while IFS= read -r mountpoint; do
      case "$mountpoint" in
        "$TESTDIR"/*)
          echo "Unmounting leftover mount: $mountpoint"
          ${pkgs.util-linux}/bin/umount -l "$mountpoint" >/dev/null 2>&1 || true
          ;;
      esac
    done < <(${pkgs.util-linux}/bin/findmnt --kernel --list --noheadings --output TARGET 2>/dev/null || true)

    if rm -rf "$TESTDIR"; then
      echo "[OK] removed previous test directory"
    else
      echo "[FAIL] failed to remove previous test directory: $TESTDIR"
      return 1
    fi
  }

  cleanup() {
    set +e
    echo ""
    echo "=== Cleaning up ==="

    if [ -f "$TESTDIR/nomad.pid" ]; then
      ${pkgs.nomad}/bin/nomad job stop -purge -detach kata-driver-test >/dev/null 2>&1 || true
      ${pkgs.nomad}/bin/nomad job stop -purge -detach kata-multi-vm >/dev/null 2>&1 || true
      sleep 3
    fi

    if [ -S "$CONTAINERD_SOCK" ]; then
      for task in $(${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" task ls -q 2>/dev/null); do
        ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" task kill "$task" >/dev/null 2>&1 || true
        ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" task rm "$task" >/dev/null 2>&1 || true
      done
      for container in $(${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" container ls -q 2>/dev/null); do
        ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" container rm "$container" >/dev/null 2>&1 || true
      done
    fi

    if [ -f "$TESTDIR/nomad.pid" ]; then
      kill "$(cat "$TESTDIR/nomad.pid")" 2>/dev/null || true
      wait "$(cat "$TESTDIR/nomad.pid")" 2>/dev/null || true
    fi
    if [ -f "$TESTDIR/containerd.pid" ]; then
      kill "$(cat "$TESTDIR/containerd.pid")" 2>/dev/null || true
      wait "$(cat "$TESTDIR/containerd.pid")" 2>/dev/null || true
    fi

    remove_testdir
    echo "Done."
  }
  trap cleanup EXIT

  echo "=== Kata Driver Integration Test ==="
  echo ""

  # Kill stale test Nomad from a previous interrupted run (not the system Nomad)
  if [ -f "$TESTDIR/nomad.pid" ]; then
    kill "$(cat "$TESTDIR/nomad.pid")" 2>/dev/null || true
    sleep 1
  fi

  # Prep dirs
  echo "Preparing test directory..."
  remove_testdir
  mkdir -p "$TESTDIR"/{containerd,run,nomad,plugins,cni}
  echo "[OK] test directory ready: $TESTDIR"

  # Symlink plugin binary
  ln -sf ${driverPkg}/bin/nomad-driver-kata "$TESTDIR/plugins/nomad-driver-kata"

  # Load kernel modules
  modprobe vhost_vsock 2>/dev/null || true
  modprobe vhost_net 2>/dev/null || true
  modprobe tun 2>/dev/null || true

  # Start containerd
  echo "Starting containerd..."
  PATH="${pkgs.kata-runtime}/bin:$PATH" \
    ${pkgs.containerd}/bin/containerd \
      --config ${containerdConfig} \
      &>"$TESTDIR/containerd.log" &
  echo $! > "$TESTDIR/containerd.pid"

  # Wait for containerd
  for i in $(seq 1 30); do
    if ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" version &>/dev/null; then
      break
    fi
    sleep 0.5
  done
  ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" version >/dev/null || {
    echo "ERROR: containerd failed to start"
    cat "$TESTDIR/containerd.log"
    exit 1
  }
  echo "[OK] containerd running"

  CACHE_DIR="/var/cache/kata-test"
  echo "Loading images..."
  if [ -f "$CACHE_DIR/pause.tar" ]; then
    ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" image import "$CACHE_DIR/pause.tar" >/dev/null
  else
    echo "  pulling registry.k8s.io/pause:3.9 (no local cache)..."
    ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" image pull registry.k8s.io/pause:3.9 >/dev/null
    mkdir -p "$CACHE_DIR"
    ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" image export "$CACHE_DIR/pause.tar" registry.k8s.io/pause:3.9
  fi
  if [ -f "$CACHE_DIR/busybox.tar" ]; then
    ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" image import "$CACHE_DIR/busybox.tar" >/dev/null
  else
    echo "  pulling docker.io/library/busybox:latest (no local cache)..."
    ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" image pull docker.io/library/busybox:latest >/dev/null
    mkdir -p "$CACHE_DIR"
    ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" image export "$CACHE_DIR/busybox.tar" docker.io/library/busybox:latest
  fi
  echo "[OK] images ready"

  # Start Nomad in dev mode
  echo "Starting Nomad..."
  PATH="${pkgs.iptables}/bin:${pkgs.cni-plugins}/bin:$PATH" \
    ${pkgs.nomad}/bin/nomad agent \
      -config=${nomadConfig} \
      -bind=127.0.0.1 \
      -acl-enabled=false \
      &>"$TESTDIR/nomad.log" &
  echo $! > "$TESTDIR/nomad.pid"

  # Wait for Nomad
  for i in $(seq 1 30); do
    if ${pkgs.nomad}/bin/nomad node status -address="$NOMAD_ADDR" &>/dev/null; then
      break
    fi
    sleep 1
  done
  ${pkgs.nomad}/bin/nomad node status -address="$NOMAD_ADDR" >/dev/null || {
    echo "ERROR: Nomad failed to start"
    tail -50 "$TESTDIR/nomad.log"
    exit 1
  }
  echo "[OK] Nomad running"

  # Hand off to the shared assertion body. It talks to Nomad over NOMAD_ADDR and
  # containerd over CONTAINERD_SOCK, submits the shared jobs, and asserts driver
  # behaviour. NOMAD_LOG gives it this script's Nomad log for failure diagnostics.
  export CONTAINERD_SOCK
  export SINGLE_JOB=${jobs.single}
  export MULTI_VM_JOB=${jobs.multiVm}
  export NOMAD_LOG="$TESTDIR/nomad.log"
  ${verify}
''
