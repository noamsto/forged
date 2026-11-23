{
  description = "Shared, forge-agnostic PR state cache daemon";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    treefmt-nix.url = "github:numtide/treefmt-nix";
    treefmt-nix.inputs.nixpkgs.follows = "nixpkgs";
    git-hooks-nix.url = "github:cachix/git-hooks.nix";
    git-hooks-nix.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs = inputs @ {
    flake-parts,
    self,
    ...
  }:
    flake-parts.lib.mkFlake {inherit inputs;} {
      systems = ["x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin"];

      imports = [
        inputs.treefmt-nix.flakeModule
        inputs.git-hooks-nix.flakeModule
      ];

      flake.homeManagerModules.default = import ./nix/hm-module.nix self;

      perSystem = {
        pkgs,
        config,
        ...
      }: let
        # The same dependency pin the binary is built with. buildGoModule wires
        # GOPATH/GOMODCACHE to a fetched, read-only module cache, so the
        # static-analysis checks run offline in the Nix sandbox.
        vendorHash = "sha256-RkmZ4qyOWyGaQkgmR1Ak2K0NLZa/7C4wfivddmPPJyc=";
        version = "0.1.0";
      in {
        packages.forge-cache = pkgs.buildGoModule {
          pname = "forge-cache";
          inherit version;
          src = ./.;
          inherit vendorHash;
          # checks.go-gate runs the suite.
          doCheck = false;
          ldflags = ["-s" "-w" "-X github.com/noamsto/forge-cache/internal/cli.version=${version}"];
          postInstall = ''
            install -Dm644 packaging/systemd/forge-cache.socket packaging/systemd/forge-cache.service \
              -t $out/share/systemd/user
            substituteInPlace $out/share/systemd/user/forge-cache.service --subst-var out
          '';
          meta = {
            description = "Shared, forge-agnostic PR state cache daemon";
            mainProgram = "forge-cache";
          };
        };
        packages.default = config.packages.forge-cache;

        # The whole Go gate in one derivation, so its steps share one GOCACHE
        # instead of each compiling every dependency from scratch. It reuses
        # packages.forge-cache's module cache (same src + vendorHash) and runs in
        # checkPhase, so `nix flake check` enforces it locally and in CI.
        checks.go-gate = pkgs.buildGoModule {
          pname = "forge-cache-go-gate";
          version = "0.0.0";
          src = ./.;
          inherit vendorHash;
          nativeCheckInputs = [pkgs.git pkgs.golangci-lint pkgs.nilaway];
          # -race needs cgo; the stdenv provides the C toolchain. Every step
          # builds with it so they all hit the same cache entries.
          env.CGO_ENABLED = "1";
          buildPhase = "true";
          checkPhase = ''
            runHook preCheck
            export HOME="$TMPDIR"
            export GOCACHE="$TMPDIR/go-cache"
            # buildGoModule's -trimpath breaks tests that read on-disk assets;
            # drop it for the same reason its own checkPhase does.
            export GOFLAGS=''${GOFLAGS//-trimpath/}
            golangci-lint run ./...
            nilaway -include-pkgs=github.com/noamsto/forge-cache ./...
            go test ./...
            go test -race ./...
            runHook postCheck
          '';
          installPhase = "touch $out";
          meta.description = "forge-cache Go gate: golangci-lint, nilaway, go test, go test -race";
        };

        treefmt = {
          projectRootFile = "flake.nix";
          programs = {
            alejandra.enable = true;
            gofmt.enable = true;
          };
        };

        pre-commit.settings.hooks = {
          statix.enable = true;
          deadnix.enable = true;
          alejandra.enable = true;
          typos.enable = true;
          check-merge-conflicts.enable = true;
          trim-trailing-whitespace.enable = true;
        };

        devShells.default = pkgs.mkShell {
          inherit (config.pre-commit) shellHook;
          packages =
            config.pre-commit.settings.enabledPackages
            ++ [
              pkgs.go
              pkgs.gopls
              pkgs.gotools
              pkgs.golangci-lint
              pkgs.nilaway
              config.treefmt.build.wrapper
            ];
        };
      };
    };
}
