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

// Package proot exposes a statically linked proot binary that is embedded into
// the kaniko executable at build time. When kaniko runs as a stand-alone binary
// (rather than from its container image, which ships /kaniko/proot), this lets
// sandboxed RUN commands that rely on a correct /proc/self/exe (rustc, cargo,
// the glibc loader, ...) work without any runtime download.
//
// The per-architecture binary is embedded by the build-constrained files
// (embed_linux_amd64.go, embed_linux_arm64.go). The assets committed to the
// repository are text placeholders; the release build replaces them with the
// real static proot (see hack/fetch-proot.sh) before compiling. Detection is by
// ELF magic, so a placeholder is reported as "not embedded".
package proot

// Binary returns the embedded static proot binary for the current platform, or
// nil when none was embedded at build time (placeholder asset or unsupported
// platform).
func Binary() []byte {
	if isELF(prootBinary) {
		return prootBinary
	}
	return nil
}

// Available reports whether a real proot binary is embedded in this build.
func Available() bool {
	return isELF(prootBinary)
}

func isELF(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x7f && b[1] == 'E' && b[2] == 'L' && b[3] == 'F'
}
