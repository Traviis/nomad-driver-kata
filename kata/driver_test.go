package kata

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
)

func testDriver() *Driver {
	d := NewDriver(hclog.NewNullLogger()).(*Driver)
	return d
}

func testDriverWithRecorder() (*Driver, *recorder) {
	rec := newRecorder()
	d := NewDriver(hclog.NewNullLogger()).(*Driver)
	d.ctr = rec
	d.config = &PluginConfig{
		ContainerdAddr: "/test.sock",
		Namespace:      defaultNamespace,
		PauseImage:     defaultPauseImage,
		Runtime:        defaultRuntime,
	}
	d.sandboxMgr = NewSandboxManager(rec, d.logger)
	d.eventer = eventer.NewEventer(d.ctx, d.logger)
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

func TestWriteHosts(t *testing.T) {
	d := testDriver()
	path := filepath.Join(t.TempDir(), "hosts")

	if err := d.writeHosts(path, nil); err != nil {
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
	if err := d.writeHosts(path, hosts); err != nil {
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
	d, _ := testDriverWithRecorder()
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
