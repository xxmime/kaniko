/*
Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package util

import (
	"errors"
	"testing"

	"github.com/xxmime/kaniko/testutil"
)

func TestShellExecutables(t *testing.T) {
	got := shellExecutables("zig build && strip zig-out/bin/app")
	testutil.CheckDeepEqual(t, []string{"zig", "strip"}, got)

	got = shellExecutables("cmake -B build && cargo build --release")
	testutil.CheckDeepEqual(t, []string{"cmake", "cargo"}, got)

	got = shellExecutables("go build -o app . ; ./install.sh")
	testutil.CheckDeepEqual(t, []string{"go", "./install.sh"}, got)
}

func TestResolveSandboxProcSelfExeTarget(t *testing.T) {
	paths := map[string]string{
		"zig":    "/usr/bin/zig",
		"rustc":  "/usr/bin/rustc",
		"cargo":  "/usr/bin/cargo",
		"go":     "/usr/local/go/bin/go",
		"node":   "/usr/bin/node",
		"npm":    "/usr/bin/npm",
		"python3": "/usr/bin/python3",
		"cmake":  "/usr/bin/cmake",
	}
	// Binaries that exist on PATH but are not recognized toolchains. These
	// model the tools already present before a RUN installs its compiler.
	genericPaths := map[string]string{
		"curl":  "/usr/bin/curl",
		"sh":    "/bin/sh",
		"mv":    "/bin/mv",
		"cp":    "/bin/cp",
		"upx":   "/usr/bin/upx",
		"mkdir": "/bin/mkdir",
	}
	// resolverFor builds lookPath/validateAbs closures. withToolchain controls
	// whether the recognized compilers (rustc/cargo/…) resolve yet; when false
	// only the generic binaries are available, modeling a RUN that installs its
	// toolchain partway through.
	resolverFor := func(withToolchain bool) (LookPathFunc, ValidateExecutableFunc) {
		available := map[string]string{}
		for k, v := range genericPaths {
			available[k] = v
		}
		if withToolchain {
			for k, v := range paths {
				available[k] = v
			}
		}
		byPath := map[string]bool{}
		for _, v := range available {
			byPath[v] = true
		}
		lookPath := func(name string) (string, error) {
			if p, ok := available[name]; ok {
				return p, nil
			}
			return "", errTestNotFound
		}
		validateAbs := func(path string) error {
			if byPath[path] || path == "./install.sh" {
				return nil
			}
			return errTestNotFound
		}
		return lookPath, validateAbs
	}

	cases := []struct {
		name        string
		cmd         []string
		shell       bool
		noToolchain bool
		want        string
	}{
		{
			name:  "zig direct",
			cmd:   []string{"/bin/sh", "-c", "zig build"},
			shell: true,
			want:  "/usr/bin/zig",
		},
		{
			name:  "cargo prefers rustc",
			cmd:   []string{"/bin/sh", "-c", "cargo build --release"},
			shell: true,
			want:  "/usr/bin/rustc",
		},
		{
			name:  "go build",
			cmd:   []string{"/bin/sh", "-c", "go build -o app ."},
			shell: true,
			want:  "/usr/local/go/bin/go",
		},
		{
			name:  "npm prefers node",
			cmd:   []string{"/bin/sh", "-c", "npm run build"},
			shell: true,
			want:  "/usr/bin/node",
		},
		{
			name:  "multi-step picks compiler",
			cmd:   []string{"/bin/sh", "-c", "cmake -B b && cargo build"},
			shell: true,
			want:  "/usr/bin/rustc",
		},
		{
			name:  "non-shell go",
			cmd:   []string{"/usr/local/go/bin/go", "build"},
			shell: false,
			want:  "/usr/local/go/bin/go",
		},
		{
			// The toolchain is installed inside the same RUN, so neither rustup
			// nor cargo nor rustc resolves yet. The stub must fall back to the
			// shell, never to an unrelated binary like curl, otherwise the
			// rustup/cargo proxy re-execs curl via current_exe().
			name:        "toolchain installed mid-run falls back to shell",
			cmd:         []string{"/bin/sh", "-c", "curl https://sh.rustup.rs -sSf | sh -s -- -y && rustup target add x86_64-unknown-linux-musl && cargo build --release && mv a b && upx x && mkdir -p c && cp a b"},
			shell:       true,
			noToolchain: true,
			want:        "/bin/sh",
		},
		{
			// A single unrecognized command is unambiguous, so it is safe to
			// point /proc/self/exe at it.
			name:        "single generic command is trusted",
			cmd:         []string{"/bin/sh", "-c", "curl https://example.com -o out"},
			shell:       true,
			noToolchain: true,
			want:        "/usr/bin/curl",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lookPath, validateAbs := resolverFor(!c.noToolchain)
			got := ResolveSandboxProcSelfExeTarget(c.cmd, c.shell, lookPath, validateAbs)
			testutil.CheckDeepEqual(t, c.want, got)
		})
	}
}

var errTestNotFound = errors.New("not found")
