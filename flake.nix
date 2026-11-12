{
  description = "dependency-controller - development environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";

    go-overlay = {
      url = "github:purpleclay/go-overlay";
      inputs.nixpkgs.follows = "nixpkgs";
      inputs.flake-utils.follows = "flake-utils";
    };

    dev-kit = {
      url = "github:opendefensecloud/dev-kit/v4.0.0";
      inputs.nixpkgs.follows = "nixpkgs";
      inputs.go-overlay.follows = "go-overlay";
      inputs.flake-utils.follows = "flake-utils";
    };
  };

  outputs = { flake-utils, dev-kit, ... }:
    flake-utils.lib.eachDefaultSystem (system:
      {
        devShells.default = dev-kit.lib.mkShell {
          inherit system;
          goVersion = "1.27.4";
          # Private module host: skip the public proxy/sumdb for our GitLab modules.
          env.GOPRIVATE = "gitlab.opendefense.cloud";
          # dev-kit's hook ends on `[[ -f …pre-commit.legacy ]] && rm`, which exits 1
          # when the file is absent and fails `direnv reload`; end on a command that succeeds.
          shellHook = ''
            export IN_NIX_SHELL=1
          '';
        };
      }
    );
}
