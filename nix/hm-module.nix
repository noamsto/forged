self: {
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.services.forged;
in {
  options.services.forged = {
    enable = lib.mkEnableOption "the forged PR state cache daemon";
    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "forged.packages.\${system}.default";
      description = "The forged package to run.";
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
      description = "Extra arguments appended to `forged serve`.";
    };
  };

  config = lib.mkIf cfg.enable {
    home.packages = [cfg.package];

    systemd.user.sockets.forged = {
      Unit.Description = "forged PR state cache socket";
      Socket = {
        ListenStream = "%t/forged/forged.sock";
        SocketMode = "0600";
        DirectoryMode = "0700";
      };
      Install.WantedBy = ["sockets.target"];
    };

    systemd.user.services.forged = {
      Unit = {
        Description = "forged PR state cache daemon";
        Requires = ["forged.socket"];
      };
      Service = {
        Environment = "PATH=${lib.makeBinPath [cfg.ghPackage]}";
        ExecStart = lib.escapeShellArgs (["${cfg.package}/bin/forged" "serve"] ++ cfg.extraArgs);
        Restart = "on-failure";
      };
    };
  };
}
