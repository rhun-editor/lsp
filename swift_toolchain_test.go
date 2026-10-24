package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSwiftLibraryViewMigratesWithoutChangingToolchain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS toolchain view uses Unix symbolic links")
	}
	root := t.TempDir()
	source, view := filepath.Join(root, "original"), filepath.Join(root, "view")
	compiler := filepath.Join(source, "host", "compiler")
	if err := os.MkdirAll(compiler, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lib_InternalSwiftScan.dylib", "lib_CompilerSwiftSyntax.dylib"} {
		if err := os.WriteFile(filepath.Join(compiler, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("compiler/lib_InternalSwiftScan.dylib", filepath.Join(source, "host", "lib_InternalSwiftScan.dylib")); err != nil {
		t.Fatal(err)
	}
	// A previous release linked the whole Swift directory.
	if err := os.Symlink(source, view); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := swiftLibraryView(source, view); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"host/lib_InternalSwiftScan.dylib", "host/compiler/lib_InternalSwiftScan.dylib"} {
		if _, err := os.Lstat(filepath.Join(view, path)); !os.IsNotExist(err) {
			t.Fatal("scan library remains in the view", path, err)
		}
		if _, err := os.Stat(filepath.Join(source, path)); err != nil {
			t.Fatal("original toolchain was modified", path, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(view, "host/compiler/lib_CompilerSwiftSyntax.dylib")); err != nil || string(got) != "lib_CompilerSwiftSyntax.dylib" {
		t.Fatal("support library unavailable", string(got), err)
	}
}
