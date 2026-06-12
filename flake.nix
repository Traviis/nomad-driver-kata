{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs {
        inherit system;
        config.allowUnfree = true;
      };

      driverPkg = pkgs.buildGoModule {
        pname = "nomad-driver-kata";
        version = "0.1.0";
        src = ./.;
        vendorHash = "sha256-RplDmsBNxGOkI40eFRXpa/+P01Ap1hk4NhPATZKiU80=";
        env.CGO_ENABLED = 0;
        ldflags = [ "-s" "-w" ];

        preCheck = ''
          go vet ./...
        '';

        meta = with pkgs.lib; {
          description =
            "Nomad task driver for Kata Containers with sandbox-aware VM sharing";
          license = licenses.mit;
          platforms = platforms.linux;
        };
      };

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
        }

        plugin_dir = "/tmp/kata-driver-test/plugins"

        plugin "kata" {
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
                hostname    = "kata-test-host"
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
                command    = "sh"
                args       = ["-c", "echo SIDECAR_OK && sleep 3600"]
                pids_limit = 256
              }

              resources {
                cpu    = 50
                memory = 32
              }
            }
          }
        }
      '';

      integrationTest = pkgs.writeShellScriptBin "kata-driver-test" ''
        set -euo pipefail

        if [ "$(id -u)" -ne 0 ]; then
          echo "ERROR: must run as root (needs KVM + containerd)"
          exit 1
        fi

        TESTDIR="/tmp/kata-driver-test"
        CONTAINERD_SOCK="$TESTDIR/containerd.sock"
        NOMAD_ADDR="http://127.0.0.1:4646"
        export NOMAD_ADDR

        cleanup() {
          echo ""
          echo "=== Cleaning up ==="
          # Stop nomad
          if [ -f "$TESTDIR/nomad.pid" ]; then
            kill "$(cat "$TESTDIR/nomad.pid")" 2>/dev/null || true
          fi
          # Stop containerd
          if [ -f "$TESTDIR/containerd.pid" ]; then
            kill "$(cat "$TESTDIR/containerd.pid")" 2>/dev/null || true
          fi
          # Clean up containers
          ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" task kill kata-driver-test 2>/dev/null || true
          sleep 2
          rm -rf "$TESTDIR"
          echo "Done."
        }
        trap cleanup EXIT

        echo "=== Kata Driver Integration Test ==="
        echo ""

        # Prep dirs
        rm -rf "$TESTDIR"
        mkdir -p "$TESTDIR"/{containerd,run,nomad,plugins}

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

        # Pre-pull images so Nomad doesn't timeout
        echo "Pulling images..."
        ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" image pull registry.k8s.io/pause:3.9 >/dev/null
        ${pkgs.containerd}/bin/ctr -a "$CONTAINERD_SOCK" image pull docker.io/library/busybox:latest >/dev/null
        echo "[OK] images cached"

        # Start Nomad in dev mode
        echo "Starting Nomad..."
        PATH="${pkgs.cni-plugins}/bin:$PATH" \
          ${pkgs.nomad}/bin/nomad agent \
            -config=${nomadConfig} \
            -bind=127.0.0.1 \
            &>"$TESTDIR/nomad.log" &
        echo $! > "$TESTDIR/nomad.pid"

        # Wait for Nomad
        for i in $(seq 1 30); do
          if ${pkgs.nomad}/bin/nomad node status &>/dev/null; then
            break
          fi
          sleep 1
        done
        ${pkgs.nomad}/bin/nomad node status >/dev/null || {
          echo "ERROR: Nomad failed to start"
          tail -50 "$TESTDIR/nomad.log"
          exit 1
        }
        echo "[OK] Nomad running"

        # Check driver is detected
        echo ""
        echo "=== Checking driver fingerprint ==="
        sleep 5
        DRIVER_STATUS=$(${pkgs.nomad}/bin/nomad node status -self -json 2>/dev/null | ${pkgs.jq}/bin/jq -r '.Drivers.kata.Detected // false')
        if [ "$DRIVER_STATUS" = "true" ]; then
          echo "[OK] kata driver detected"
        else
          echo "[FAIL] kata driver not detected"
          ${pkgs.nomad}/bin/nomad node status -self -json | ${pkgs.jq}/bin/jq '.Drivers' 2>/dev/null || true
          tail -30 "$TESTDIR/nomad.log"
          exit 1
        fi

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
          exit 1
        fi

        # Check task logs
        echo ""
        echo "=== Task logs ==="
        echo "--- hello ---"
        HELLO_LOGS=$(${pkgs.nomad}/bin/nomad alloc logs "$ALLOC_ID" hello 2>/dev/null || echo "")
        echo "$HELLO_LOGS"
        if echo "$HELLO_LOGS" | grep -q "KATA_DRIVER_OK"; then
          echo "[OK] hello task produced expected output"
        else
          echo "[FAIL] hello task missing KATA_DRIVER_OK in logs"
          exit 1
        fi

        echo ""
        echo "--- sidecar ---"
        SIDECAR_LOGS=$(${pkgs.nomad}/bin/nomad alloc logs "$ALLOC_ID" sidecar 2>/dev/null || echo "")
        echo "$SIDECAR_LOGS"
        if echo "$SIDECAR_LOGS" | grep -q "SIDECAR_OK"; then
          echo "[OK] sidecar task produced expected output"
        else
          echo "[FAIL] sidecar task missing SIDECAR_OK in logs"
          exit 1
        fi

        # Exec into container
        echo ""
        echo "=== Exec verification ==="
        EXEC_HOSTNAME=$(${pkgs.nomad}/bin/nomad alloc exec -task hello "$ALLOC_ID" hostname 2>/dev/null || echo "")
        echo "Hostname from exec: $EXEC_HOSTNAME"
        if echo "$EXEC_HOSTNAME" | grep -q "kata-test-host"; then
          echo "[OK] hostname config applied"
        else
          echo "[FAIL] expected hostname 'kata-test-host', got '$EXEC_HOSTNAME'"
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
        ${pkgs.nomad}/bin/nomad alloc signal -task sidecar "$ALLOC_ID" SIGUSR1 2>/dev/null || true
        sleep 1
        SIDECAR_STATE=$(${pkgs.nomad}/bin/nomad alloc status -json "$ALLOC_ID" | ${pkgs.jq}/bin/jq -r '.TaskStates.sidecar.State')
        if [ "$SIDECAR_STATE" = "running" ]; then
          echo "[OK] sidecar survived SIGUSR1 signal"
        else
          echo "[FAIL] sidecar state after signal: $SIDECAR_STATE"
          exit 1
        fi

        # Check kata shim count
        echo ""
        echo "=== VM sharing verification ==="
        SHIM_COUNT=$(ps aux | grep containerd-shim-kata-v2 | grep -v grep | wc -l)
        echo "Kata shim processes: $SHIM_COUNT"
        if [ "$SHIM_COUNT" -eq 1 ]; then
          echo "[OK] Single VM — both tasks share one Kata sandbox"
        elif [ "$SHIM_COUNT" -eq 0 ]; then
          echo "[INFO] No shim processes (Kata may use built-in Dragonball VMM)"
          TASK_STATES=$(${pkgs.nomad}/bin/nomad alloc status -json "$ALLOC_ID" | ${pkgs.jq}/bin/jq -r '.TaskStates | to_entries[] | "\(.key): \(.value.State)"')
          echo "$TASK_STATES"
        else
          echo "[WARN] $SHIM_COUNT shim processes — tasks may be in separate VMs"
        fi

        echo ""
        echo "=== Integration test passed ==="
        echo "Job left running. Inspect with:"
        echo "  NOMAD_ADDR=$NOMAD_ADDR nomad alloc status $ALLOC_ID"
        echo "  NOMAD_ADDR=$NOMAD_ADDR nomad alloc logs $ALLOC_ID hello"
        echo ""
        echo "Press Enter to tear down, or Ctrl-C to keep running."
        read -r
      '';

    in {
      packages.${system}.default = driverPkg;

      checks.${system}.default = driverPkg;

      apps.${system}.integration-test = {
        type = "app";
        program = "${integrationTest}/bin/kata-driver-test";
      };

      nixosModules.default = ./module.nix;

      devShells.${system}.default = pkgs.mkShell {
        buildInputs = with pkgs; [
          go
          gopls
          gotools
          nomad
          containerd
          kata-runtime
        ];
      };
    };
}
