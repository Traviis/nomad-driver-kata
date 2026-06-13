{ pkgs, driverPkg }:

let
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

  testJob = pkgs.writeText "test-job.nomad.hcl" ''
    job "kata-driver-test" {
      type        = "batch"
      datacenters = ["dc1"]

      group "test" {
        task "hello" {
          driver = "kata"

          config {
            image       = "docker.io/library/busybox:latest"
            command     = "sh"
            args        = ["-c", "echo KATA_DRIVER_OK && cat /proc/version && sleep 60"]
            extra_hosts = ["mydb:10.0.0.5", "cache:10.0.0.6"]
          }

          resources {
            cpu    = 100
            memory = 64
          }
        }

        task "sidecar" {
          driver = "kata"

          lifecycle {
            hook    = "prestart"
            sidecar = true
          }

          config {
            image      = "docker.io/library/busybox:latest"
            pids_limit = 256
            command    = "sh"
            args       = ["-c", "echo SIDECAR_OK; sleep 3600"]
          }

          resources {
            cpu    = 50
            memory = 32
          }
        }
      }
    }
  '';

  multiVmJob = pkgs.writeText "multi-vm-job.nomad.hcl" ''
    job "kata-multi-vm" {
      type        = "batch"
      datacenters = ["dc1"]

      group "server" {
        network {
          mode = "bridge"
          port "http" {
            to = 8080
          }
        }

        task "web" {
          driver = "kata"
          config {
            image   = "docker.io/library/busybox:latest"
            command = "sh"
            args    = ["-c", "echo WEB_OK && mkdir -p /www && echo SERVER_OK > /www/index.html && httpd -f -p 8080 -h /www"]
          }
          resources {
            cpu    = 100
            memory = 128
          }
        }

        task "web-sidecar" {
          driver = "kata"
          lifecycle {
            hook    = "prestart"
            sidecar = true
          }
          config {
            image   = "docker.io/library/busybox:latest"
            command = "sh"
            args    = ["-c", "echo WEB_SIDECAR_OK; sleep 3600"]
          }
          resources {
            cpu    = 50
            memory = 64
          }
        }
      }

      group "client" {
        network {
          mode = "bridge"
        }

        task "fetcher" {
          driver = "kata"
          config {
            image   = "docker.io/library/busybox:latest"
            command = "sh"
            args    = ["-c", "echo FETCHER_OK; sleep 3600"]
          }
          resources {
            cpu    = 100
            memory = 128
          }
        }

        task "fetcher-sidecar" {
          driver = "kata"
          lifecycle {
            hook    = "prestart"
            sidecar = true
          }
          config {
            image   = "docker.io/library/busybox:latest"
            command = "sh"
            args    = ["-c", "echo FETCHER_SIDECAR_OK; sleep 3600"]
          }
          resources {
            cpu    = 50
            memory = 64
          }
        }
      }
    }
  '';

in pkgs.writeShellScriptBin "kata-driver-test" ''
  set -euo pipefail

  if [ "$(id -u)" -ne 0 ]; then
    echo "ERROR: must run as root (needs KVM + containerd)"
    exit 1
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

  # Check driver is detected
  echo ""
  echo "=== Checking driver fingerprint ==="
  sleep 5
  DRIVER_STATUS=$(${pkgs.nomad}/bin/nomad node status -address="$NOMAD_ADDR" -self -json 2>/dev/null | ${pkgs.jq}/bin/jq -r '.Drivers.kata.Detected // false')
  if [ "$DRIVER_STATUS" = "true" ]; then
    echo "[OK] kata driver detected"
  else
    echo "[FAIL] kata driver not detected"
    ${pkgs.nomad}/bin/nomad node status -self -json | ${pkgs.jq}/bin/jq '.Drivers' 2>/dev/null || true
    tail -30 "$TESTDIR/nomad.log"
    exit 1
  fi

  echo ""
  echo "=== Verifying images in containerd ==="
  ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" image ls -q
  echo "[OK] images listed"

  # Submit test job
  echo ""
  echo "=== Submitting test job ==="
  ${pkgs.nomad}/bin/nomad job run ${testJob}

  # Wait for allocation
  echo "Waiting for allocation..."
  for i in $(seq 1 60); do
    ALLOC_STATUS=$(${pkgs.nomad}/bin/nomad job status -json kata-driver-test 2>/dev/null | ${pkgs.jq}/bin/jq -r '.[0].Allocations[0].ClientStatus // "pending"')
    if [ "$ALLOC_STATUS" = "running" ] || [ "$ALLOC_STATUS" = "complete" ]; then
      break
    fi
    sleep 2
  done

  ALLOC_ID=$(${pkgs.nomad}/bin/nomad job status -json kata-driver-test | ${pkgs.jq}/bin/jq -r '.[0].Allocations[0].ID')
  echo "Allocation: $ALLOC_ID (status: $ALLOC_STATUS)"

  if [ "$ALLOC_STATUS" != "running" ] && [ "$ALLOC_STATUS" != "complete" ]; then
    echo "[FAIL] allocation did not reach running state"
    ${pkgs.nomad}/bin/nomad alloc status "$ALLOC_ID" 2>/dev/null || true
    echo "--- nomad log tail ---"
    tail -100 "$TESTDIR/nomad.log" || true
    exit 1
  fi

  # Check task logs (retry — logs may take a moment to appear)
  echo ""
  echo "=== Task logs ==="
  echo "--- hello ---"
  HELLO_LOGS=""
  for i in $(seq 1 15); do
    HELLO_LOGS=$(${pkgs.nomad}/bin/nomad alloc logs "$ALLOC_ID" hello 2>/dev/null || echo "")
    if echo "$HELLO_LOGS" | grep -q "KATA_DRIVER_OK"; then
      break
    fi
    sleep 2
  done
  echo "$HELLO_LOGS"
  if echo "$HELLO_LOGS" | grep -q "KATA_DRIVER_OK"; then
    echo "[OK] hello task produced expected output"
  else
    echo "[FAIL] hello task missing KATA_DRIVER_OK in logs"
    ${pkgs.nomad}/bin/nomad alloc status "$ALLOC_ID" 2>/dev/null || true
    exit 1
  fi

  echo ""
  echo "--- sidecar ---"
  SIDECAR_LOGS=""
  for i in $(seq 1 15); do
    SIDECAR_LOGS=$(${pkgs.nomad}/bin/nomad alloc logs "$ALLOC_ID" sidecar 2>/dev/null || echo "")
    if echo "$SIDECAR_LOGS" | grep -q "SIDECAR_OK"; then
      break
    fi
    sleep 2
  done
  echo "$SIDECAR_LOGS"
  if echo "$SIDECAR_LOGS" | grep -q "SIDECAR_OK"; then
    echo "[OK] sidecar task produced expected output"
  else
    echo "[FAIL] sidecar task missing SIDECAR_OK in logs"
    ${pkgs.nomad}/bin/nomad alloc status "$ALLOC_ID" 2>/dev/null || true
    exit 1
  fi

  # Exec into container
  echo ""
  echo "=== Exec verification ==="
  EXEC_HOSTNAME=$(${pkgs.nomad}/bin/nomad alloc exec -task hello "$ALLOC_ID" hostname 2>/dev/null || echo "")
  echo "Hostname from exec: $EXEC_HOSTNAME"
  if [ "$EXEC_HOSTNAME" = "test" ]; then
    echo "[OK] sandbox hostname inherited (group name 'test')"
  else
    echo "[FAIL] expected sandbox hostname 'test', got '$EXEC_HOSTNAME'"
    exit 1
  fi

  EXEC_HOSTS=$(${pkgs.nomad}/bin/nomad alloc exec -task hello "$ALLOC_ID" cat /etc/hosts 2>/dev/null || echo "")
  echo "Hosts file:"
  echo "$EXEC_HOSTS"
  if echo "$EXEC_HOSTS" | grep -q "mydb" && echo "$EXEC_HOSTS" | grep -q "cache"; then
    echo "[OK] extra_hosts entries present"
  else
    echo "[FAIL] extra_hosts entries missing from /etc/hosts"
    exit 1
  fi

  # Signal test
  echo ""
  echo "=== Signal verification ==="
  ${pkgs.nomad}/bin/nomad alloc signal -s SIGCONT -task sidecar "$ALLOC_ID" 2>/dev/null || {
    echo "[FAIL] nomad alloc signal failed"
    exit 1
  }
  sleep 1
  SIDECAR_STATE=$(${pkgs.nomad}/bin/nomad alloc status -json "$ALLOC_ID" | ${pkgs.jq}/bin/jq -r '.TaskStates.sidecar.State')
  if [ "$SIDECAR_STATE" = "running" ]; then
    echo "[OK] sidecar survived SIGCONT signal"
  else
    echo "[FAIL] sidecar state after signal: $SIDECAR_STATE"
    ${pkgs.nomad}/bin/nomad alloc status "$ALLOC_ID" 2>/dev/null || true
    exit 1
  fi

  # Verify VM sharing via hostname — both tasks should see the sandbox hostname
  echo ""
  echo "=== VM sharing verification ==="
  SIDECAR_HOSTNAME=$(${pkgs.nomad}/bin/nomad alloc exec -task sidecar "$ALLOC_ID" hostname 2>/dev/null || echo "")
  echo "hello hostname:   $EXEC_HOSTNAME"
  echo "sidecar hostname: $SIDECAR_HOSTNAME"
  if [ "$EXEC_HOSTNAME" = "$SIDECAR_HOSTNAME" ]; then
    echo "[OK] both tasks share sandbox hostname — same Kata VM"
  else
    echo "[FAIL] hostnames differ — tasks may be in separate VMs"
    exit 1
  fi

  # Stop Phase 1 job to free resources for Phase 2
  echo ""
  echo "========================================="
  echo "=== Phase 2: Multi-VM Networking ==="
  echo "========================================="
  ${pkgs.nomad}/bin/nomad job stop -purge -detach kata-driver-test >/dev/null 2>&1 || true
  sleep 5

  echo ""
  echo "=== Submitting multi-VM job ==="
  ${pkgs.nomad}/bin/nomad job run -detach ${multiVmJob}

  echo "Waiting for allocations..."
  SERVER_STATUS="pending"
  CLIENT_STATUS="pending"
  for i in $(seq 1 90); do
    if [ "$SERVER_STATUS" != "running" ]; then
      SERVER_STATUS=$(${pkgs.nomad}/bin/nomad job status -json kata-multi-vm 2>/dev/null | ${pkgs.jq}/bin/jq -r '[.[0].Allocations[] | select(.TaskGroup == "server")][0].ClientStatus // "pending"') || true
    fi
    if [ "$CLIENT_STATUS" != "running" ]; then
      CLIENT_STATUS=$(${pkgs.nomad}/bin/nomad job status -json kata-multi-vm 2>/dev/null | ${pkgs.jq}/bin/jq -r '[.[0].Allocations[] | select(.TaskGroup == "client")][0].ClientStatus // "pending"') || true
    fi
    if [ "$SERVER_STATUS" = "running" ] && [ "$CLIENT_STATUS" = "running" ]; then break; fi
    sleep 2
  done

  SERVER_ALLOC=$(${pkgs.nomad}/bin/nomad job status -json kata-multi-vm | ${pkgs.jq}/bin/jq -r '[.[0].Allocations[] | select(.TaskGroup == "server")][0].ID')
  CLIENT_ALLOC=$(${pkgs.nomad}/bin/nomad job status -json kata-multi-vm | ${pkgs.jq}/bin/jq -r '[.[0].Allocations[] | select(.TaskGroup == "client")][0].ID')
  echo "Server: $SERVER_ALLOC ($SERVER_STATUS)"
  echo "Client: $CLIENT_ALLOC ($CLIENT_STATUS)"
  if [ "$SERVER_STATUS" != "running" ]; then
    echo "[FAIL] server allocation not running (status: $SERVER_STATUS)"
    ${pkgs.nomad}/bin/nomad alloc status "$SERVER_ALLOC" 2>/dev/null || true
    echo "--- nomad log tail ---"
    tail -50 "$TESTDIR/nomad.log" 2>/dev/null || true
    exit 1
  fi
  if [ "$CLIENT_STATUS" != "running" ]; then
    echo "[FAIL] client allocation not running (status: $CLIENT_STATUS)"
    ${pkgs.nomad}/bin/nomad alloc status "$CLIENT_ALLOC" 2>/dev/null || true
    echo "--- nomad log tail ---"
    tail -50 "$TESTDIR/nomad.log" 2>/dev/null || true
    exit 1
  fi

  # VM isolation: different groups = different VMs
  echo ""
  echo "=== VM isolation ==="
  SERVER_HOSTNAME=$(${pkgs.nomad}/bin/nomad alloc exec -task web "$SERVER_ALLOC" hostname 2>/dev/null || echo "")
  CLIENT_HOSTNAME=$(${pkgs.nomad}/bin/nomad alloc exec -task fetcher "$CLIENT_ALLOC" hostname 2>/dev/null || echo "")
  echo "server VM: $SERVER_HOSTNAME"
  echo "client VM: $CLIENT_HOSTNAME"
  if [ -n "$SERVER_HOSTNAME" ] && [ -n "$CLIENT_HOSTNAME" ] && [ "$SERVER_HOSTNAME" != "$CLIENT_HOSTNAME" ]; then
    echo "[OK] different groups run in separate VMs"
  else
    echo "[FAIL] expected different hostnames for different VMs"
    exit 1
  fi

  # VM sharing: tasks within group share VM
  echo ""
  echo "=== Intra-group VM sharing ==="
  WEB_SIDECAR_HOSTNAME=$(${pkgs.nomad}/bin/nomad alloc exec -task web-sidecar "$SERVER_ALLOC" hostname 2>/dev/null || echo "")
  FETCHER_SIDECAR_HOSTNAME=$(${pkgs.nomad}/bin/nomad alloc exec -task fetcher-sidecar "$CLIENT_ALLOC" hostname 2>/dev/null || echo "")
  echo "web + web-sidecar:         $SERVER_HOSTNAME / $WEB_SIDECAR_HOSTNAME"
  echo "fetcher + fetcher-sidecar: $CLIENT_HOSTNAME / $FETCHER_SIDECAR_HOSTNAME"
  if [ "$SERVER_HOSTNAME" = "$WEB_SIDECAR_HOSTNAME" ] && [ "$CLIENT_HOSTNAME" = "$FETCHER_SIDECAR_HOSTNAME" ]; then
    echo "[OK] tasks within each group share a VM"
  else
    echo "[FAIL] tasks within a group have different hostnames"
    exit 1
  fi

  # Cross-VM networking
  echo ""
  echo "=== Cross-VM networking ==="
  SERVER_IP=$(${pkgs.nomad}/bin/nomad alloc exec -task web "$SERVER_ALLOC" ip addr 2>/dev/null | sed -n 's/.*inet \([0-9.]*\)\/.*scope global.*/\1/p') || true
  echo "Server bridge IP: $SERVER_IP"

  if [ -z "$SERVER_IP" ] || [ "$SERVER_IP" = "null" ]; then
    echo "[FAIL] could not determine server bridge IP"
    ${pkgs.nomad}/bin/nomad alloc status -json "$SERVER_ALLOC" | ${pkgs.jq}/bin/jq '.AllocatedResources.Shared' 2>/dev/null || true
    exit 1
  fi

  RESPONSE=""
  for i in $(seq 1 15); do
    RESPONSE=$(${pkgs.nomad}/bin/nomad alloc exec -task fetcher "$CLIENT_ALLOC" wget -q -O - "http://$SERVER_IP:8080/" 2>/dev/null || echo "")
    if echo "$RESPONSE" | grep -q "SERVER_OK"; then break; fi
    sleep 2
  done
  echo "Response: $RESPONSE"
  if echo "$RESPONSE" | grep -q "SERVER_OK"; then
    echo "[OK] cross-VM HTTP request succeeded via bridge"
  else
    echo "[FAIL] could not reach server from client VM"
    echo "--- diagnostics ---"
    echo "Server network:"
    ${pkgs.nomad}/bin/nomad alloc exec -task web "$SERVER_ALLOC" ip addr 2>/dev/null || true
    echo "Client network:"
    ${pkgs.nomad}/bin/nomad alloc exec -task fetcher "$CLIENT_ALLOC" ip addr 2>/dev/null || true
    ${pkgs.nomad}/bin/nomad alloc exec -task fetcher "$CLIENT_ALLOC" ip route 2>/dev/null || true
    exit 1
  fi

  echo ""
  echo "=== All integration tests passed ==="
''
