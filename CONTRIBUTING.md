# Hacking on the Jupyter kernel for Ivy

As stated in [the README](README.md), I made this primarily for my personal use.
Therefore, I'm not looking for contributions per se, but I'll probably accept bug fixes.
I'm using [Nix][] and [direnv][] to provide a reproducible development environment.
To try out a development build, run this:

```shell
nix build && JUPYTER_PATH=$(pwd)/result/share/jupyter jupyter console --kernel ivy
```

[direnv]: https://direnv.net/
[Nix]: https://nixos.org/
