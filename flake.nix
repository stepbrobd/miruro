{
  outputs = inputs: inputs.parts.lib.mkFlake { inherit inputs; } {
    systems = import inputs.systems;

    perSystem = { lib, pkgs, system, self', ... }: {
      _module.args = lib.fix (self: {
        lib = with inputs; builtins // nixpkgs.lib // parts.lib;
        pkgs = import inputs.nixpkgs {
          inherit system;
          overlays = [ inputs.gomod2nix.overlays.default ];
        };
      });

      packages.default = pkgs.buildGoApplication (lib.fix (finalAttrs: {
        __structuredAttrs = true;
        __darwinAllowLocalNetworking = true;
        pname = "miruro";
        meta.mainProgram = finalAttrs.pname;
        version = lib.fileContents ./version.txt;
        src = with lib.fileset; toSource {
          root = ./.;
          fileset = unions [
            (fileFilter (file: file.hasExt "go") ./.)
            ./go.mod
            ./go.sum
          ];
        };
        modules = ./gomod2nix.toml;
        subPackages = [ "cmd/miruro" ];
        ldflags = [ "-X" "main.version=${finalAttrs.version}" ];
        nativeBuildInputs = [ pkgs.installShellFiles ];
        postInstall = ''
          for shell in bash zsh fish; do
            installShellCompletion --cmd ${finalAttrs.pname} --''${shell} <("$out/bin/${finalAttrs.pname}" completion "$shell")
          done
        '';
      }));

      devShells.default = pkgs.mkShell {
        inputsFrom = lib.attrValues self'.packages;
        packages = with pkgs; [
          deno
          nixpkgs-fmt

          ffmpeg

          go
          go-tools
          gomod2nix
          gopls
        ];
      };

      formatter = pkgs.writeShellScriptBin "formatter" ''
        set -eoux pipefail
        shopt -s globstar

        root="$PWD"
        while [[ ! -f "$root/.git/index" ]]; do
          if [[ "$root" == "/" ]]; then
            exit 1
          fi
          root="$(dirname "$root")"
        done
        pushd "$root" > /dev/null

        ${lib.getExe pkgs.deno} fmt **/*.md
        ${lib.getExe pkgs.nixpkgs-fmt} .

        ${lib.getExe pkgs.go} fix ./...
        ${lib.getExe pkgs.go} fmt ./...
        ${lib.getExe pkgs.go} mod tidy
        ${lib.getExe pkgs.go} test -race ./...
        ${lib.getExe pkgs.go} vet ./...
        ${lib.getExe' pkgs.go-tools "staticcheck"} ./...
        ${lib.getExe' pkgs.gomod2nix "gomod2nix"}

        popd
      '';
    };
  };

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixpkgs-unstable";
  inputs.systems.url = "github:nix-systems/default";
  inputs.parts.url = "github:hercules-ci/flake-parts";
  inputs.parts.inputs.nixpkgs-lib.follows = "nixpkgs";
  inputs.utils.url = "github:numtide/flake-utils";
  inputs.utils.inputs.systems.follows = "systems";
  inputs.gomod2nix.url = "github:nix-community/gomod2nix";
  inputs.gomod2nix.inputs.nixpkgs.follows = "nixpkgs";
  inputs.gomod2nix.inputs.flake-utils.follows = "utils";
}
