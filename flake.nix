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
            go_1_24   # update to go_1_26 when nixpkgs unstable ships it
            gopls
            gotools
            golangci-lint
            sqlite
            gcc       # required for go-sqlite3 (cgo)
          ];

          shellHook = ''
            echo "ElasticClaw dev — $(go version)"
          '';
        };
      });
}
