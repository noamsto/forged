self: {
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.services.forged;
  tomlFormat = pkgs.formats.toml {};
  hooksFile = tomlFormat.generate "hooks.toml" cfg.hooks.settings;
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
    hooks = {
      enable = lib.mkEnableOption "the forged hooks runner, which runs local commands on forge events";
      settings = lib.mkOption {
        inherit (tomlFormat) type;
        default = {};
        example = lib.literalExpression ''
          {
            rule = [
              {
                name = "notify-merged";
                repos = ["owner/repo"];
                on = "state.changed";
                to = "merged";
                command = ["notify-send" "--" "{repo}#{number}" "merged"];
              }
              {
                name = "clean-worktree";
                repos = ["owner/repo"];
                builtin = "remove-merged-worktree";
                checkout = "~/git/owner/repo";
              }
            ];
          }
        '';
        description = ''
          Contents of `forged/hooks.toml`: an optional `viewer` plus a list
          of `rule` tables. Each rule names the `repos` and event (`on`) it
          matches, and either a `command` argv or a `builtin` with its `checkout`.
        '';
      };
      extraPackages = lib.mkOption {
        type = lib.types.listOf lib.types.package;
        default = [];
        description = "Packages added to the hooks runner's PATH.";
      };
      gitPackage = lib.mkOption {
        type = lib.types.package;
        default = pkgs.git;
        defaultText = lib.literalExpression "pkgs.git";
        description = "The git package put on the hooks runner's PATH.";
      };
    };
  };

  config = lib.mkMerge [
    (lib.mkIf cfg.enable {
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
    })

    (lib.mkIf cfg.hooks.enable {
      assertions = [
        {
          assertion = cfg.enable;
          message = "services.forged.hooks.enable requires services.forged.enable = true.";
        }
      ];

      xdg.configFile."forged/hooks.toml".source = hooksFile;

      systemd.user.services.forged-hooks = {
        Unit = {
          Description = "forged hooks runner";
          Requires = ["forged.socket"];
          After = ["forged.socket"];
          "X-Restart-Triggers" = [hooksFile];
        };
        Service = {
          Environment = "PATH=${lib.makeBinPath ([cfg.ghPackage cfg.hooks.gitPackage] ++ cfg.hooks.extraPackages)}";
          ExecStart = lib.escapeShellArgs ["${cfg.package}/bin/forged" "hooks" "--config" "${config.xdg.configHome}/forged/hooks.toml"];
          Restart = "on-failure";
          RestartPreventExitStatus = 2;
          RestartSec = 10;
        };
        Install.WantedBy = ["default.target"];
      };
    })
  ];
}
