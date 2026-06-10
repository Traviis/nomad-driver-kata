package kata

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
)

func recorderClient(t *testing.T) (*CtrClient, func() string) {
	t.Helper()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "args.log")

	script := filepath.Join(dir, "ctr")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> "+logFile+"\n"), 0755); err != nil {
		t.Fatal(err)
	}

	c := NewCtrClient(script, "/run/test.sock", "testns", hclog.NewNullLogger())
	readLog := func() string {
		data, _ := os.ReadFile(logFile)
		return strings.TrimSpace(string(data))
	}
	return c, readLog
}

func TestCreateContainerBasic(t *testing.T) {
	c, readLog := recorderClient(t)
	err := c.CreateContainer(context.Background(), &ContainerConfig{
		ID:      "test-1",
		Image:   "busybox:latest",
		Runtime: "io.containerd.kata.v2",
		Annotations: map[string]string{
			"io.kubernetes.cri-o.ContainerType": "sandbox",
		},
		Command: []string{"sh", "-c", "echo hello"},
	})
	if err != nil {
		t.Fatal(err)
	}

	log := readLog()
	for _, want := range []string{
		"-a /run/test.sock",
		"-n testns",
		"container create",
		"--runtime io.containerd.kata.v2",
		"--annotation io.kubernetes.cri-o.ContainerType=sandbox",
		"busybox:latest test-1",
		"sh -c echo hello",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("args missing %q in: %s", want, log)
		}
	}
}

func TestCreateContainerWithNetNS(t *testing.T) {
	c, readLog := recorderClient(t)
	c.CreateContainer(context.Background(), &ContainerConfig{
		ID:      "test-ns",
		Image:   "pause:3.9",
		Runtime: "kata",
		NetNS:   "/var/run/netns/test123",
	})

	log := readLog()
	if !strings.Contains(log, "--with-ns network:/var/run/netns/test123") {
		t.Errorf("expected --with-ns flag in: %s", log)
	}
}

func TestCreateContainerWithEnvAndMounts(t *testing.T) {
	c, readLog := recorderClient(t)
	c.CreateContainer(context.Background(), &ContainerConfig{
		ID:      "test-em",
		Image:   "alpine:latest",
		Runtime: "kata",
		Env:     []string{"FOO=bar", "BAZ=qux"},
		Mounts: []string{
			"type=bind,src=/tmp/resolv.conf,dst=/etc/resolv.conf,options=bind",
		},
	})

	log := readLog()
	if !strings.Contains(log, "--env FOO=bar") {
		t.Errorf("missing --env FOO=bar in: %s", log)
	}
	if !strings.Contains(log, "--env BAZ=qux") {
		t.Errorf("missing --env BAZ=qux in: %s", log)
	}
	if !strings.Contains(log, "--mount type=bind,src=/tmp/resolv.conf,dst=/etc/resolv.conf,options=bind") {
		t.Errorf("missing --mount in: %s", log)
	}
}

func TestCreateContainerNoNetNSWhenEmpty(t *testing.T) {
	c, readLog := recorderClient(t)
	c.CreateContainer(context.Background(), &ContainerConfig{
		ID:      "test-no-ns",
		Image:   "busybox:latest",
		Runtime: "kata",
		NetNS:   "",
	})

	log := readLog()
	if strings.Contains(log, "--with-ns") {
		t.Errorf("unexpected --with-ns in: %s", log)
	}
}

func TestExecStreamingArgs(t *testing.T) {
	c, readLog := recorderClient(t)
	c.ExecStreaming(context.Background(), "container-1", "exec-123", []string{"cat", "/etc/hosts"}, false, nil, os.Stdout, os.Stderr)

	log := readLog()
	if !strings.Contains(log, "task exec --exec-id exec-123 container-1 cat /etc/hosts") {
		t.Errorf("unexpected exec args: %s", log)
	}
	if strings.Contains(log, "--tty") {
		t.Error("unexpected --tty flag when tty=false")
	}
}

func TestExecStreamingTTY(t *testing.T) {
	c, readLog := recorderClient(t)
	c.ExecStreaming(context.Background(), "container-1", "exec-456", []string{"sh"}, true, nil, os.Stdout, os.Stderr)

	log := readLog()
	if !strings.Contains(log, "--tty") {
		t.Errorf("expected --tty flag in: %s", log)
	}
}

func TestCreateContainerWithResourceLimits(t *testing.T) {
	c, readLog := recorderClient(t)
	c.CreateContainer(context.Background(), &ContainerConfig{
		ID:               "test-limits",
		Image:            "alpine:latest",
		Runtime:          "kata",
		User:             "nobody",
		Hostname:         "myhost",
		Cwd:              "/app",
		MemoryLimitBytes: 536870912,
		CPUQuota:         200000,
		CPUPeriod:        100000,
	})

	log := readLog()
	for _, want := range []string{
		"--user nobody",
		"--hostname myhost",
		"--cwd /app",
		"--memory-limit 536870912",
		"--cpu-quota 200000",
		"--cpu-period 100000",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("args missing %q in: %s", want, log)
		}
	}
}

func TestCreateContainerOmitsEmptyOptionals(t *testing.T) {
	c, readLog := recorderClient(t)
	c.CreateContainer(context.Background(), &ContainerConfig{
		ID:      "test-empty",
		Image:   "alpine:latest",
		Runtime: "kata",
	})

	log := readLog()
	for _, absent := range []string{
		"--user",
		"--hostname",
		"--cwd",
		"--memory-limit",
		"--cpu-quota",
		"--cpu-period",
	} {
		if strings.Contains(log, absent) {
			t.Errorf("unexpected %q in: %s", absent, log)
		}
	}
}

func TestCreateContainerPrivilegedAndUlimit(t *testing.T) {
	c, readLog := recorderClient(t)
	c.CreateContainer(context.Background(), &ContainerConfig{
		ID:         "test-priv",
		Image:      "alpine:latest",
		Runtime:    "kata",
		Privileged: true,
		Ulimit: map[string]string{
			"nofile": "1024:65536",
			"nproc":  "4096:8192",
		},
	})

	log := readLog()
	if !strings.Contains(log, "--privileged") {
		t.Errorf("expected --privileged in: %s", log)
	}
	if !strings.Contains(log, "--rlimit RLIMIT_NOFILE=1024:65536") {
		t.Errorf("expected --rlimit RLIMIT_NOFILE in: %s", log)
	}
	if !strings.Contains(log, "--rlimit RLIMIT_NPROC=4096:8192") {
		t.Errorf("expected --rlimit RLIMIT_NPROC in: %s", log)
	}
}

func TestEnsureImageWithAuth(t *testing.T) {
	c, readLog := recorderClient(t)
	c.EnsureImage(context.Background(), "registry.example.com/app:v1", false, "admin", "secret")

	log := readLog()
	if !strings.Contains(log, "--user admin:secret") {
		t.Errorf("expected --user auth in: %s", log)
	}
	if !strings.Contains(log, "registry.example.com/app:v1") {
		t.Errorf("expected image ref in: %s", log)
	}
}

func TestEnsureImageForcePull(t *testing.T) {
	c, readLog := recorderClient(t)
	// First call without force_pull — image "exists" in ls output (our script echoes nothing, so it won't match)
	c.EnsureImage(context.Background(), "alpine:latest", true, "", "")

	log := readLog()
	if !strings.Contains(log, "image pull") {
		t.Errorf("force_pull=true should always pull, got: %s", log)
	}
}
