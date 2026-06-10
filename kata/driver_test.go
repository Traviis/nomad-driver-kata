package kata

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
)

func testDriver() *Driver {
	d := NewDriver(hclog.NewNullLogger()).(*Driver)
	return d
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

func TestMountString(t *testing.T) {
	tests := []struct {
		name        string
		src, dst    string
		readonly    bool
		propagation string
		wantSuffix  string
	}{
		{"default", "/src", "/dst", false, "", "options=rbind:rprivate"},
		{"readonly", "/src", "/dst", true, "", "options=rbind:rprivate:ro"},
		{"file bind", "/src", "/dst", true, "file", "options=bind:ro"},
		{"bidirectional", "/src", "/dst", false, "bidirectional", "options=rbind:rshared"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mountString(tt.src, tt.dst, tt.readonly, tt.propagation)
			if !strings.HasPrefix(got, "type=bind,src=/src,dst=/dst,") {
				t.Errorf("unexpected prefix: %s", got)
			}
			if !strings.HasSuffix(got, tt.wantSuffix) {
				t.Errorf("mountString() = %q, want suffix %q", got, tt.wantSuffix)
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
