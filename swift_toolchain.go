package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Xcode's SourceKit XPC service cannot be registered from a sandboxed command-line
// client. Present the same toolchain with its in-process SourceKit library instead.
// Omit the dependency-scanning library as well: loading it beside in-process
// SourceKit duplicates Swift's Objective-C classes. SwiftPM then uses its supported
// swift-frontend subprocess fallback. Original signed Xcode files stay untouched.
func swiftToolchain(state string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "xcrun", "--find", "swiftc").Output()
	if err != nil {
		return "", err
	}
	root := filepath.Dir(filepath.Dir(strings.TrimSpace(string(output))))
	library := filepath.Join(root, "lib", "sourcekitdInProc.framework", "sourcekitdInProc")
	if _, err := os.Stat(library); err != nil {
		return "", err
	}
	view := filepath.Join(state, "cache", "swift-toolchain")
	if err := os.MkdirAll(filepath.Join(view, "lib"), 0700); err != nil {
		return "", err
	}
	link := func(target, path string) error {
		if previous, err := os.Readlink(path); err == nil {
			if previous == target {
				return nil
			}
			if err := os.Remove(path); err != nil {
				return err
			}
		}
		return os.Symlink(target, path)
	}
	if err := link(filepath.Join(root, "bin"), filepath.Join(view, "bin")); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(filepath.Join(root, "lib"))
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Name() == "sourcekitd.framework" {
			continue
		}
		if entry.Name() == "libswiftScan.dylib" {
			// Remove links left by an older version of this toolchain view.
			if err := os.Remove(filepath.Join(view, "lib", entry.Name())); err != nil && !os.IsNotExist(err) {
				return "", err
			}
			continue
		}
		if entry.Name() == "swift" {
			if err := swiftLibraryView(filepath.Join(root, "lib", "swift"), filepath.Join(view, "lib", "swift")); err != nil {
				return "", err
			}
			continue
		}
		if err := link(filepath.Join(root, "lib", entry.Name()), filepath.Join(view, "lib", entry.Name())); err != nil {
			return "", fmt.Errorf("SourceKit toolchain: %w", err)
		}
	}
	if err := link(library, filepath.Join(view, "lib", "libsourcekitdInProc.dylib")); err != nil {
		return "", err
	}
	return view, nil
}

// Only the ancestors of the scan library need directories. Everything else is
// linked to the selected toolchain, including support libraries and compiler tools.
func swiftLibraryView(source, destination string) error {
	if _, err := os.Readlink(destination); err == nil {
		if err := os.Remove(destination); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(destination, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		target, path := filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())
		if entry.Name() == "lib_InternalSwiftScan.dylib" || entry.Name() == "libswiftScan.dylib" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if entry.IsDir() && (entry.Name() == "host" || entry.Name() == "compiler") {
			if err := swiftLibraryView(target, path); err != nil {
				return err
			}
			continue
		}
		if previous, err := os.Readlink(path); err == nil {
			if previous == target {
				continue
			}
			if err := os.Remove(path); err != nil {
				return err
			}
		}
		if err := os.Symlink(target, path); err != nil {
			return err
		}
	}
	return nil
}
