{
  description = "ElasticClaw development environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in {
        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go_1_26
            gopls
            gotools
            golangci-lint
            git       # required for go VCS stamping during build
          ];

          shellHook = ''
            echo "ElasticClaw dev — $(go version)"  # go 1.26.x required
          '';
        };
      });
}
