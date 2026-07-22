# NixOS VM integration test. Boots a full NixOS guest under QEMU (as your
# unprivileged user — no sudo on the host) and runs the shared assertion body
# (tests/verify.nix) against a real containerd + Kata + Nomad stack inside it.
#
# Kata boots each task in a KVM microVM, so this test needs NESTED KVM: the
# guest is launched with `-cpu host` so it sees the host's virtualization
# extensions. On a host without nested virt the QEMU guest simply cannot boot
# Kata's microVMs and the test will fail at the fingerprint/allocation stage —
# that is a host-capability limitation, not a driver defect. Gate invocation on
# the runtime nested-virt probe (see flake `checks`).
#
# The job specifications (tests/jobs.nix) and the assertion body
# (tests/verify.nix) are shared byte-for-byte with the sudo-based script
# (tests/integration.nix).
{ pkgs, driverPkg }:

let
  jobs = import ./jobs.nix { inherit pkgs; };
  verify = import ./verify.nix { inherit pkgs; };

  containerdSock = "/run/containerd/containerd.sock";
  nomadAddr = "http://127.0.0.1:14646";

  # nixosTest guests have no network, so the driver cannot pull images. Build the
  # two images the jobs reference locally and import them into the guest
  # containerd before verify runs. The driver's EnsureImage checks GetImage
  # first and skips the pull when the image is already present, so no network is
  # ever needed. Tags are chosen so containerd stores them under the exact
  # normalized refs the jobs request (docker.io/library/busybox:latest and
  # registry.k8s.io/pause:3.9).
  busyboxImage = pkgs.dockerTools.buildImage {
    name = "docker.io/library/busybox";
    tag = "latest";
    copyToRoot = pkgs.buildEnv {
      name = "busybox-root";
      paths = [ pkgs.busybox ];
      pathsToLink = [ "/bin" ];
    };
    config.Cmd = [ "/bin/sh" ];
  };

  pauseImage = pkgs.dockerTools.buildImage {
    name = "registry.k8s.io/pause";
    tag = "3.9";
    copyToRoot = pkgs.buildEnv {
      name = "pause-root";
      paths = [ pkgs.busybox ];
      pathsToLink = [ "/bin" ];
    };
    config.Cmd = [
      "/bin/sh"
      "-c"
      "sleep infinity"
    ];
  };

in
pkgs.testers.runNixOSTest {
  name = "nomad-driver-kata-integration";

  nodes.machine =
    {
      lib,
      pkgs,
      ...
    }:
    {
      imports = [ ../module.nix ];

      # 4 cores / 4 GiB gives Kata room to boot several microVMs. `-cpu host`
      # exposes the host's virtualization extensions to the guest (nested KVM),
      # without which Kata cannot boot its microVMs.
      virtualisation = {
        cores = 4;
        memorySize = 4096;
        diskSize = 8192;
        qemu.options = [
          "-cpu"
          "host"
        ];
      };

      # containerd with the Kata shimv2 runtime registered.
      virtualisation.containerd.enable = true;
      virtualisation.containerd.settings = {
        version = lib.mkForce 3;
        plugins."io.containerd.cri.v1.runtime".containerd.runtimes."kata" = {
          runtime_type = "io.containerd.kata.v2";
          privileged_without_host_devices = true;
        };
      };

      # containerd resolves containerd-shim-kata-v2 through its own PATH, not
      # the PATH of a later Nomad task. The host integration script prepends
      # kata-runtime before starting containerd; do the equivalent here.
      systemd.services.containerd.path = [ pkgs.kata-runtime ];

      # Nomad single-node server+client. dropPrivileges = false runs the agent
      # as root, which the Kata driver needs to reach root-owned containerd and
      # to manage cgroups/mounts/netns — matching the sudo script's execution
      # context. With the default (true) nomad runs as an unprivileged user and
      # the driver cannot reach containerd.
      services.nomad = {
        enable = true;
        enableDocker = false;
        dropPrivileges = false;
        extraSettingsPlugins = [ driverPkg ];
        settings = {
          log_level = "INFO";
          bind_addr = "127.0.0.1";
          ports = {
            http = 14646;
            rpc = 14647;
            serf = 14648;
          };
          advertise = {
            http = "127.0.0.1";
            rpc = "127.0.0.1";
            serf = "127.0.0.1";
          };
          server = {
            enabled = true;
            bootstrap_expect = 1;
          };
          client = {
            enabled = true;
            cni_path = "${pkgs.cni-plugins}/bin";
            cni_config_dir = "/etc/cni/net.d";
          };
        };
      };

      # The Kata driver plugin, via the shared module.
      services.nomad-driver-kata = {
        enable = true;
        package = driverPkg;
        containerdAddr = containerdSock;
        namespace = "default";
        pauseImage = "registry.k8s.io/pause:3.9";
        runtime = "io.containerd.kata.v2";
      };

      # Tools the verify body and image import need on PATH inside the guest, plus
      # Kata itself and the networking helpers Nomad's bridge mode requires.
      environment.systemPackages = with pkgs; [
        containerd
        kata-runtime
        nomad
        jq
        cni-plugins
        iptables
      ];

      # Nomad's bridge fingerprint requires bridge before the agent starts;
      # vhost modules support Kata's vsock/network path.
      boot.kernelModules = [
        "bridge"
        "br_netfilter"
        "vhost_vsock"
        "vhost_net"
        "tun"
        "kvm"
      ];
    };

  testScript = ''
    start_all()

    with subtest("containerd is up"):
        machine.wait_for_unit("containerd.service")
        machine.wait_until_succeeds("ctr -a ${containerdSock} version")

    with subtest("import images offline"):
        machine.succeed("ctr -a ${containerdSock} image import ${busyboxImage}")
        machine.succeed("ctr -a ${containerdSock} image import ${pauseImage}")

    with subtest("nomad is up and the kata driver is detected"):
        machine.wait_for_unit("nomad.service")
        machine.wait_until_succeeds("nomad node status -address=${nomadAddr}")

    with subtest("run shared integration assertions"):
        machine.succeed(
            "env "
            "NOMAD_ADDR=${nomadAddr} "
            "CONTAINERD_SOCK=${containerdSock} "
            "SINGLE_JOB=${jobs.single} "
            "MULTI_VM_JOB=${jobs.multiVm} "
            "${verify}",
            timeout=600,
        )
  '';
}
