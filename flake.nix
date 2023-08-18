{
  description = "Jupyter kernel for Ivy";

  inputs = {
    nixpkgs.url = "nixpkgs";
    flake-utils.url = "flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils, ... }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
      in
      {
        packages.default = pkgs.callPackage ./package.nix {
          buildGoModule = pkgs.buildGo120Module;
        };

        apps.default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/jupyter-ivy";
        };

        devShells.default = pkgs.mkShell {
          packages = [
            (pkgs.python3.withPackages (ps: [
              ps.notebook
              ps.jupyter_console
            ]))
          ];

          inputsFrom = [
            self.packages.${system}.default
          ];
        };
      }
    );
}
