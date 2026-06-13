package kata

import (
	"context"
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
	if cc.Image != "alpine:latest" {
		t.Errorf("Image = %q, want %q", cc.Image, "alpine:latest")
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
