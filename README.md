# lsp

Language servers for [rhun](https://rhun.app): diagnostics in the gutter and the status bar,
hover text when the pointer rests on a symbol, and **Go to Definition** (F12). One server per
language per project, started when the first document of the language opens, run from the
project folder. Nothing is downloaded: the extension runs the server you already have on your
`PATH`:

| Language | Server |
| --- | --- |
| Go | gopls |
| Rust | rust-analyzer |
| TypeScript, JavaScript | typescript-language-server |
| Python | pyright-langserver |
| C, C++ | clangd |
| Zig | zls |
| Lua | lua-language-server |
| Ruby | solargraph |
| Dart | dart language-server |
| Kotlin | kotlin-language-server |
| Swift | sourcekit-lsp |
| OCaml | ocamllsp |
| Haskell | haskell-language-server-wrapper |
| Elixir | elixir-ls |
| Nix | nil |
| Terraform | terraform-ls |
| YAML, JSON, HTML, CSS | the vscode language servers, yaml-language-server |

A missing server shows once in the status bar ("gopls not installed") and the language is left
alone. **Restart Language Servers** stops them all and starts them again for the open documents.

The extension keeps every open document in step with rhun through the edits stream (one
incremental `didChange` per frame) and converts positions between rhun's byte columns and the
protocol's UTF-16 units, so marks land on the right characters in non-ASCII lines.

## Settings

`$RHUN_STATE/lsp.ini` (rhun shows the folder in Settings > Extensions) overrides a server
command per language, by rhun's grammar name:

```ini
[servers]
Python = pylsp
Go = gopls -remote=auto
```

## Needs

Documents: read (the text, for the server; marks and hover text go back through rhun). Project:
read (the server reads the project). Run: the servers above, by name; no network.

## Build and test

`tests/run.sh [RHUN]` builds the extension and a fake language server, points the extension at
the fake through `lsp.ini`, lays everything out as rhun installs it and runs `tests/basic.txt`
headlessly: diagnostics, marks following an edit, incremental sync agreeing with the editor,
hover, and go to definition. Releases are built by the reusable workflow from
`rhun-editor/extension-template`.
