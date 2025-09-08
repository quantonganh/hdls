# hdls
A simple language server for HDL, inspired by [nand2tetris](https://www.nand2tetris.org/).

## Installation

### Install via Homebrew

```
brew install quantonganh/tap/hdls
```

### Install via Go

```sh
$ go install github.com/quantonganh/hdls@latest
```

Alternatively, you can download the latest binary from the [release page](https://github.com/quantonganh/hdls/releases).

## Usage

Add the following to your `~/.config/helix/languages.toml` file:

```toml
[[language]]
name = "hdl"
scope = "source.hdl"
file-types = ["hdl"]
comment-token = "//"
block-comment-tokens = { start = "/*", end = "*/" }
language-servers = [ "hdls" ]

[[grammar]]
name = "hdl"
source = { git = "https://github.com/quantonganh/tree-sitter-hdl", rev="adcb20742ffecbffb2851dce83de37e6594f3ae8" }

[language-server.hdls]
command = "hdls"
```
