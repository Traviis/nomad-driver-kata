# Environment-agnostic assertion body shared by both the sudo-based integration
# script (tests/integration.nix) and the NixOS VM test (tests/integration-vm.nix).
#
# This is the part of the integration test that does NOT care how containerd and
# Nomad were started. It talks to Nomad over $NOMAD_ADDR and to containerd over
# $CONTAINERD_SOCK, submits the shared jobs, and asserts driver behaviour.
#
# Required environment at call time:
#   NOMAD_ADDR       - http address of the Nomad agent (e.g. http://127.0.0.1:14646)
#   CONTAINERD_SOCK  - path to the containerd socket
#   SINGLE_JOB       - path to the single-VM job HCL   (tests/jobs.nix .single)
#   MULTI_VM_JOB     - path to the multi-VM job HCL     (tests/jobs.nix .multiVm)
# Optional:
#   NOMAD_LOG        - path to a Nomad log file for failure diagnostics; when
#                      unset (e.g. journald-based VM), log tails are skipped.
{ pkgs }:
pkgs.writeShellScript "kata-verify" ''
  set -euo pipefail

  export PATH="${
    pkgs.lib.makeBinPath [
      pkgs.nomad
      pkgs.containerd
      pkgs.jq
    ]
  }:$PATH"

  : "''${NOMAD_ADDR:?NOMAD_ADDR must be set}"
  : "''${CONTAINERD_SOCK:?CONTAINERD_SOCK must be set}"
  : "''${SINGLE_JOB:?SINGLE_JOB must be set}"
  : "''${MULTI_VM_JOB:?MULTI_VM_JOB must be set}"
  export NOMAD_ADDR

  # Optional Nomad log for diagnostics; empty when the caller uses journald.
  NOMAD_LOG="''${NOMAD_LOG:-}"
  log_tail() {
    # $1 = number of lines. No-op when NOMAD_LOG is unset or missing.
    if [ -n "$NOMAD_LOG" ] && [ -f "$NOMAD_LOG" ]; then
      tail -"$1" "$NOMAD_LOG" 2>/dev/null || true
    fi
  }

  # Check driver is detected
  echo ""
  echo "=== Checking driver fingerprint ==="
  sleep 5
  DRIVER_STATUS=$(nomad node status -address="$NOMAD_ADDR" -self -json 2>/dev/null | jq -r '.Drivers.kata.Detected // false')
  if [ "$DRIVER_STATUS" = "true" ]; then
    echo "[OK] kata driver detected"
  else
    echo "[FAIL] kata driver not detected"
    nomad node status -self -json | jq '.Drivers' 2>/dev/null || true
    log_tail 30
    exit 1
  fi

  echo ""
  echo "=== Verifying images in containerd ==="
  ctr -a "$CONTAINERD_SOCK" image ls -q
  echo "[OK] images listed"

  # Submit test job
  echo ""
  echo "=== Submitting test job ==="
  nomad job run "$SINGLE_JOB"

  # Wait for allocation
  echo "Waiting for allocation..."
  for i in $(seq 1 60); do
    ALLOC_STATUS=$(nomad job status -json kata-driver-test 2>/dev/null | jq -r '.[0].Allocations[0].ClientStatus // "pending"')
    if [ "$ALLOC_STATUS" = "running" ] || [ "$ALLOC_STATUS" = "complete" ]; then
      break
    fi
    sleep 2
  done

  ALLOC_ID=$(nomad job status -json kata-driver-test | jq -r '.[0].Allocations[0].ID')
  echo "Allocation: $ALLOC_ID (status: $ALLOC_STATUS)"

  if [ "$ALLOC_STATUS" != "running" ] && [ "$ALLOC_STATUS" != "complete" ]; then
    echo "[FAIL] allocation did not reach running state"
    nomad alloc status "$ALLOC_ID" 2>/dev/null || true
    echo "--- nomad log tail ---"
    log_tail 100
    exit 1
  fi

  # Check task logs (retry — logs may take a moment to appear)
  echo ""
  echo "=== Task logs ==="
  echo "--- hello ---"
  HELLO_LOGS=""
  for i in $(seq 1 15); do
    HELLO_LOGS=$(nomad alloc logs "$ALLOC_ID" hello 2>/dev/null || echo "")
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
    nomad alloc status "$ALLOC_ID" 2>/dev/null || true
    exit 1
  fi

  echo ""
  echo "--- sidecar ---"
  SIDECAR_LOGS=""
  for i in $(seq 1 15); do
    SIDECAR_LOGS=$(nomad alloc logs "$ALLOC_ID" sidecar 2>/dev/null || echo "")
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
    nomad alloc status "$ALLOC_ID" 2>/dev/null || true
    exit 1
  fi

  # Exec into container
  echo ""
  echo "=== Exec verification ==="
  EXEC_HOSTNAME=$(nomad alloc exec -i=false -t=false -task hello "$ALLOC_ID" /bin/hostname 2>/dev/null || echo "")
  echo "Hostname from exec: $EXEC_HOSTNAME"
  if [ "$EXEC_HOSTNAME" = "test" ]; then
    echo "[OK] sandbox hostname inherited (group name 'test')"
  else
    echo "[FAIL] expected sandbox hostname 'test', got '$EXEC_HOSTNAME'"
    exit 1
  fi

  EXEC_HOSTS=$(nomad alloc exec -i=false -t=false -task hello "$ALLOC_ID" /bin/cat /etc/hosts 2>/dev/null || echo "")
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
  nomad alloc signal -s SIGCONT -task sidecar "$ALLOC_ID" 2>/dev/null || {
    echo "[FAIL] nomad alloc signal failed"
    exit 1
  }
  sleep 1
  SIDECAR_STATE=$(nomad alloc status -json "$ALLOC_ID" | jq -r '.TaskStates.sidecar.State')
  if [ "$SIDECAR_STATE" = "running" ]; then
    echo "[OK] sidecar survived SIGCONT signal"
  else
    echo "[FAIL] sidecar state after signal: $SIDECAR_STATE"
    nomad alloc status "$ALLOC_ID" 2>/dev/null || true
    exit 1
  fi

  # Verify VM sharing via hostname — both tasks should see the sandbox hostname
  echo ""
  echo "=== VM sharing verification ==="
  SIDECAR_HOSTNAME=$(nomad alloc exec -i=false -t=false -task sidecar "$ALLOC_ID" /bin/hostname 2>/dev/null || echo "")
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
  nomad job stop -purge -detach kata-driver-test >/dev/null 2>&1 || true
  sleep 5

  echo ""
  echo "=== Submitting multi-VM job ==="
  nomad job run -detach "$MULTI_VM_JOB"

  echo "Waiting for allocations..."
  SERVER_STATUS="pending"
  CLIENT_STATUS="pending"
  for i in $(seq 1 90); do
    if [ "$SERVER_STATUS" != "running" ]; then
      SERVER_STATUS=$(nomad job status -json kata-multi-vm 2>/dev/null | jq -r '[.[0].Allocations[] | select(.TaskGroup == "server")][0].ClientStatus // "pending"') || true
    fi
    if [ "$CLIENT_STATUS" != "running" ]; then
      CLIENT_STATUS=$(nomad job status -json kata-multi-vm 2>/dev/null | jq -r '[.[0].Allocations[] | select(.TaskGroup == "client")][0].ClientStatus // "pending"') || true
    fi
    if [ "$SERVER_STATUS" = "running" ] && [ "$CLIENT_STATUS" = "running" ]; then break; fi
    sleep 2
  done

  SERVER_ALLOC=$(nomad job status -json kata-multi-vm | jq -r '[.[0].Allocations[] | select(.TaskGroup == "server")][0].ID')
  CLIENT_ALLOC=$(nomad job status -json kata-multi-vm | jq -r '[.[0].Allocations[] | select(.TaskGroup == "client")][0].ID')
  echo "Server: $SERVER_ALLOC ($SERVER_STATUS)"
  echo "Client: $CLIENT_ALLOC ($CLIENT_STATUS)"
  if [ "$SERVER_STATUS" != "running" ]; then
    echo "[FAIL] server allocation not running (status: $SERVER_STATUS)"
    nomad alloc status "$SERVER_ALLOC" 2>/dev/null || true
    echo "--- nomad log tail ---"
    log_tail 50
    exit 1
  fi
  if [ "$CLIENT_STATUS" != "running" ]; then
    echo "[FAIL] client allocation not running (status: $CLIENT_STATUS)"
    nomad alloc status "$CLIENT_ALLOC" 2>/dev/null || true
    echo "--- nomad log tail ---"
    log_tail 50
    exit 1
  fi

  # VM isolation: different groups = different VMs
  echo ""
  echo "=== VM isolation ==="
  SERVER_HOSTNAME=$(nomad alloc exec -i=false -t=false -task web "$SERVER_ALLOC" /bin/hostname 2>/dev/null || echo "")
  CLIENT_HOSTNAME=$(nomad alloc exec -i=false -t=false -task fetcher "$CLIENT_ALLOC" /bin/hostname 2>/dev/null || echo "")
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
  WEB_SIDECAR_HOSTNAME=$(nomad alloc exec -i=false -t=false -task web-sidecar "$SERVER_ALLOC" /bin/hostname 2>/dev/null || echo "")
  FETCHER_SIDECAR_HOSTNAME=$(nomad alloc exec -i=false -t=false -task fetcher-sidecar "$CLIENT_ALLOC" /bin/hostname 2>/dev/null || echo "")
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
  SERVER_IP=$(nomad alloc exec -i=false -t=false -task web "$SERVER_ALLOC" /bin/ip addr 2>/dev/null | sed -n 's/.*inet \([0-9.]*\)\/.*scope global.*/\1/p') || true
  echo "Server bridge IP: $SERVER_IP"

  if [ -z "$SERVER_IP" ] || [ "$SERVER_IP" = "null" ]; then
    echo "[FAIL] could not determine server bridge IP"
    nomad alloc status -json "$SERVER_ALLOC" | jq '.AllocatedResources.Shared' 2>/dev/null || true
    exit 1
  fi

  RESPONSE=""
  for i in $(seq 1 15); do
    RESPONSE=$(nomad alloc exec -i=false -t=false -task fetcher "$CLIENT_ALLOC" /bin/wget -q -O - "http://$SERVER_IP:8080/" 2>/dev/null || echo "")
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
    nomad alloc exec -i=false -t=false -task web "$SERVER_ALLOC" /bin/ip addr 2>/dev/null || true
    echo "Client network:"
    nomad alloc exec -i=false -t=false -task fetcher "$CLIENT_ALLOC" /bin/ip addr 2>/dev/null || true
    nomad alloc exec -i=false -t=false -task fetcher "$CLIENT_ALLOC" /bin/ip route 2>/dev/null || true
    exit 1
  fi

  echo ""
  echo "=== All integration tests passed ==="
''
