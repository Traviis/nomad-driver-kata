{ config, lib, ... }:
let
  cfg = config.services.nomad-driver-kata;
  driverPkg = cfg.package;
in {
  options.services.nomad-driver-kata = {
    enable = lib.mkEnableOption "Kata Containers task driver for Nomad";

    package = lib.mkOption {
      type = lib.types.package;
      description = "The nomad-driver-kata package to install.";
    };

    containerdAddr = lib.mkOption {
      type = lib.types.str;
      default = "/run/docker/containerd/containerd.sock";
      description = "Path to the containerd socket.";
    };

    namespace = lib.mkOption {
      type = lib.types.str;
      default = "default";
      description = "containerd namespace for Kata containers.";
    };

    runtime = lib.mkOption {
      type = lib.types.str;
      default = "io.containerd.kata.v2";
      description = "Kata shimv2 runtime identifier.";
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.tmpfiles.rules = [
      "d /opt/nomad/plugins 0755 root root - -"
      "L+ /opt/nomad/plugins/nomad-driver-kata - - - - ${driverPkg}/bin/nomad-driver-kata"
    ];

    services.nomad.settings = {
      plugin_dir = "/opt/nomad/plugins";

      plugin."nomad-driver-kata" = {
        config = {
          containerd_addr = cfg.containerdAddr;
          namespace = cfg.namespace;
          runtime = cfg.runtime;
        };
      };
    };
  };
}
