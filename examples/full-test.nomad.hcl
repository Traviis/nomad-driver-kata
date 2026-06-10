job "kata-full" {
  type        = "service"
  datacenters = ["dc1"]

  group "web" {

    network {
      mode = "bridge"
    }

    task "sidecar" {
      driver = "kata"

      lifecycle {
        hook    = "prestart"
        sidecar = true
      }

      config {
        image   = "docker.io/library/busybox:latest"
        command = "sh"
        args    = ["-c", "mkdir -p /www && echo sidecar-ok > /www/health && echo 'Sidecar: listening on :8080' && httpd -f -p 8080 -h /www"]
      }

      resources {
        cpu    = 50
        memory = 32
      }
    }

    task "app" {
      driver = "kata"

      config {
        image   = "docker.io/library/busybox:latest"
        command = "sh"
        args = ["-c", <<EOT
echo '=== Kata VM info ==='
cat /proc/version

echo '=== /etc/hosts ==='
cat /etc/hosts

echo '=== /etc/resolv.conf ==='
cat /etc/resolv.conf

echo '=== DNS resolution ==='
nslookup icanhazip.com 2>/dev/null | head -5 || echo 'nslookup not available'

echo '=== Task directories ==='
echo "NOMAD_ALLOC_DIR=$NOMAD_ALLOC_DIR"
echo "NOMAD_TASK_DIR=$NOMAD_TASK_DIR"
echo "NOMAD_SECRETS_DIR=$NOMAD_SECRETS_DIR"
ls -la /alloc/ 2>/dev/null && echo "ALLOC_DIR: exists" || echo "ALLOC_DIR: missing"
ls -la /local/ 2>/dev/null && echo "LOCAL_DIR: exists" || echo "LOCAL_DIR: missing"
ls -la /secrets/ 2>/dev/null && echo "SECRETS_DIR: exists" || echo "SECRETS_DIR: missing"

echo '=== Environment check ==='
echo "NOMAD_JOB_NAME=$NOMAD_JOB_NAME"
echo "NOMAD_TASK_NAME=$NOMAD_TASK_NAME"
echo "NOMAD_ALLOC_ID=$NOMAD_ALLOC_ID"
echo "NOMAD_DC=$NOMAD_DC"

echo '=== Network interfaces ==='
ifconfig eth0 2>/dev/null || ip addr show eth0 2>/dev/null

echo '=== External TCP connectivity (8.8.8.8:443) ==='
echo | nc -w 5 8.8.8.8 443 && echo 'EXTERNAL: PASS' || echo 'EXTERNAL: FAIL'

echo '=== Internal connectivity (sidecar on 127.0.0.1:8080) ==='
RESP=$(wget -qO- --timeout=5 http://127.0.0.1:8080/health 2>/dev/null)
if [ "$RESP" = "sidecar-ok" ]; then
  echo "INTERNAL: PASS (response: $RESP)"
else
  echo "INTERNAL: FAIL (response: $RESP)"
fi

echo '=== Tests complete ==='
sleep 3600
EOT
        ]
      }

      resources {
        cpu    = 100
        memory = 64
      }
    }
  }
}
