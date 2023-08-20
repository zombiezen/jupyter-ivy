{ buildGoModule
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

  vendorHash = "sha256-dQ0Ni2AnK0fO7/5YxYvHQ9SBbiSOXNbPaE/kkssoDoI=";

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
}
