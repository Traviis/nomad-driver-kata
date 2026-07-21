package kata

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"slices"
)

func testDriver() *Driver {
	d := NewDriver(hclog.NewNullLogger()).(*Driver)
	return d
}

func testDriverWithRecorder(t *testing.T) (*Driver, *recorder) {
	t.Helper()
	rec := newRecorder()
	d := NewDriver(hclog.NewNullLogger()).(*Driver)
	d.ctr = rec
	d.stateDir = t.TempDir()
	d.config = &PluginConfig{
		ContainerdAddr: "/test.sock",
		Namespace:      defaultNamespace,
		Runtime:        defaultRuntime,
	}
	d.sandboxMgr = NewSandboxManager(rec, d.logger)
	d.eventer = eventer.NewEventer(d.ctx, d.logger)
	d.imagePullTimeout = 5 * time.Minute
	return d, rec
}

func TestCapabilities(t *testing.T) {
	d := testDriver()
	caps, err := d.Capabilities()
	if err != nil {
		t.Fatal(err)
	}

	if !caps.SendSignals {
		t.Error("SendSignals should be true")
	}
	if !caps.Exec {
		t.Error("Exec should be true")
	}

	hasHost := false
	hasGroup := false
	for _, m := range caps.NetIsolationModes {
		if m == drivers.NetIsolationModeHost {
			hasHost = true
		}
		if m == drivers.NetIsolationModeGroup {
			hasGroup = true
		}
	}
	if !hasHost {
		t.Error("should support NetIsolationModeHost")
	}
	if !hasGroup {
		t.Error("should support NetIsolationModeGroup")
	}
}

func TestPluginInfo(t *testing.T) {
	d := testDriver()
	info, err := d.PluginInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "kata" {
		t.Errorf("plugin name = %q, want %q", info.Name, "kata")
	}
}

func TestWriteResolvConfExplicitDNS(t *testing.T) {
	d := testDriver()
	path := filepath.Join(t.TempDir(), "resolv.conf")

	dns := &drivers.DNSConfig{
		Servers:  []string{"10.0.0.1", "10.0.0.2"},
		Searches: []string{"svc.cluster.local", "cluster.local"},
		Options:  []string{"ndots:5"},
	}

	if err := d.writeResolvConf(path, dns); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)

	for _, want := range []string{
		"nameserver 10.0.0.1",
		"nameserver 10.0.0.2",
		"search svc.cluster.local cluster.local",
		"options ndots:5",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("resolv.conf missing %q in:\n%s", want, content)
		}
	}
}

func TestWriteResolvConfNilDNS(t *testing.T) {
	d := testDriver()
	path := filepath.Join(t.TempDir(), "resolv.conf")

	if err := d.writeResolvConf(path, nil); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)

	if !strings.Contains(content, "nameserver") {
		t.Errorf("resolv.conf should have at least one nameserver, got:\n%s", content)
	}
}

func TestWriteResolvConfLoopbackFallback(t *testing.T) {
	d := testDriver()
	path := filepath.Join(t.TempDir(), "resolv.conf")

	dns := &drivers.DNSConfig{
		Servers: []string{},
	}

	if err := d.writeResolvConf(path, dns); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)

	if !strings.Contains(content, "nameserver") {
		t.Errorf("resolv.conf should have fallback nameservers, got:\n%s", content)
	}
}

func TestFilterResolvConf(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantReal bool
		wantHas  []string
		wantNot  []string
	}{
		{
			name:     "real servers kept",
			input:    "nameserver 10.0.0.1\nnameserver 10.0.0.2\nsearch example.com\n",
			wantReal: true,
			wantHas:  []string{"nameserver 10.0.0.1", "nameserver 10.0.0.2", "search example.com"},
		},
		{
			name:     "ipv4 loopback filtered",
			input:    "nameserver 127.0.0.53\nsearch example.com\n",
			wantReal: false,
			wantHas:  []string{"search example.com"},
			wantNot:  []string{"127.0.0.53"},
		},
		{
			name:     "ipv6 loopback filtered",
			input:    "nameserver ::1\nsearch example.com\n",
			wantReal: false,
			wantHas:  []string{"search example.com"},
			wantNot:  []string{"::1"},
		},
		{
			name:     "mixed keeps real only",
			input:    "nameserver 127.0.0.1\nnameserver 10.0.0.1\n",
			wantReal: true,
			wantHas:  []string{"nameserver 10.0.0.1"},
			wantNot:  []string{"127.0.0.1"},
		},
		{
			name:     "comments stripped",
			input:    "# comment\nnameserver 10.0.0.1\n",
			wantReal: true,
			wantHas:  []string{"nameserver 10.0.0.1"},
			wantNot:  []string{"# comment"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines, hasReal := filterResolvConf([]byte(tt.input))
			if hasReal != tt.wantReal {
				t.Errorf("hasReal = %v, want %v", hasReal, tt.wantReal)
			}
			content := strings.Join(lines, "\n")
			for _, want := range tt.wantHas {
				if !strings.Contains(content, want) {
					t.Errorf("missing %q in:\n%s", want, content)
				}
			}
			for _, notWant := range tt.wantNot {
				if strings.Contains(content, notWant) {
					t.Errorf("should not contain %q in:\n%s", notWant, content)
				}
			}
		})
	}
}

func TestHostResolvConfFallback(t *testing.T) {
	dir := t.TempDir()

	primary := filepath.Join(dir, "resolv.conf")
	os.WriteFile(primary, []byte("nameserver 127.0.0.53\nsearch local\n"), 0644)

	fallback := filepath.Join(dir, "upstream-resolv.conf")
	os.WriteFile(fallback, []byte("nameserver 192.168.1.1\nsearch example.com\n"), 0644)

	lines := hostResolvConf([]string{primary, fallback})
	content := strings.Join(lines, "\n")

	if strings.Contains(content, "127.0.0.53") {
		t.Errorf("should not contain stub resolver:\n%s", content)
	}
	if !strings.Contains(content, "192.168.1.1") {
		t.Errorf("should contain upstream server:\n%s", content)
	}
	if !strings.Contains(content, "search example.com") {
		t.Errorf("should contain upstream search domain:\n%s", content)
	}
}

func TestHostResolvConfAllLoopback(t *testing.T) {
	dir := t.TempDir()

	stub := filepath.Join(dir, "resolv.conf")
	os.WriteFile(stub, []byte("nameserver 127.0.0.53\n"), 0644)

	lines := hostResolvConf([]string{stub, filepath.Join(dir, "nonexistent")})
	content := strings.Join(lines, "\n")

	if !strings.Contains(content, "8.8.8.8") {
		t.Errorf("should fall back to public DNS:\n%s", content)
	}
}

func TestWriteHosts(t *testing.T) {
	d := testDriver()
	path := filepath.Join(t.TempDir(), "hosts")

	if err := d.writeHosts(path, nil, nil); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)

	if !strings.Contains(content, "127.0.0.1") {
		t.Errorf("hosts missing 127.0.0.1 in:\n%s", content)
	}
	if !strings.Contains(content, "localhost") {
		t.Errorf("hosts missing localhost in:\n%s", content)
	}
	if !strings.Contains(content, "::1") {
		t.Errorf("hosts missing ::1 in:\n%s", content)
	}
}

func TestWriteHostsWithHostname(t *testing.T) {
	d := testDriver()
	path := filepath.Join(t.TempDir(), "hosts")

	hosts := &drivers.HostsConfig{
		Address:  "172.26.64.5",
		Hostname: "my-task",
	}
	if err := d.writeHosts(path, hosts, nil); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)

	if !strings.Contains(content, "172.26.64.5 my-task") {
		t.Errorf("hosts missing hostname entry in:\n%s", content)
	}
	if !strings.Contains(content, "127.0.0.1 localhost") {
		t.Errorf("hosts missing localhost in:\n%s", content)
	}
}

func TestWriteHostsExtraHosts(t *testing.T) {
	d := testDriver()
	path := filepath.Join(t.TempDir(), "hosts")

	extra := []string{"mydb:10.0.0.5", "cache:10.0.0.6"}
	if err := d.writeHosts(path, nil, extra); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)

	if !strings.Contains(content, "10.0.0.5 mydb") {
		t.Errorf("hosts missing extra host mydb in:\n%s", content)
	}
	if !strings.Contains(content, "10.0.0.6 cache") {
		t.Errorf("hosts missing extra host cache in:\n%s", content)
	}
	if !strings.Contains(content, "127.0.0.1 localhost") {
		t.Errorf("hosts missing localhost in:\n%s", content)
	}
}

func TestBindMount(t *testing.T) {
	tests := []struct {
		name        string
		src, dst    string
		readonly    bool
		propagation string
		wantOpts    string
	}{
		{"default", "/src", "/dst", false, "", "rbind:rprivate"},
		{"readonly", "/src", "/dst", true, "", "rbind:rprivate:ro"},
		{"file bind", "/src", "/dst", true, "file", "bind:ro"},
		{"bidirectional", "/src", "/dst", false, "bidirectional", "rbind:rshared"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := bindMount(tt.src, tt.dst, tt.readonly, tt.propagation)
			if m.Source != tt.src {
				t.Errorf("Source = %q, want %q", m.Source, tt.src)
			}
			if m.Destination != tt.dst {
				t.Errorf("Destination = %q, want %q", m.Destination, tt.dst)
			}
			if m.Type != "bind" {
				t.Errorf("Type = %q, want %q", m.Type, "bind")
			}
			got := strings.Join(m.Options, ":")
			if got != tt.wantOpts {
				t.Errorf("Options = %q, want %q", got, tt.wantOpts)
			}
		})
	}
}

func TestPropagationOption(t *testing.T) {
	tests := []struct {
		mode string
		want string
	}{
		{"", "rprivate"},
		{structs.VolumeMountPropagationPrivate, "rprivate"},
		{structs.VolumeMountPropagationHostToTask, "rslave"},
		{structs.VolumeMountPropagationBidirectional, "rshared"},
		{"custom", "custom"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("mode=%q", tt.mode), func(t *testing.T) {
			got := propagationOption(tt.mode)
			if got != tt.want {
				t.Errorf("propagationOption(%q) = %q, want %q", tt.mode, got, tt.want)
			}
		})
	}
}

func TestAddPortEnvNilResources(t *testing.T) {
	cfg := &drivers.TaskConfig{}
	env := []string{"FOO=bar"}
	got := addPortEnv(env, cfg)
	if len(got) != 1 || got[0] != "FOO=bar" {
		t.Errorf("expected unchanged env, got %v", got)
	}
}

func TestAddPortEnvWithPorts(t *testing.T) {
	ports := structs.AllocatedPorts{
		{Label: "http", Value: 8080, To: 80},
		{Label: "grpc", Value: 9090, To: 0},
	}
	cfg := &drivers.TaskConfig{
		Resources: &drivers.Resources{
			Ports: &ports,
		},
	}
	got := addPortEnv(nil, cfg)

	has := func(want string) bool {
		for _, v := range got {
			if v == want {
				return true
			}
		}
		return false
	}

	if !has("NOMAD_PORT_http=80") {
		t.Errorf("missing NOMAD_PORT_http=80 in %v", got)
	}
	if !has("NOMAD_PORT_grpc=9090") {
		t.Errorf("missing NOMAD_PORT_grpc=9090 (To=0 should use Value) in %v", got)
	}
}

func TestBuildDriverNetworkWithPorts(t *testing.T) {
	ports := structs.AllocatedPorts{
		{Label: "http", Value: 8080, To: 80},
		{Label: "grpc", Value: 9090, To: 0},
	}
	cfg := &drivers.TaskConfig{
		Resources: &drivers.Resources{Ports: &ports},
	}
	net := buildDriverNetwork(cfg)
	if net == nil {
		t.Fatal("expected non-nil DriverNetwork")
	}
	if net.PortMap["http"] != 80 {
		t.Errorf("PortMap[http] = %d, want 80", net.PortMap["http"])
	}
	if net.PortMap["grpc"] != 9090 {
		t.Errorf("PortMap[grpc] = %d, want 9090", net.PortMap["grpc"])
	}
}

func TestBuildDriverNetworkNoPorts(t *testing.T) {
	cfg := &drivers.TaskConfig{}
	if net := buildDriverNetwork(cfg); net != nil {
		t.Errorf("expected nil for no ports, got %+v", net)
	}
}

func TestBuildDriverNetworkAutoAdvertise(t *testing.T) {
	ports := structs.AllocatedPorts{
		{Label: "http", Value: 8080, To: 80},
	}
	cfg := &drivers.TaskConfig{
		Resources: &drivers.Resources{Ports: &ports},
		NetworkIsolation: &drivers.NetworkIsolationSpec{
			HostsConfig: &drivers.HostsConfig{
				Address:  "172.26.64.5",
				Hostname: "myapp",
			},
		},
	}
	net := buildDriverNetwork(cfg)
	if net == nil {
		t.Fatal("expected non-nil DriverNetwork")
	}
	if net.IP != "172.26.64.5" {
		t.Errorf("IP = %q, want %q", net.IP, "172.26.64.5")
	}
	if !net.AutoAdvertise {
		t.Error("AutoAdvertise should be true when IP is set")
	}
}

func TestBuildMountsStandard(t *testing.T) {
	dir := t.TempDir()
	cfg := &drivers.TaskConfig{
		AllocDir: dir,
		Name:     "web",
	}

	mounts, err := buildMounts(cfg, filepath.Join(dir, "resolv.conf"), filepath.Join(dir, "hosts"))
	if err != nil {
		t.Fatal(err)
	}

	if len(mounts) != 5 {
		t.Fatalf("expected 5 mounts, got %d", len(mounts))
	}

	for _, dst := range []string{"/alloc", "/local", "/secrets", "/etc/resolv.conf", "/etc/hosts"} {
		found := false
		for _, m := range mounts {
			if m.Destination == dst {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing mount for dst=%s", dst)
		}
	}
}

func TestBuildMountsWithCustom(t *testing.T) {
	dir := t.TempDir()
	cfg := &drivers.TaskConfig{
		AllocDir: dir,
		Name:     "web",
		Mounts: []*drivers.MountConfig{
			{HostPath: "/data/db", TaskPath: "/var/lib/db", Readonly: false},
		},
	}

	mounts, err := buildMounts(cfg, "/tmp/resolv.conf", "/tmp/hosts")
	if err != nil {
		t.Fatal(err)
	}

	if len(mounts) != 6 {
		t.Fatalf("expected 6 mounts, got %d", len(mounts))
	}

	found := false
	for _, m := range mounts {
		if m.Destination == "/var/lib/db" && m.Source == "/data/db" {
			found = true
		}
	}
	if !found {
		t.Error("custom mount not found")
	}
}

func TestBuildMountsRejectsEmptyPath(t *testing.T) {
	dir := t.TempDir()
	cfg := &drivers.TaskConfig{
		AllocDir: dir,
		Name:     "web",
		Mounts: []*drivers.MountConfig{
			{HostPath: "", TaskPath: "/dst"},
		},
	}

	_, err := buildMounts(cfg, "/tmp/resolv.conf", "/tmp/hosts")
	if err == nil {
		t.Error("expected error for empty host path")
	}
}

func TestHostsConfigNil(t *testing.T) {
	cfg := &drivers.TaskConfig{}
	if got := hostsConfig(cfg); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestHostsConfigPresent(t *testing.T) {
	want := &drivers.HostsConfig{
		Address:  "10.0.0.1",
		Hostname: "myhost",
	}
	cfg := &drivers.TaskConfig{
		NetworkIsolation: &drivers.NetworkIsolationSpec{
			HostsConfig: want,
		},
	}
	got := hostsConfig(cfg)
	if got != want {
		t.Errorf("hostsConfig() = %+v, want %+v", got, want)
	}
}

func TestBuildFingerprintNotConfigured(t *testing.T) {
	d := testDriver()
	fp := d.buildFingerprint()
	if fp.Health != drivers.HealthStateUndetected {
		t.Errorf("Health = %v, want HealthStateUndetected", fp.Health)
	}
}

func TestBuildFingerprintHealthy(t *testing.T) {
	d, _ := testDriverWithRecorder(t)
	fp := d.buildFingerprint()
	if fp.Health != drivers.HealthStateHealthy {
		t.Errorf("Health = %v, want HealthStateHealthy", fp.Health)
	}
}

func TestBuildFingerprintUnhealthy(t *testing.T) {
	d := testDriver()
	rec := newRecorder()
	rec.versionErr = fmt.Errorf("connection refused")
	d.ctr = rec

	fp := d.buildFingerprint()
	if fp.Health != drivers.HealthStateUnhealthy {
		t.Errorf("Health = %v, want HealthStateUnhealthy", fp.Health)
	}
}
func testTaskConfig(t *testing.T, taskCfg *TaskConfig) *drivers.TaskConfig {
	t.Helper()
	cfg := &drivers.TaskConfig{
		ID:            "test-task-id",
		AllocID:       "alloc-1",
		Name:          "web",
		TaskGroupName: "group",
	}
	if err := cfg.EncodeConcreteDriverConfig(taskCfg); err != nil {
		t.Fatalf("encoding driver config: %v", err)
	}
	return cfg
}

func TestStartTask(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})

	handle, _, err := d.StartTask(cfg)
	if err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	if handle == nil {
		t.Fatal("expected non-nil handle")
	}

	if !rec.called("Cleanup") {
		t.Error("expected Cleanup call for previous state")
	}
	if !rec.called("EnsureImage") {
		t.Error("expected EnsureImage call")
	}
	if !rec.called("CreateContainer") {
		t.Error("expected CreateContainer call")
	}

	cc := rec.lastConfig()
	if cc == nil {
		t.Fatal("no ContainerConfig recorded")
	}
	if cc.Image != "docker.io/library/alpine:latest" {
		t.Errorf("Image = %q, want %q", cc.Image, "docker.io/library/alpine:latest")
	}
	if cc.Runtime != defaultRuntime {
		t.Errorf("Runtime = %q, want %q", cc.Runtime, defaultRuntime)
	}
	if cc.ID != "kata-alloc-1-web" {
		t.Errorf("container ID = %q, want %q", cc.ID, "kata-alloc-1-web")
	}
}

func TestStartTaskPassesOptions(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{
		Image:          "myapp:v1",
		Command:        "/bin/server",
		Args:           []string{"--port", "8080"},
		Hostname:       "myhost",
		Privileged:     true,
		ReadonlyRootfs: true,
		PidsLimit:      100,
		CapAdd:         []string{"NET_ADMIN"},
		CapDrop:        []string{"MKNOD"},
		Devices:        []string{"/dev/fuse"},
		Labels:         map[string]string{"env": "test"},
	})

	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	sandbox := rec.configForID(sandboxID(cfg.AllocID))
	if sandbox == nil {
		t.Fatal("no sandbox ContainerConfig recorded")
	}
	if sandbox.Hostname != cfg.TaskGroupName {
		t.Errorf("sandbox Hostname = %q, want %q", sandbox.Hostname, cfg.TaskGroupName)
	}

	cc := rec.lastConfig()
	if cc.Hostname != "myhost" {
		t.Errorf("Hostname = %q, want %q", cc.Hostname, "myhost")
	}
	if !cc.Privileged {
		t.Error("Privileged should be true")
	}
	if !cc.ReadonlyRootfs {
		t.Error("ReadonlyRootfs should be true")
	}
	if cc.PidsLimit != 100 {
		t.Errorf("PidsLimit = %d, want 100", cc.PidsLimit)
	}
	if len(cc.CapAdd) != 1 || cc.CapAdd[0] != "NET_ADMIN" {
		t.Errorf("CapAdd = %v, want [NET_ADMIN]", cc.CapAdd)
	}
	if len(cc.CapDrop) != 1 || cc.CapDrop[0] != "MKNOD" {
		t.Errorf("CapDrop = %v, want [MKNOD]", cc.CapDrop)
	}
	if len(cc.Devices) != 1 || cc.Devices[0] != "/dev/fuse" {
		t.Errorf("Devices = %v, want [/dev/fuse]", cc.Devices)
	}
	if cc.Labels["env"] != "test" {
		t.Errorf("Labels[env] = %q, want %q", cc.Labels["env"], "test")
	}
	if len(cc.Command) != 3 || cc.Command[0] != "/bin/server" {
		t.Errorf("Command = %v, want [/bin/server --port 8080]", cc.Command)
	}
}

func TestStartTaskVolumes(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{
		Image: "alpine:latest",
		Volumes: []string{
			"/host/data:/container/data",
			"/host/config:/container/config:ro",
		},
	})

	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	cc := rec.lastConfig()
	if cc == nil {
		t.Fatal("no ContainerConfig recorded")
	}

	// 5 standard mounts + 2 volumes = 7
	if len(cc.Mounts) != 7 {
		t.Fatalf("expected 7 mounts, got %d: %+v", len(cc.Mounts), cc.Mounts)
	}

	dataMount := cc.Mounts[5]
	if dataMount.Source != "/host/data" || dataMount.Destination != "/container/data" {
		t.Errorf("volume[0] = %s:%s, want /host/data:/container/data", dataMount.Source, dataMount.Destination)
	}
	for _, opt := range dataMount.Options {
		if opt == "ro" {
			t.Error("volume[0] should not be readonly")
		}
	}

	configMount := cc.Mounts[6]
	if configMount.Source != "/host/config" || configMount.Destination != "/container/config" {
		t.Errorf("volume[1] = %s:%s, want /host/config:/container/config", configMount.Source, configMount.Destination)
	}
	hasRo := false
	for _, opt := range configMount.Options {
		if opt == "ro" {
			hasRo = true
		}
	}
	if !hasRo {
		t.Error("volume[1] should be readonly")
	}
}

func TestStartTaskVolumeInvalid(t *testing.T) {
	d, _ := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{
		Image:   "alpine:latest",
		Volumes: []string{"no-colon-here"},
	})

	_, _, err := d.StartTask(cfg)
	if err == nil {
		t.Fatal("expected error for invalid volume string")
	}
	if !strings.Contains(err.Error(), "invalid volume") {
		t.Errorf("error = %q, want it to contain 'invalid volume'", err)
	}
}

func TestStartTaskVolumeRelativePath(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	taskCfg := &TaskConfig{
		Image: "alpine:latest",
		Volumes: []string{
			"local/init.sql:/docker-entrypoint-initdb.d/init.sql",
		},
	}
	cfg := &drivers.TaskConfig{
		ID:            "test-task-id",
		AllocID:       "alloc-1",
		Name:          "web",
		TaskGroupName: "group",
		AllocDir:      "/opt/nomad/alloc/abc123",
	}
	if err := cfg.EncodeConcreteDriverConfig(taskCfg); err != nil {
		t.Fatalf("encoding driver config: %v", err)
	}

	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	cc := rec.lastConfig()
	if cc == nil {
		t.Fatal("no ContainerConfig recorded")
	}

	// 5 standard mounts + 1 volume = 6
	if len(cc.Mounts) != 6 {
		t.Fatalf("expected 6 mounts, got %d: %+v", len(cc.Mounts), cc.Mounts)
	}

	vol := cc.Mounts[5]
	wantSource := "/opt/nomad/alloc/abc123/web/local/init.sql"
	if vol.Source != wantSource {
		t.Errorf("volume source = %q, want %q", vol.Source, wantSource)
	}
	if vol.Destination != "/docker-entrypoint-initdb.d/init.sql" {
		t.Errorf("volume dest = %q, want /docker-entrypoint-initdb.d/init.sql", vol.Destination)
	}
}

func TestStartTaskMountBlocks(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{
		Image: "alpine:latest",
		Mounts: []MountConfig{
			{Type: "bind", Source: "/mnt/data", Target: "/data", Readonly: false},
			{Type: "bind", Source: "/mnt/cache", Target: "/cache", Readonly: true},
		},
	})

	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	cc := rec.lastConfig()
	if cc == nil {
		t.Fatal("no ContainerConfig recorded")
	}

	// 5 standard mounts + 2 mount blocks = 7
	if len(cc.Mounts) != 7 {
		t.Fatalf("expected 7 mounts, got %d: %+v", len(cc.Mounts), cc.Mounts)
	}

	dataMount := cc.Mounts[5]
	if dataMount.Source != "/mnt/data" || dataMount.Destination != "/data" {
		t.Errorf("mount[0] = %s:%s, want /mnt/data:/data", dataMount.Source, dataMount.Destination)
	}

	cacheMount := cc.Mounts[6]
	if cacheMount.Source != "/mnt/cache" || cacheMount.Destination != "/cache" {
		t.Errorf("mount[1] = %s:%s, want /mnt/cache:/cache", cacheMount.Source, cacheMount.Destination)
	}
	hasRo := false
	for _, opt := range cacheMount.Options {
		if opt == "ro" {
			hasRo = true
		}
	}
	if !hasRo {
		t.Error("mount[1] should be readonly")
	}
}

func TestStartTaskMountEmptySourceOrTarget(t *testing.T) {
	d, _ := testDriverWithRecorder(t)

	cfg := testTaskConfig(t, &TaskConfig{
		Image:  "alpine:latest",
		Mounts: []MountConfig{{Type: "bind", Source: "", Target: "/data"}},
	})
	_, _, err := d.StartTask(cfg)
	if err == nil {
		t.Fatal("expected error for empty mount source")
	}
	if !strings.Contains(err.Error(), "mount requires source and target") {
		t.Errorf("error = %q, want it to contain 'mount requires source and target'", err)
	}

	cfg = testTaskConfig(t, &TaskConfig{
		Image:  "alpine:latest",
		Mounts: []MountConfig{{Type: "bind", Source: "/data", Target: ""}},
	})
	_, _, err = d.StartTask(cfg)
	if err == nil {
		t.Fatal("expected error for empty mount target")
	}
}

func TestDestroyTask(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})

	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	if err := d.DestroyTask(cfg.ID, true); err != nil {
		t.Fatalf("DestroyTask: %v", err)
	}

	if !rec.called("DeleteTask") {
		t.Error("expected DeleteTask call")
	}
	if !rec.called("DeleteContainer") {
		t.Error("expected DeleteContainer call")
	}

	configDir := d.taskConfigDir(cfg.AllocID, cfg.Name)
	if _, err := os.Stat(configDir); err == nil {
		t.Errorf("config dir should be removed: %s", configDir)
	}

	if _, ok := d.tasks.Get(cfg.ID); ok {
		t.Error("task should be removed from store")
	}
}

func TestParseDevice(t *testing.T) {
	tests := []struct {
		input     string
		wantHost  string
		wantCtr   string
		wantPerms string
	}{
		{"/dev/fuse", "/dev/fuse", "", "rwm"},
		{"/dev/fuse:/dev/fuse", "/dev/fuse", "/dev/fuse", "rwm"},
		{"/dev/sda:/dev/xvda:r", "/dev/sda", "/dev/xvda", "r"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			host, ctr, perms := parseDevice(tt.input)
			if host != tt.wantHost {
				t.Errorf("host = %q, want %q", host, tt.wantHost)
			}
			if ctr != tt.wantCtr {
				t.Errorf("container = %q, want %q", ctr, tt.wantCtr)
			}
			if perms != tt.wantPerms {
				t.Errorf("perms = %q, want %q", perms, tt.wantPerms)
			}
		})
	}
}
func TestParseSignal(t *testing.T) {
	tests := []struct {
		input string
		want  syscall.Signal
	}{
		{"SIGTERM", syscall.SIGTERM},
		{"SIGKILL", syscall.SIGKILL},
		{"SIGHUP", syscall.SIGHUP},
		{"SIGINT", syscall.SIGINT},
		{"SIGQUIT", syscall.SIGQUIT},
		{"SIGUSR1", syscall.SIGUSR1},
		{"SIGUSR2", syscall.SIGUSR2},
		{"SIGPIPE", syscall.SIGPIPE},
		{"SIGCONT", syscall.SIGCONT},
		// without SIG prefix
		{"TERM", syscall.SIGTERM},
		{"KILL", syscall.SIGKILL},
		{"HUP", syscall.SIGHUP},
		// lowercase
		{"sigterm", syscall.SIGTERM},
		{"kill", syscall.SIGKILL},
		// unknown falls back to SIGTERM
		{"BOGUS", syscall.SIGTERM},
		{"", syscall.SIGTERM},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseSignal(tt.input)
			if got != tt.want {
				t.Errorf("parseSignal(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}
func TestStopTask(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})

	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	if err := d.StopTask(cfg.ID, 5*time.Second, "SIGINT"); err != nil {
		t.Fatalf("StopTask: %v", err)
	}

	if !rec.called("KillTask") {
		t.Error("expected KillTask call")
	}
}

func TestStopTaskDefaultSignal(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})

	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	if err := d.StopTask(cfg.ID, 5*time.Second, ""); err != nil {
		t.Fatalf("StopTask: %v", err)
	}

	rec.mu.Lock()
	var signal string
	for _, c := range rec.calls {
		if c.Method == "KillTask" && len(c.Args) >= 2 {
			signal, _ = c.Args[1].(string)
		}
	}
	rec.mu.Unlock()
	if signal != "SIGTERM" {
		t.Errorf("expected default SIGTERM, got %q", signal)
	}
}

func TestWaitTask(t *testing.T) {
	d, _ := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})

	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := d.WaitTask(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("WaitTask: %v", err)
	}

	result := <-ch
	if result == nil {
		t.Fatal("expected non-nil exit result")
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

func TestInspectTask(t *testing.T) {
	d, _ := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})

	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	status, err := d.InspectTask(cfg.ID)
	if err != nil {
		t.Fatalf("InspectTask: %v", err)
	}
	// NOTE: TaskStatus.ID currently reports the container ID, not cfg.ID.
	// Whether that is correct is an open question in docs/TEST_GAPS.md.
	if status.State != drivers.TaskStateRunning {
		t.Errorf("State = %q, want %q", status.State, drivers.TaskStateRunning)
	}
}

func TestInspectTaskNotFound(t *testing.T) {
	d, _ := testDriverWithRecorder(t)
	_, err := d.InspectTask("nonexistent")
	if err == nil {
		t.Error("expected error for unknown task")
	}
}

func TestWaitTaskNotFound(t *testing.T) {
	d, _ := testDriverWithRecorder(t)
	_, err := d.WaitTask(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for unknown task")
	}
}

func TestSignalTask(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})

	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	if err := d.SignalTask(cfg.ID, "SIGUSR1"); err != nil {
		t.Fatalf("SignalTask: %v", err)
	}

	rec.mu.Lock()
	var signal string
	for _, c := range rec.calls {
		if c.Method == "KillTask" && len(c.Args) >= 2 {
			signal, _ = c.Args[1].(string)
		}
	}
	rec.mu.Unlock()
	if signal != "SIGUSR1" {
		t.Errorf("expected SIGUSR1, got %q", signal)
	}
}

func TestSignalTaskNotFound(t *testing.T) {
	d, _ := testDriverWithRecorder(t)
	err := d.SignalTask("nonexistent", "SIGUSR1")
	if err == nil {
		t.Fatal("expected error for unknown task")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

func TestExecTask(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})

	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	result, err := d.ExecTask(cfg.ID, []string{"echo", "hello"}, 5*time.Second)
	if err != nil {
		t.Fatalf("ExecTask: %v", err)
	}

	if result.ExitResult.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitResult.ExitCode)
	}

	if !rec.called("Exec") {
		t.Error("expected Exec call")
	}
}

func TestRecoverTask(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})

	containerID := "kata-alloc-1-web"
	rec.mu.Lock()
	rec.running[containerID] = true
	rec.mu.Unlock()

	state := &TaskState{
		ContainerID: containerID,
		SandboxID:   "kata-alloc-1-sandbox",
		AllocID:     "alloc-1",
		TaskName:    "web",
		StartedAt:   time.Now().UnixNano(),
	}

	handle := drivers.NewTaskHandle(taskHandleVersion)
	handle.Config = cfg
	if err := handle.SetDriverState(state); err != nil {
		t.Fatalf("SetDriverState: %v", err)
	}

	if err := d.RecoverTask(handle); err != nil {
		t.Fatalf("RecoverTask: %v", err)
	}

	if _, ok := d.tasks.Get(cfg.ID); !ok {
		t.Error("task should be in store after recovery")
	}
}

func TestRecoverTaskNotRunning(t *testing.T) {
	d, _ := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})

	state := &TaskState{
		ContainerID: "kata-alloc-1-web",
		SandboxID:   "kata-alloc-1-sandbox",
		AllocID:     "alloc-1",
		TaskName:    "web",
		StartedAt:   time.Now().UnixNano(),
	}

	handle := drivers.NewTaskHandle(taskHandleVersion)
	handle.Config = cfg
	if err := handle.SetDriverState(state); err != nil {
		t.Fatalf("SetDriverState: %v", err)
	}

	err := d.RecoverTask(handle)
	if err == nil {
		t.Error("expected error when container is not running")
	}
}

func TestStartTaskRollbackOnContainerFailure(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	rec.createContainerErrFor = map[string]error{
		"kata-alloc-1-web": fmt.Errorf("disk full"),
	}

	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})
	_, _, err := d.StartTask(cfg)
	if err == nil {
		t.Fatal("expected StartTask to fail")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("unexpected error: %v", err)
	}

	sbID := "kata-alloc-1-sandbox"
	rec.mu.Lock()
	var cleanupSandbox, deleteSandboxMeta bool
	for _, c := range rec.calls {
		if c.Method == "Cleanup" && len(c.Args) > 0 && c.Args[0] == sbID {
			cleanupSandbox = true
		}
		if c.Method == "DeleteSandboxMetadata" && len(c.Args) > 0 && c.Args[0] == sbID {
			deleteSandboxMeta = true
		}
	}
	rec.mu.Unlock()

	if !cleanupSandbox {
		t.Error("expected sandbox Cleanup after rollback")
	}
	if !deleteSandboxMeta {
		t.Error("expected sandbox DeleteSandboxMetadata after rollback")
	}

	if _, ok := d.tasks.Get(cfg.ID); ok {
		t.Error("task should not be in store after failed start")
	}
}

func TestWaitTaskExitCode(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	rec.runExit = 42

	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})
	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := d.WaitTask(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("WaitTask: %v", err)
	}

	result := <-ch
	if result == nil {
		t.Fatal("expected non-nil exit result")
	}
	if result.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", result.ExitCode)
	}
}

func TestTaskStats(t *testing.T) {
	d, _ := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})

	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := d.TaskStats(ctx, cfg.ID, time.Millisecond)
	if err != nil {
		t.Fatalf("TaskStats: %v", err)
	}

	select {
	case usage, ok := <-ch:
		if !ok {
			t.Fatal("stats channel closed unexpectedly")
		}
		if usage == nil {
			t.Fatal("expected non-nil usage")
		}
		if usage.ResourceUsage == nil {
			t.Fatal("expected non-nil ResourceUsage")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for stats")
	}

	cancel()
	// Drain until channel closes
	for range ch {
	}
}

func TestTaskStatsContextCancellation(t *testing.T) {
	d, _ := testDriverWithRecorder(t)
	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})

	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := d.TaskStats(ctx, cfg.ID, time.Millisecond)
	if err != nil {
		t.Fatalf("TaskStats: %v", err)
	}

	cancel()

	closed := false
	timeout := time.After(3 * time.Second)
	for !closed {
		select {
		case _, ok := <-ch:
			if !ok {
				closed = true
			}
		case <-timeout:
			t.Fatal("stats channel not closed after context cancellation")
		}
	}
}

func TestTaskStatsMetricsError(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	rec.metricsErr = fmt.Errorf("containerd unavailable")

	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})
	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := d.TaskStats(ctx, cfg.ID, time.Millisecond)
	if err != nil {
		t.Fatalf("TaskStats: %v", err)
	}

	// Let ticks fire with errors
	time.Sleep(10 * time.Millisecond)

	// Channel should still be open (errors are swallowed)
	cancel()
	for range ch {
	}

	if rec.callCount("Metrics") == 0 {
		t.Error("expected Metrics calls despite errors")
	}
}

func TestDestroyTaskNotForceRunning(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	rec.runCh = make(chan struct{})
	defer close(rec.runCh)

	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})
	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}

	err := d.DestroyTask(cfg.ID, false)
	if err == nil {
		t.Fatal("expected error destroying running task without force")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDestroyTaskSandboxTeardown(t *testing.T) {
	d, rec := testDriverWithRecorder(t)

	cfg1 := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})
	cfg1.ID = "task-1"
	cfg1.Name = "web"

	cfg2 := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})
	cfg2.ID = "task-2"
	cfg2.Name = "sidecar"

	if _, _, err := d.StartTask(cfg1); err != nil {
		t.Fatalf("StartTask(1): %v", err)
	}
	if _, _, err := d.StartTask(cfg2); err != nil {
		t.Fatalf("StartTask(2): %v", err)
	}

	sbID := "kata-alloc-1-sandbox"

	if err := d.DestroyTask(cfg1.ID, true); err != nil {
		t.Fatalf("DestroyTask(1): %v", err)
	}

	// Sandbox should still exist — second task holds a ref
	rec.mu.Lock()
	var cleanupCount int
	for _, c := range rec.calls {
		if c.Method == "Cleanup" && len(c.Args) > 0 && c.Args[0] == sbID {
			cleanupCount++
		}
	}
	rec.mu.Unlock()
	if cleanupCount != 0 {
		t.Errorf("sandbox Cleanup called %d times after first destroy, want 0", cleanupCount)
	}

	if err := d.DestroyTask(cfg2.ID, true); err != nil {
		t.Fatalf("DestroyTask(2): %v", err)
	}

	// Now sandbox should be torn down
	rec.mu.Lock()
	var sandboxCleanup, sandboxDeleteMeta bool
	for _, c := range rec.calls {
		if c.Method == "Cleanup" && len(c.Args) > 0 && c.Args[0] == sbID {
			sandboxCleanup = true
		}
		if c.Method == "DeleteSandboxMetadata" && len(c.Args) > 0 && c.Args[0] == sbID {
			sandboxDeleteMeta = true
		}
	}
	rec.mu.Unlock()

	if !sandboxCleanup {
		t.Error("expected sandbox Cleanup after last task destroyed")
	}
	if !sandboxDeleteMeta {
		t.Error("expected sandbox DeleteSandboxMetadata after last task destroyed")
	}
}

func TestRecoverMultipleTasksSameAlloc(t *testing.T) {
	d, rec := testDriverWithRecorder(t)

	containerIDs := []string{"kata-alloc-1-web", "kata-alloc-1-sidecar"}
	rec.mu.Lock()
	for _, id := range containerIDs {
		rec.running[id] = true
	}
	rec.mu.Unlock()

	sbID := "kata-alloc-1-sandbox"

	for i, cID := range containerIDs {
		taskName := "web"
		if i == 1 {
			taskName = "sidecar"
		}

		state := &TaskState{
			ContainerID: cID,
			SandboxID:   sbID,
			AllocID:     "alloc-1",
			TaskName:    taskName,
			StartedAt:   time.Now().UnixNano(),
		}

		cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})
		cfg.ID = fmt.Sprintf("task-%d", i)
		cfg.Name = taskName

		handle := drivers.NewTaskHandle(taskHandleVersion)
		handle.Config = cfg
		if err := handle.SetDriverState(state); err != nil {
			t.Fatalf("SetDriverState(%d): %v", i, err)
		}

		if err := d.RecoverTask(handle); err != nil {
			t.Fatalf("RecoverTask(%d): %v", i, err)
		}
	}

	for i := range containerIDs {
		taskID := fmt.Sprintf("task-%d", i)
		h, ok := d.tasks.Get(taskID)
		if !ok {
			t.Errorf("task %s should be in store after recovery", taskID)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ch, err := d.WaitTask(ctx, taskID)
		if err != nil {
			cancel()
			t.Fatalf("WaitTask(%s): %v", taskID, err)
		}
		<-ch
		cancel()
		_ = h
	}

	if rec.callCount("MonitorTask") != 2 {
		t.Errorf("MonitorTask call count = %d, want 2", rec.callCount("MonitorTask"))
	}
}

func TestRecoverThenDestroyTeardownsSandbox(t *testing.T) {
	d, rec := testDriverWithRecorder(t)

	containerIDs := []string{"kata-alloc-1-web", "kata-alloc-1-sidecar"}
	rec.mu.Lock()
	for _, id := range containerIDs {
		rec.running[id] = true
	}
	rec.mu.Unlock()

	sbID := "kata-alloc-1-sandbox"

	taskIDs := make([]string, 2)
	for i, cID := range containerIDs {
		taskName := "web"
		if i == 1 {
			taskName = "sidecar"
		}
		taskIDs[i] = fmt.Sprintf("task-%d", i)

		state := &TaskState{
			ContainerID: cID,
			SandboxID:   sbID,
			AllocID:     "alloc-1",
			TaskName:    taskName,
			StartedAt:   time.Now().UnixNano(),
		}

		cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})
		cfg.ID = taskIDs[i]
		cfg.Name = taskName

		handle := drivers.NewTaskHandle(taskHandleVersion)
		handle.Config = cfg
		if err := handle.SetDriverState(state); err != nil {
			t.Fatalf("SetDriverState: %v", err)
		}
		if err := d.RecoverTask(handle); err != nil {
			t.Fatalf("RecoverTask: %v", err)
		}
	}

	// Destroy first task — sandbox should survive
	if err := d.DestroyTask(taskIDs[0], true); err != nil {
		t.Fatalf("DestroyTask(0): %v", err)
	}

	rec.mu.Lock()
	var earlyCleanup bool
	for _, c := range rec.calls {
		if c.Method == "Cleanup" && len(c.Args) > 0 && c.Args[0] == sbID {
			earlyCleanup = true
		}
	}
	rec.mu.Unlock()
	if earlyCleanup {
		t.Error("sandbox should not be cleaned up while second task exists")
	}

	// Destroy second task — sandbox should tear down
	if err := d.DestroyTask(taskIDs[1], true); err != nil {
		t.Fatalf("DestroyTask(1): %v", err)
	}

	rec.mu.Lock()
	var finalCleanup, finalDeleteMeta bool
	for _, c := range rec.calls {
		if c.Method == "Cleanup" && len(c.Args) > 0 && c.Args[0] == sbID {
			finalCleanup = true
		}
		if c.Method == "DeleteSandboxMetadata" && len(c.Args) > 0 && c.Args[0] == sbID {
			finalDeleteMeta = true
		}
	}
	rec.mu.Unlock()

	if !finalCleanup {
		t.Error("expected sandbox Cleanup after all recovered tasks destroyed")
	}
	if !finalDeleteMeta {
		t.Error("expected sandbox DeleteSandboxMetadata after all recovered tasks destroyed")
	}
}

func testBootstrapJSON(pipePath string) []byte {
	b := map[string]interface{}{
		"static_resources": map[string]interface{}{
			"clusters": []interface{}{
				map[string]interface{}{
					"name": "local_agent",
					"load_assignment": map[string]interface{}{
						"endpoints": []interface{}{
							map[string]interface{}{
								"lb_endpoints": []interface{}{
									map[string]interface{}{
										"endpoint": map[string]interface{}{
											"address": map[string]interface{}{
												"pipe": map[string]interface{}{
													"path": pipePath,
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(b, "", "  ")
	return data
}

func TestRewriteEnvoyBootstrap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "envoy_bootstrap.json")
	if err := os.WriteFile(path, testBootstrapJSON("alloc/tmp/consul_grpc.sock"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := rewriteEnvoyBootstrap(path, "10.0.0.1:8502"); err != nil {
		t.Fatalf("rewriteEnvoyBootstrap: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parsing result: %v", err)
	}

	sr := result["static_resources"].(map[string]interface{})
	clusters := sr["clusters"].([]interface{})
	cluster := clusters[0].(map[string]interface{})
	la := cluster["load_assignment"].(map[string]interface{})
	eps := la["endpoints"].([]interface{})
	lbes := eps[0].(map[string]interface{})["lb_endpoints"].([]interface{})
	addr := lbes[0].(map[string]interface{})["endpoint"].(map[string]interface{})["address"].(map[string]interface{})

	if _, hasPipe := addr["pipe"]; hasPipe {
		t.Error("pipe address should have been removed")
	}
	sa, ok := addr["socket_address"].(map[string]interface{})
	if !ok {
		t.Fatal("expected socket_address in result")
	}
	if sa["address"] != "10.0.0.1" {
		t.Errorf("address = %v, want 10.0.0.1", sa["address"])
	}
	if port, ok := sa["port_value"].(float64); !ok || int(port) != 8502 {
		t.Errorf("port_value = %v, want 8502", sa["port_value"])
	}
}

func TestRewriteEnvoyBootstrapCamelCase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "envoy_bootstrap.json")
	b := map[string]interface{}{
		"static_resources": map[string]interface{}{
			"clusters": []interface{}{
				map[string]interface{}{
					"name": "local_agent",
					"loadAssignment": map[string]interface{}{
						"endpoints": []interface{}{
							map[string]interface{}{
								"lbEndpoints": []interface{}{
									map[string]interface{}{
										"endpoint": map[string]interface{}{
											"address": map[string]interface{}{
												"pipe": map[string]interface{}{
													"path": "alloc/tmp/consul_grpc.sock",
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(b, "", "  ")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	if err := rewriteEnvoyBootstrap(path, "10.0.0.1:8502"); err != nil {
		t.Fatalf("rewriteEnvoyBootstrap: %v", err)
	}

	result, _ := os.ReadFile(path)
	if strings.Contains(string(result), "pipe") {
		t.Error("pipe address should have been removed")
	}
	if !strings.Contains(string(result), "socket_address") {
		t.Error("should contain socket_address after rewrite")
	}
	if !strings.Contains(string(result), "10.0.0.1") {
		t.Error("should contain the consul address")
	}
}

func TestRewriteEnvoyBootstrapNoLocalAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "envoy_bootstrap.json")
	original := []byte(`{"static_resources":{"clusters":[{"name":"other_cluster"}]}}`)
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}

	if err := rewriteEnvoyBootstrap(path, "10.0.0.1:8502"); err != nil {
		t.Fatalf("rewriteEnvoyBootstrap: %v", err)
	}

	data, _ := os.ReadFile(path)
	if string(data) != string(original) {
		t.Error("file should not have been modified")
	}
}

func TestRewriteEnvoyBootstrapNoPipe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "envoy_bootstrap.json")
	b := map[string]interface{}{
		"static_resources": map[string]interface{}{
			"clusters": []interface{}{
				map[string]interface{}{
					"name": "local_agent",
					"load_assignment": map[string]interface{}{
						"endpoints": []interface{}{
							map[string]interface{}{
								"lb_endpoints": []interface{}{
									map[string]interface{}{
										"endpoint": map[string]interface{}{
											"address": map[string]interface{}{
												"socket_address": map[string]interface{}{
													"address":    "10.0.0.1",
													"port_value": 8502,
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	original, _ := json.MarshalIndent(b, "", "  ")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}

	if err := rewriteEnvoyBootstrap(path, "10.0.0.1:8502"); err != nil {
		t.Fatalf("rewriteEnvoyBootstrap: %v", err)
	}

	data, _ := os.ReadFile(path)
	if string(data) != string(original) {
		t.Error("file should not have been modified when no pipe address exists")
	}
}

func TestStartTaskRewritesBootstrap(t *testing.T) {
	d, rec := testDriverWithRecorder(t)
	d.config.ConsulGRPCAddr = "10.0.0.1:8502"

	allocDir := t.TempDir()
	taskCfg := &TaskConfig{Image: "envoyproxy/envoy:v1.34.7"}
	cfg := &drivers.TaskConfig{
		ID:            "test-task-id",
		AllocID:       "alloc-1",
		Name:          "connect-proxy",
		TaskGroupName: "group",
		AllocDir:      allocDir,
	}
	if err := cfg.EncodeConcreteDriverConfig(taskCfg); err != nil {
		t.Fatalf("encoding driver config: %v", err)
	}

	secretsDir := filepath.Join(allocDir, "connect-proxy", "secrets")
	if err := os.MkdirAll(secretsDir, 0755); err != nil {
		t.Fatal(err)
	}
	bootstrapPath := filepath.Join(secretsDir, "envoy_bootstrap.json")
	if err := os.WriteFile(bootstrapPath, testBootstrapJSON("alloc/tmp/consul_grpc.sock"), 0644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	_ = rec

	data, err := os.ReadFile(bootstrapPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "pipe") {
		t.Error("bootstrap should have been rewritten to remove pipe address")
	}
	if !strings.Contains(string(data), "socket_address") {
		t.Error("bootstrap should contain socket_address after rewrite")
	}
}

func TestStartTaskSkipsRewriteWithoutBootstrap(t *testing.T) {
	d, _ := testDriverWithRecorder(t)
	d.config.ConsulGRPCAddr = "10.0.0.1:8502"

	cfg := testTaskConfig(t, &TaskConfig{Image: "alpine:latest"})

	handle, _, err := d.StartTask(cfg)
	if err != nil {
		t.Fatalf("StartTask should succeed without bootstrap file: %v", err)
	}
	if handle == nil {
		t.Fatal("expected non-nil handle")
	}
}
func TestMergeCommand(t *testing.T) {
	tests := []struct {
		name     string
		ep       []string
		cmd      []string
		taskCmd  string
		taskArgs []string
		want     []string
	}{
		{
			name: "image entrypoint and cmd",
			ep:   []string{"/init"},
			cmd:  []string{"start"},
			want: []string{"/init", "start"},
		},
		{
			name:    "command overrides cmd, entrypoint preserved",
			ep:      []string{"/init"},
			cmd:     []string{"start"},
			taskCmd: "run",
			want:    []string{"/init", "run"},
		},
		{
			name:     "command with args, entrypoint preserved",
			ep:       []string{"/init"},
			cmd:      []string{"start"},
			taskCmd:  "run",
			taskArgs: []string{"--flag"},
			want:     []string{"/init", "run", "--flag"},
		},
		{
			name:     "args only overrides cmd",
			ep:       []string{"/init"},
			cmd:      []string{"start"},
			taskArgs: []string{"serve"},
			want:     []string{"/init", "serve"},
		},
		{
			name:    "empty entrypoint, command set",
			cmd:     []string{"python", "app.py"},
			taskCmd: "/init",
			want:    []string{"/init"},
		},
		{
			name: "empty entrypoint, no override",
			cmd:  []string{"python", "app.py"},
			want: []string{"python", "app.py"},
		},
		{
			name:    "empty entrypoint, no double prepend",
			taskCmd: "/init",
			want:    []string{"/init"},
		},
		{
			name:     "multi-element entrypoint",
			ep:       []string{"/bin/sh", "-c"},
			cmd:      []string{"echo hello"},
			taskArgs: []string{"echo bye"},
			want:     []string{"/bin/sh", "-c", "echo bye"},
		},
		{
			name: "all empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeCommand(tt.ep, tt.cmd, tt.taskCmd, tt.taskArgs)
			if !slices.Equal(got, tt.want) {
				t.Errorf("mergeCommand(%v, %v, %q, %v) = %v, want %v",
					tt.ep, tt.cmd, tt.taskCmd, tt.taskArgs, got, tt.want)
			}
		})
	}
}

func TestStartTaskEntrypoint(t *testing.T) {
	tests := []struct {
		name    string
		imgCfg  ocispec.ImageConfig
		taskCfg TaskConfig
		wantCmd []string
	}{
		{
			name: "no override uses image entrypoint and cmd",
			imgCfg: ocispec.ImageConfig{
				Entrypoint: []string{"/entrypoint.sh"},
				Cmd:        []string{"default-arg"},
			},
			taskCfg: TaskConfig{Image: "test:latest"},
			wantCmd: []string{"/entrypoint.sh", "default-arg"},
		},
		{
			name: "command override preserves entrypoint",
			imgCfg: ocispec.ImageConfig{
				Entrypoint: []string{"/entrypoint.sh"},
				Cmd:        []string{"default-arg"},
			},
			taskCfg: TaskConfig{Image: "test:latest", Command: "custom-cmd", Args: []string{"--flag"}},
			wantCmd: []string{"/entrypoint.sh", "custom-cmd", "--flag"},
		},
		{
			name: "args only override replaces cmd",
			imgCfg: ocispec.ImageConfig{
				Entrypoint: []string{"/entrypoint.sh"},
				Cmd:        []string{"default-arg"},
			},
			taskCfg: TaskConfig{Image: "test:latest", Args: []string{"override-arg"}},
			wantCmd: []string{"/entrypoint.sh", "override-arg"},
		},
		{
			name: "empty entrypoint image",
			imgCfg: ocispec.ImageConfig{
				Cmd: []string{"python", "app.py"},
			},
			taskCfg: TaskConfig{Image: "test:latest", Command: "bash"},
			wantCmd: []string{"bash"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, rec := testDriverWithRecorder(t)
			rec.imageConfig = tt.imgCfg
			cfg := testTaskConfig(t, &tt.taskCfg)
			_, _, err := d.StartTask(cfg)
			if err != nil {
				t.Fatalf("StartTask: %v", err)
			}
			cc := rec.lastConfig()
			if cc == nil {
				t.Fatal("no ContainerConfig recorded")
			}
			if !slices.Equal(cc.Command, tt.wantCmd) {
				t.Errorf("Command = %v, want %v", cc.Command, tt.wantCmd)
			}
		})
	}
}
