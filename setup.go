package main

// Installation advice stays in the extension that selects the servers. rhun only displays
// the guide; installing a tool is a separate action in the user's terminal.
import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

type installGuide struct {
	url, prerequisites, command, note string
	macCommand                        string // optional Homebrew option
}

var installGuides = map[string]installGuide{
	"gopls": {url: "https://go.dev/gopls/", prerequisites: "Install Go first.",
		command: "go install golang.org/x/tools/gopls@latest", note: "Add Go's bin directory (GOBIN, or GOPATH/bin) to your PATH."},
	"rust-analyzer": {url: "https://rust-analyzer.github.io/book/installation.html", prerequisites: "Install a Rust toolchain with rustup, including cargo and rustc.",
		command: "rustup component add rust-analyzer rust-src", note: "Use the toolchain selected by your project. Add rustup's bin directory to your PATH."},
	"typescript-language-server": {url: "https://github.com/typescript-language-server/typescript-language-server#installing", prerequisites: "Install Node.js and npm first.",
		command: "npm install -g typescript-language-server typescript@6", note: "TypeScript 6 supplies tsserver. The TypeScript 7 native compiler uses a different server."},
	"pyright-langserver": {url: "https://github.com/microsoft/pyright/blob/main/docs/installation.md", prerequisites: "Install Node.js and npm. Install Python and the dependencies your project imports.",
		command: "npm install -g pyright"},
	"clangd": {url: "https://clangd.llvm.org/installation", prerequisites: "Install clangd from LLVM's releases or your system's package manager.",
		macCommand: "brew install llvm", note: "Add LLVM's bin directory to PATH. For project headers and build flags, generate compile_commands.json with your build system."},
	"zls": {url: "https://zigtools.org/zls/install/", prerequisites: "Install Zig, then download the matching ZLS release for your operating system and processor.",
		macCommand: "brew install zig zls", note: "Zig and ZLS release versions must match. Put zls on PATH. Compiler diagnostics need a build.zig with a check step."},
	"lua-language-server": {url: "https://github.com/LuaLS/lua-language-server/wiki/Getting-Started", prerequisites: "Download LuaLS for your operating system and processor, or use your system's package manager.",
		macCommand: "brew install lua-language-server", note: "Keep the release's support files beside its executable. Add its bin directory to PATH."},
	"solargraph": {url: "https://github.com/castwide/solargraph#installation", prerequisites: "Install Ruby and RubyGems first.",
		command: "gem install solargraph", note: "Add your Ruby installation's gem bin directory to PATH."},
	"dart": {url: "https://dart.dev/get-dart", prerequisites: "Install the Dart SDK, or use the Dart SDK included with Flutter.",
		note: "The SDK includes the language server. Add its bin directory to PATH. No separate server package is needed."},
	"kotlin-language-server": {url: "https://github.com/fwcd/kotlin-language-server#installation", prerequisites: "Install a compatible JDK, then download kotlin-language-server from its releases.",
		macCommand: "brew install kotlin-language-server", note: "Install a Kotlin compiler compatible with the server for its standard library. fwcd 1.3.x uses Kotlin 2.1. Add the JDK, Kotlin and server bin directories to your login shell's PATH."},
	"sourcekit-lsp": {url: "https://github.com/swiftlang/sourcekit-lsp#getting-started", prerequisites: "Install the Swift toolchain for your operating system. On macOS, SourceKit-LSP is included with Xcode.",
		note: "Use the sourcekit-lsp from the same toolchain as your Swift compiler. Build the project so its dependencies are available."},
	"ocamllsp": {url: "https://github.com/ocaml/ocaml-lsp#installation", prerequisites: "Install OCaml and opam. Select the opam switch used by your project.",
		command: "opam install ocaml-lsp-server", note: "Put the selected switch's bin directory on PATH and build the project with Dune. Install ocamlformat for formatting. The server executable is ocamllsp."},
	"haskell-language-server-wrapper": {url: "https://haskell-language-server.readthedocs.io/en/latest/installation.html", prerequisites: "Install GHC and GHCup. Cabal projects also need cabal-install.",
		command: "ghcup install hls", note: "Choose an HLS release supporting the project's GHC. Add GHCup's bin directory to PATH."},
	"elixir-ls": {url: "https://github.com/elixir-lsp/elixir-ls#installation", prerequisites: "Install compatible Erlang/OTP and Elixir versions, then download an ElixirLS release.",
		macCommand: "brew install elixir-ls", note: "ElixirLS releases use launch.sh (launch.bat on Windows). If no elixir-ls command is provided, set Elixir in lsp.ini to the launcher's absolute path."},
	"nil": {url: "https://github.com/oxalica/nil#installation", prerequisites: "Install Nix first. Then install the server in your Nix profile:",
		command: "nix --extra-experimental-features 'nix-command flakes' profile install nixpkgs#nil", note: "Add the Nix profile's bin directory to PATH. Nix requires a supported Unix environment; on Windows, use a Linux environment."},
	"terraform-ls": {url: "https://github.com/hashicorp/terraform-ls/blob/main/docs/installation.md", prerequisites: "Download terraform-ls for your operating system, or install it with a package manager. Install Terraform for project validation.",
		macCommand: "brew install hashicorp/tap/terraform-ls", note: "Put terraform-ls on PATH. Open the folder containing your Terraform configuration."},
	"yaml-language-server": {url: "https://github.com/redhat-developer/yaml-language-server#installation", prerequisites: "Install Node.js and npm first.",
		command: "npm install -g yaml-language-server"},
	"vscode-json-language-server": webInstallGuide(),
	"vscode-html-language-server": webInstallGuide(),
	"vscode-css-language-server":  webInstallGuide(),
	"ty":                          {url: "https://docs.astral.sh/ty/installation/", prerequisites: "Install uv and Python first.", command: "uv tool install ty"},
	"pylsp":                       {url: "https://github.com/python-lsp/python-lsp-server#installation", prerequisites: "Install uv and Python first.", command: "uv tool install \"python-lsp-server[all]\""},
	"basedpyright-langserver":     {url: "https://docs.basedpyright.com/latest/installation/command-line-and-language-server/", prerequisites: "Install uv and Python first.", command: "uv tool install basedpyright"},
	"jedi-language-server":        {url: "https://github.com/pappasam/jedi-language-server#installation", prerequisites: "Install uv and Python first.", command: "uv tool install jedi-language-server", note: "Jedi provides completion and navigation. It does not check types. For type errors, choose Pyright, basedpyright or ty in server settings."},
	"ruff":                        {url: "https://docs.astral.sh/ruff/installation/", prerequisites: "Install uv first.", command: "uv tool install ruff", note: "Ruff provides linting and formatting. It does not provide type checking or completion. rhun currently selects one server per language. Choose Pyright, basedpyright or ty if you need those features."},
	"vtsls":                       {url: "https://github.com/yioneko/vtsls#installation", prerequisites: "Install Node.js and npm first.", command: "npm install -g @vtsls/language-server"},
	"deno":                        {url: "https://docs.deno.com/runtime/getting_started/installation/", prerequisites: "Install Deno using its official instructions.", macCommand: "brew install deno", note: "The language server is bundled. Choosing deno lsp enables analysis for this language; explicit settings can disable it."},
	"ccls":                        {url: "https://github.com/MaskRay/ccls/wiki/Build", prerequisites: "Install ccls with your system's package manager, or build it with LLVM and Clang.", macCommand: "brew install ccls", note: "Generate compile_commands.json with your build system."},
	"ruby-lsp":                    {url: "https://shopify.github.io/ruby-lsp/", prerequisites: "Install Ruby and RubyGems first.", command: "gem install ruby-lsp", note: "Use the Ruby version selected by the project and add its gem bin directory to PATH."},
	"haskell-language-server":     {url: "https://haskell-language-server.readthedocs.io/en/latest/installation.html", prerequisites: "Install GHC and GHCup. Cabal projects also need cabal-install.", command: "ghcup install hls", note: "Select a binary matching the project's GHC, or use haskell-language-server-wrapper instead."},
	"nixd":                        {url: "https://github.com/nix-community/nixd#installation", prerequisites: "Install Nix first. Then install the server in your Nix profile:", command: "nix --extra-experimental-features 'nix-command flakes' profile install nixpkgs#nixd", note: "Add the Nix profile's bin directory to PATH. Nix requires a supported Unix environment; on Windows, use a Linux environment."},
}

func webInstallGuide() installGuide {
	return installGuide{url: "https://github.com/hrsh7th/vscode-langservers-extracted#usage", prerequisites: "Install Node.js and npm first.", command: "npm install -g vscode-langservers-extracted"}
}

func serverProgram(command string) string {
	name := filepath.Base(command)
	for _, suffix := range []string{".exe", ".cmd", ".bat"} {
		if strings.HasSuffix(strings.ToLower(name), suffix) {
			return name[:len(name)-len(suffix)]
		}
	}
	return name
}

// codeSpan handles paths and custom commands containing Markdown punctuation too.
func codeSpan(s string) string {
	fence := "`"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	return fence + " " + strings.NewReplacer("\n", " ", "\r", " ").Replace(s) + " " + fence
}

func setupMarkdown(lang string, argv []string, platform string, found bool) string {
	osName := map[string]string{"darwin": "macOS", "linux": "Linux", "windows": "Windows"}[platform]
	if osName == "" {
		osName = platform
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s language server\n\n", lang)
	if len(argv) == 0 {
		b.WriteString("Language features are disabled for this language in lsp.ini. [Edit server settings](lsp-setup:config), then [restart language servers](lsp-setup:restart).\n\n")
	} else {
		fmt.Fprintf(&b, "Server command: %s\n\n", codeSpan(strings.Join(argv, " ")))
		if found {
			b.WriteString("**Executable found.** This confirms it is on PATH; it does not confirm that the server started successfully.\n\n")
		} else {
			fmt.Fprintf(&b, "**Executable missing:** %s was not found on rhun's PATH.\n\n", codeSpan(argv[0]))
		}
		fmt.Fprintf(&b, "## Install on %s\n\n", osName)
		if g, ok := installGuides[serverProgram(argv[0])]; ok {
			b.WriteString(g.prerequisites + "\n\n")
			command := g.command
			if platform == "darwin" && g.macCommand != "" {
				b.WriteString("If you use Homebrew:\n\n")
				command = g.macCommand
			}
			if command != "" && !(platform == "windows" && (serverProgram(argv[0]) == "nil" || serverProgram(argv[0]) == "nixd")) {
				fmt.Fprintf(&b, "Run in your terminal:\n\n```\n%s\n```\n\n", command)
			}
			if g.note != "" {
				b.WriteString(g.note + "\n\n")
			}
			fmt.Fprintf(&b, "[Official installation instructions](%s)\n\n", g.url)
		} else {
			b.WriteString("This is a custom server command. Follow that server's installation instructions and put the executable on PATH, or set its absolute path in [server settings](lsp-setup:config).\n\n")
		}
		b.WriteString("## Prepare your project\n\nOpen the project folder in rhun. A single file may give the server too little information to find dependencies or build settings. Run project commands below from that folder.\n\n")
		b.WriteString(projectSetup(lang))
		b.WriteString("## After installing\n\nAdd the executable's directory to your login shell's PATH. If you changed PATH, quit and reopen rhun so it picks up the change. Then [restart language servers](lsp-setup:restart).\n\nIf the executable is found but features are still missing, check Language Server in Extensions for a launch error.\n\n")
	}
	b.WriteString("[Choose another language](lsp-setup:languages) | [Edit server settings](lsp-setup:config)\n")
	return b.String()
}

func projectSetup(lang string) string {
	switch lang {
	case "Go":
		return "Open the folder containing go.mod or go.work. Download the module's dependencies:\n\n```\ngo mod download\n```\n\n"
	case "Rust":
		return "Open the folder containing Cargo.toml. Use the Rust toolchain selected by rust-toolchain.toml, if present. rust-analyzer may run Cargo to discover build settings.\n\n"
	case "TypeScript", "JavaScript":
		return "Open the folder containing package.json and tsconfig.json or jsconfig.json. Install dependencies using the package manager and lockfile already used by the project. For Deno, open the folder containing deno.json or deno.jsonc.\n\n"
	case "Python":
		return "Install the project's dependencies in its virtual environment. If imports cannot be found, configure the selected server to use that environment in settings.json.\n\n"
	case "C", "C++":
		return "Generate compile_commands.json with the project's build system, then open its root folder. For CMake projects:\n\n```\ncmake -S . -B build -DCMAKE_EXPORT_COMPILE_COMMANDS=ON\n```\n\n"
	case "Zig":
		return "Open the folder containing build.zig. Compiler diagnostics require a check step that compiles the project's modules. Confirm that the project defines it:\n\n```\nzig build check\n```\n\nIf the check step is missing, follow the [ZLS build configuration guide](https://zigtools.org/zls/guides/build-on-save/). Syntax errors and navigation can still work without it.\n\n"
	case "Swift":
		return "For Swift packages, open the folder containing Package.swift and resolve its dependencies by building:\n\n```\nswift build\n```\n\nUse sourcekit-lsp from the same toolchain. Xcode projects need a compatible build server to supply compiler settings; opening an .xcodeproj alone does not supply them to SourceKit-LSP.\n\n"
	case "OCaml":
		return "Open the Dune project root using the same opam switch as the server. Generate the build information the server needs:\n\n```\ndune build\n```\n\nRebuild after changing dependencies or Dune files.\n\n"
	case "Kotlin":
		return "Open the Gradle or Maven project root so the server can resolve dependencies. The fwcd server uses Kotlin 2.1 libraries. Allow its first analysis to finish before expecting language features.\n\n"
	case "Haskell":
		return "Open the Cabal or Stack project root. HLS must support the project's GHC version. Build dependencies with the package manager already used by the project.\n\n"
	case "Dart":
		return "Open the folder containing pubspec.yaml. For Dart projects, fetch dependencies with:\n\n```\ndart pub get\n```\n\nFor Flutter projects, use:\n\n```\nflutter pub get\n```\n\n"
	case "Elixir":
		return "Open the folder containing mix.exs. Fetch its dependencies:\n\n```\nmix deps.get\n```\n\nElixirLS compiles the project for analysis. Match the project's Elixir and Erlang versions.\n\n"
	case "Ruby":
		return "Open the folder containing Gemfile and use the project's Ruby version. Install its dependencies with Bundler.\n\n"
	case "Terraform":
		return "Open the folder containing the Terraform configuration and initialize its providers:\n\n```\nterraform init\n```\n\n"
	default:
		return "Keep the language's project configuration and schema files inside the open folder so the server can discover them.\n\n"
	}
}

// Called with l.mu held. A failed lookup is cached by serverFor, so this notice appears once
// per language until the user retries. It never opens a view while the user is typing.
func (l *lsp) missingServer(lang, program string) {
	if !l.has(rhunext.FeatureNotify) || !l.has(rhunext.FeatureViewDoc) || !l.has(rhunext.FeatureViewTree) {
		return
	}
	go func() {
		id, err := l.ext.Notify(rhunext.SeverityInfo, rhunext.NewNote(lang+" language server is missing").
			Detail(filepath.Base(program)+" was not found. Install it to enable language features.").
			Button("Install instructions"))
		if err != nil {
			l.ext.Logf("installation notice: %v", err)
			return
		}
		l.mu.Lock()
		if l.setupNotes == nil {
			l.setupNotes = map[int64]string{}
		}
		l.setupNotes[id] = lang
		l.mu.Unlock()
	}()
}

func (l *lsp) NoteEvent(ext *rhunext.Ext, ev rhunext.NoteEvent) {
	l.mu.Lock()
	lang := l.setupNotes[ev.Note]
	if ev.Closed || ev.Pressed != 0 {
		delete(l.setupNotes, ev.Note)
	}
	l.mu.Unlock()
	if lang != "" && ev.Pressed == 1 {
		go l.showSetup(0, lang, false)
	}
}

func (l *lsp) showSetup(doc rhunext.Doc, lang string, choose bool) {
	l.mu.Lock()
	if lang == "" && !choose {
		if d := l.docs[doc]; d != nil {
			lang = d.lang
		} else {
			lang = l.setupLang
		}
	}
	_, known := servers[lang]
	l.mu.Unlock()
	if !known || choose {
		var langs []string
		for name := range servers {
			langs = append(langs, name)
		}
		slices.Sort(langs)
		i, err := l.ext.Choose("Set up a language server", langs)
		if err != nil || i < 0 || i >= len(langs) {
			return
		}
		lang = langs[i]
	}
	l.mu.Lock()
	argv := l.config[lang]
	if argv == nil {
		argv = servers[lang]
	}
	argv = slices.Clone(argv)
	l.mu.Unlock()
	if !l.has(rhunext.FeatureViewDoc) || !l.has(rhunext.FeatureViewTree) {
		l.ext.Toast("Update rhun to view language server setup instructions")
		return
	}
	found := false
	if len(argv) > 0 {
		_, err := exec.LookPath(argv[0])
		found = err == nil
	}
	l.setupMu.Lock()
	defer l.setupMu.Unlock()
	l.mu.Lock()
	old := l.setupView
	l.setupView = 0
	l.mu.Unlock()
	if old != 0 {
		l.ext.CloseView(old)
	}
	v, err := l.ext.TabView("doc", "Language Server Setup")
	if err == nil {
		l.mu.Lock()
		l.setupView = v
		l.setupLang = lang
		l.mu.Unlock()
		_, err = l.ext.PatchView(v, (&rhunext.Patch{}).Doc(setupMarkdown(lang, argv, runtime.GOOS, found)))
	}
	if err != nil {
		l.ext.Toast("Could not open language server setup: " + err.Error())
	}
}

func ensureConfig(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, writeErr := f.WriteString("# Override a server command, or use off to disable a language.\n# Example: Python = ty server\n[servers]\n")
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func (l *lsp) ViewOpen(ext *rhunext.Ext, open rhunext.ViewOpen) {
}

func (l *lsp) ViewEvent(ext *rhunext.Ext, view rhunext.View, ev rhunext.RetainedEvent) {
	l.mu.Lock()
	ours := view == l.setupView
	if ours && ev.Kind == "closed" {
		l.setupView = 0
		l.setupLang = ""
	}
	l.mu.Unlock()
	if !ours || ev.Kind != "link" {
		return
	}
	switch string(ev.Body) {
	case "lsp-setup:languages":
		go l.showSetup(0, "", true)
	case "lsp-setup:restart":
		go l.Run(ext, "lsp.restart", 0)
	case "lsp-setup:config":
		go func() {
			path := filepath.Join(l.state, "lsp.ini")
			if err := ensureConfig(path); err != nil {
				ext.Toast("Could not open server settings: " + err.Error())
				return
			}
			ext.Open(path, rhunext.Pos{})
		}()
	}
}
