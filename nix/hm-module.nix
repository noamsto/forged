self: {
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.services.forge-cache;
in {
  options.services.forge-cache = {
    enable = lib.mkEnableOption "the forge-cache PR state cache daemon";
    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "forge-cache.packages.\${system}.default";
      description = "The forge-cache package to run.";
    };
    ghPackage = lib.mkOption {
      type = lib.types.package;
      default = pkgs.gh;
      defaultText = lib.literalExpression "pkgs.gh";
      description = "The gh package put on the daemon's PATH.";
    };
    extraArgs = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [];
      description = "Extra arguments appended to `forge-cache serve`.";
    };
  };

  config = lib.mkIf cfg.enable {
    home.packages = [cfg.package];

    systemd.user.sockets.forge-cache = {
      Unit.Description = "forge-cache PR state cache socket";
      Socket = {
        ListenStream = "%t/forge-cache/forge-cache.sock";
        SocketMode = "0600";
        DirectoryMode = "0700";
      };
      Install.WantedBy = ["sockets.target"];
    };

    systemd.user.services.forge-cache = {
      Unit = {
        Description = "forge-cache PR state cache daemon";
        Requires = ["forge-cache.socket"];
      };
      Service = {
        Environment = "PATH=${lib.makeBinPath [cfg.ghPackage]}";
        ExecStart = lib.escapeShellArgs (["${cfg.package}/bin/forge-cache" "serve"] ++ cfg.extraArgs);
        Restart = "on-failure";
      };
    };
  };
}
