# Copyright 2023 Ross Light
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# SPDX-License-Identifier: Apache-2.0

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
