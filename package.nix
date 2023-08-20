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

{ lib
, buildGoModule
, nix-gitignore
, zeromq
, pkg-config
}:

buildGoModule {
  name = "jupyter-ivy";

  src = let
    root = ./.;
    patterns = nix-gitignore.withGitignoreFile extraIgnores root;
    extraIgnores = [ ".github" ".vscode" "*.nix" "flake.lock" ];
  in builtins.path {
    name = "jupyter-ivy-source";
    path = root;
    filter = nix-gitignore.gitignoreFilterPure (_: _: true) patterns root;
  };

  vendorHash = "sha256-LQypp9zfYF7TBsv/TnAQc+nHggWuLV35H8+6+cIXFwQ=";

  ldflags = [ "-s" "-w" ];

  buildInputs = [
    zeromq
  ];

  nativeBuildInputs = [
    pkg-config
  ];

  postInstall = ''
    mkdir -p $out/share/jupyter/kernels/ivy
    {
      echo '{'
      echo "  \"argv\": [\"$out/bin/jupyter-ivy\", \"--\", \"{connection_file}\"],"
      echo '  "display_name": "Ivy",'
      echo '  "language": "ivy"'
      echo '}'
    } > $out/share/jupyter/kernels/ivy/kernel.json
  '';

  meta = {
    description = "A Jupyter kernel for the Ivy programming language";
    homepage = "https://github.com/zombiezen/jupyter-ivy";
    license = lib.licenses.asl20;
    maintainers = [ lib.maintainers.zombiezen ];
  };
}
