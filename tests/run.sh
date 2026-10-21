#!/bin/sh
# Builds lsp and the fake language server, points the extension at the fake through lsp.ini in
# its state folder, lays it out as rhun installs it, and runs tests/basic.txt headlessly against a
# rhun binary. usage: tests/run.sh [RHUN]
set -e
cd "$(dirname "$0")/.."
RHUN=${1:-$(command -v rhun || echo "$HOME/.local/bin/rhun")}
case "$(uname -s)-$(uname -m)" in
    Linux-x86_64) target=linux-x86_64 ;;
    Darwin-arm64) target=macos-arm64 ;;
    *) echo "no target for $(uname -s)-$(uname -m)"; exit 77 ;;
esac
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
dir=$work/exts/rhun-editor/lsp
state=$work/state/rhun/extensions/rhun-editor--lsp
mkdir -p "$dir/bin" "$work/proj" "$work/config/rhun" "$state"
CGO_ENABLED=0 go build -o "$dir/bin/lsp-$target" .
# the fake server is "gopls" on a PATH of its own: the manifest's run list names gopls, and the
# sandbox lets the extension start only what the list names
mkdir -p "$work/bin"
CGO_ENABLED=0 go build -o "$work/bin/gopls" ./tests/fakeserver
cp extension.ini "$dir/"
printf 'package main\n\n// TODO first\nfunc main() {\n\tERR := 1\n\t_ = ERR\n}\n' > "$work/proj/main.go"
printf 'package main\n\nfunc other() {}\n' > "$work/proj/other.go"
printf 'package main\n\nfunc f() {\n    x := 1\n    _ = x\n}\n' > "$work/proj/fmt.go"
printf '[files]\nrestore_session = false\nrestore_project = false\n[updates]\ncheck = false\n[git]\nenabled = false\n[ui]\nsidebar = false\nagents_panel = false\n[editor]\ncursor_blink = false\n' > "$work/config/rhun/config"
run() { # SCRIPT: runs it in a fresh editor, the output on stdout
    sed "s|proj/|$work/proj/|" "$1" > "$work/script.txt"
    PATH="$work/bin:$PATH" HOME=$work XDG_CONFIG_HOME=$work/config XDG_STATE_HOME=$work/state \
        "$RHUN" "$work/proj" --headless 1000x600 --scale 1 --extensions "$work/exts" --script "$work/script.txt" 2>&1
}
cell=$(run tests/cell.txt | tail -1)   # where line 5 column 3 is on this platform's font
sed "s|^move X Y|move $cell|" tests/basic.txt > "$work/basic.src"
run "$work/basic.src" | sed 's/pid=[1-9][0-9]*/pid=PID/' > "$work/out.txt"
check() { # NAME: the output against tests/NAME.expected (UPDATE=1 writes it instead)
    if [ -n "$UPDATE" ]; then cp "$work/out.txt" "tests/$1.expected"; fi
    if cmp -s "$work/out.txt" "tests/$1.expected"; then
        echo "ok   $1"
    else
        echo "FAIL $1"; diff "tests/$1.expected" "$work/out.txt" || true
        echo "--- the extension's log:"; cat "$work"/state/rhun/extensions/*.log 2>/dev/null | tail -20; exit 1
    fi
}
check basic
# the language features change the files: a fresh copy of the project
printf 'package main\n\nfunc other() {}\n' > "$work/proj/other.go"
printf 'package main\n\n// TODO first\nfunc main() {\n\tERR := 1\n\t_ = ERR\n}\n' > "$work/proj/main.go"
# the confirmation list names files by their paths: the scratch folder is WORK
run tests/lang.txt | sed -e 's/pid=[1-9][0-9]*/pid=PID/' -e "s|/private$work|WORK|g" -e "s|$work|WORK|g" > "$work/out.txt"
check lang

# protocol 1.1: every server but the fake turned off, so the scope does not depend on what this
# machine has installed; formatting as you type on
printf 'package main\n\n// TODO first\nfunc main() {\n\tERR := 1\n\t_ = ERR\n}\n' > "$work/proj/main.go"
printf 'package main\n\nfunc f() {\n    x := 1\n    _ = x\n}\n' > "$work/proj/fmt.go"
printf '[servers]\n' > "$state/lsp.ini"
for lang in Rust TypeScript JavaScript Python C C++ Zig Lua Ruby Dart Kotlin Swift OCaml Haskell Elixir Nix Terraform YAML JSON HTML CSS; do
    printf '%s = off\n' "$lang" >> "$state/lsp.ini"
done
cp "$work/config/rhun/config" "$work/config.saved"
printf '\n[editor]\nformat_on_type = yes\n' >> "$work/config/rhun/config"
printf 'package main\n\nfunc leaf() {}\n\nfunc caller() {\n\tleaf()\n\tleaf()\n}\n' > "$work/proj/calls.go"
run tests/lang11.txt | sed -e 's/pid=[1-9][0-9]*/pid=PID/' -e "s|/private$work|WORK|g" -e "s|$work|WORK|g" > "$work/out.txt"
check lang11
cp "$work/config.saved" "$work/config/rhun/config"
rm "$state/lsp.ini"

# Saving must negotiate through the real SDK, format first and then write the resulting bytes.
printf '\n[files]\nformat_on_save = true\n' >> "$work/config/rhun/config"
run tests/save.txt > "$work/out.txt"
check save
printf 'package main\n\nfunc f() {\n\tx := 1\n\t_ = x\n}\n' > "$work/saved.expected"
if ! cmp -s "$work/proj/fmt.go" "$work/saved.expected"; then
    echo "FAIL saved formatting"; exit 1
fi

# linked editing: HTML's server is the fake one too
printf '<div>\n  <p>hi</p>\n</div>\n' > "$work/proj/a.html"
printf '[servers]\nHTML = gopls\n' > "$state/lsp.ini"
run tests/linked.txt > "$work/out.txt"
check linked
rm "$state/lsp.ini"

# workspace folders: two from the command line, a third added
mkdir -p "$work/lib" "$work/docs"
printf 'package main\n\n// FOLDERS\n' > "$work/proj/folders.go"
sed -e "s|proj/|$work/proj/|" -e "s|WORKDIR/|$work/|" tests/folders.txt > "$work/script.txt"
PATH="$work/bin:$PATH" HOME=$work XDG_CONFIG_HOME=$work/config XDG_STATE_HOME=$work/state \
    "$RHUN" "$work/proj" "$work/lib" --headless 1000x600 --scale 1 --extensions "$work/exts" --script "$work/script.txt" 2>&1 |
    sed -e 's/pid=[1-9][0-9]*/pid=PID/' -e "s|/private$work|WORK|g" -e "s|$work|WORK|g" > "$work/out.txt"
check folders

python3 tests/callbacks.py "$RHUN"

python3 tests/source-actions.py "$RHUN"

python3 tests/watched-files.py "$RHUN"
