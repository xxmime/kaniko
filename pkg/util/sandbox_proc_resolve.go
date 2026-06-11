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
	"path/filepath"
	"strings"
)

// LookPathFunc resolves an executable name against PATH inside the sandbox.
type LookPathFunc func(name string) (string, error)

// ValidateExecutableFunc checks that an absolute path exists and is executable
// inside the sandbox.
type ValidateExecutableFunc func(absPath string) error

// procSelfExeAliases maps common build-tool front-ends to the compiler or
// runtime binary that actually reads /proc/self/exe (e.g. cargo invokes rustc).
var procSelfExeAliases = map[string][]string{
	"cargo":      {"rustc"},
	"rustup":     {"rustc"},
	"clippy":     {"rustc"},
	"rustfmt":    {"rustc"},
	"rustdoc":    {"rustc"},
	"npm":        {"node"},
	"npx":        {"node"},
	"yarn":       {"node"},
	"pnpm":       {"node"},
	"pip":        {"python3", "python"},
	"pip3":       {"python3", "python"},
	"deno":       {"deno"},
	"bun":        {"bun"},
	"dotnet":     {"dotnet"},
	"msbuild":    {"dotnet"},
	"gradle":     {"java"},
	"mvn":        {"java"},
}

// procSelfExePriority ranks binaries for the stub /proc/self/exe symlink when
// a RUN script invokes several tools. Higher wins. Compilers and language
// runtimes beat wrappers (cargo, npm, make).
var procSelfExePriority = map[string]int{
	"rustc": 100, "zig": 100,
	"gcc": 95, "g++": 95, "cc": 95, "c++": 95,
	"clang": 95, "clang++": 95,
	"go": 90,
	"node": 85, "nodejs": 85,
	"python3": 85, "python": 85, "python2": 85,
	"java": 80, "javac": 80, "kotlin": 80, "kotlinc": 80,
	"deno": 80, "bun": 80, "dotnet": 80,
	"ld": 75, "ld.lld": 75, "lld": 75, "gold": 75,
	"cargo": 60, "rustup": 60, "npm": 55, "npx": 55,
	"yarn": 55, "pnpm": 55, "pip": 55, "pip3": 55,
	"make": 40, "cmake": 40, "ninja": 40, "meson": 40,
	"strip": 30, "objcopy": 30, "install": 30,
}

// ResolveSandboxProcSelfExeTarget picks the chroot-absolute path that the stub
// /proc/self/exe symlink should reference for a RUN command. It understands
// shell scripts, toolchain wrappers (cargo→rustc, npm→node), and multi-step
// commands (cmake && make && cargo build).
func ResolveSandboxProcSelfExeTarget(cmd []string, prependShell bool, lookPath LookPathFunc, validateAbs ValidateExecutableFunc) string {
	if len(cmd) == 0 {
		return ""
	}
	if !prependShell {
		return resolveProcSelfExeCandidate(cmd[0], lookPath, validateAbs)
	}
	if len(cmd) < 3 || cmd[1] != "-c" {
		return resolveProcSelfExeCandidate(cmd[0], lookPath, validateAbs)
	}

	bestPath := ""
	bestScore := -1
	for _, word := range shellExecutables(cmd[2]) {
		path, score := bestProcSelfExeForWord(word, lookPath, validateAbs)
		if score > bestScore && path != "" {
			bestPath = path
			bestScore = score
		}
	}
	if bestPath != "" {
		return bestPath
	}
	return resolveProcSelfExeCandidate(cmd[0], lookPath, validateAbs)
}

func bestProcSelfExeForWord(word string, lookPath LookPathFunc, validateAbs ValidateExecutableFunc) (string, int) {
	word = strings.Trim(word, `"'`)
	if word == "" || isShellBuiltin(word) {
		return "", -1
	}

	bestPath := ""
	bestScore := -1
	try := func(name string) {
		path, score := resolveProcSelfExeCandidateWithScore(name, lookPath, validateAbs)
		if score > bestScore && path != "" {
			bestPath = path
			bestScore = score
		}
	}

	try(word)
	base := filepath.Base(word)
	if aliases, ok := procSelfExeAliases[base]; ok {
		for _, alias := range aliases {
			try(alias)
		}
	}
	return bestPath, bestScore
}

func resolveProcSelfExeCandidate(word string, lookPath LookPathFunc, validateAbs ValidateExecutableFunc) string {
	path, _ := resolveProcSelfExeCandidateWithScore(word, lookPath, validateAbs)
	return path
}

func resolveProcSelfExeCandidateWithScore(word string, lookPath LookPathFunc, validateAbs ValidateExecutableFunc) (string, int) {
	word = strings.Trim(word, `"'`)
	if word == "" || isShellBuiltin(word) {
		return "", -1
	}

	var path string
	if strings.Contains(word, string(filepath.Separator)) {
		if validateAbs != nil {
			if err := validateAbs(word); err != nil {
				return "", -1
			}
		}
		path = filepath.Clean(word)
	} else {
		p, err := lookPath(word)
		if err != nil {
			return "", -1
		}
		path = p
	}

	base := strings.ToLower(filepath.Base(path))
	score := procSelfExePriority[base]
	if score == 0 {
		score = 10 // unknown but resolved executable
	}
	return path, score
}

// shellExecutables returns the leading command name from every simple shell
// segment (split on &&, ||, ;, |). Handles env assignments and python -m.
func shellExecutables(script string) []string {
	var words []string
	for _, segment := range splitShellSegments(script) {
		if word := firstShellCommandWord(segment); word != "" {
			words = append(words, word)
		}
	}
	return words
}

func splitShellSegments(script string) []string {
	script = strings.TrimSpace(script)
	if script == "" {
		return nil
	}
	var segments []string
	var current strings.Builder
	for i := 0; i < len(script); i++ {
		if i+1 < len(script) {
			two := script[i : i+2]
			if two == "&&" || two == "||" {
				segments = append(segments, strings.TrimSpace(current.String()))
				current.Reset()
				i++
				continue
			}
		}
		if script[i] == ';' || script[i] == '|' {
			segments = append(segments, strings.TrimSpace(current.String()))
			current.Reset()
			continue
		}
		current.WriteByte(script[i])
	}
	if tail := strings.TrimSpace(current.String()); tail != "" {
		segments = append(segments, tail)
	}
	return segments
}

func firstShellCommandWord(segment string) string {
	segment = strings.TrimSpace(segment)
	parts := strings.Fields(segment)
	skipEnv := true
	for i := 0; i < len(parts); i++ {
		part := strings.Trim(parts[i], `"'`)
		if skipEnv && strings.Contains(part, "=") && !strings.HasPrefix(part, "-") {
			continue
		}
		skipEnv = false
		if part == "env" {
			continue
		}
		if part == "command" || part == "builtin" || part == "exec" {
			continue
		}
		// python3 -m pip / python -m build
		if (part == "python" || part == "python3" || part == "python2") && i+2 < len(parts) && parts[i+1] == "-m" {
			return part
		}
		if part != "" && !isShellBuiltin(part) {
			return part
		}
	}
	return ""
}

func isShellBuiltin(word string) bool {
	switch word {
	case "cd", "export", "unset", "set", "true", "false", ":", ".", "source",
		"umask", "ulimit", "shift", "exit", "return", "test", "[", "echo",
		"printf", "read", "local", "declare", "typeset", "alias", "unalias",
		"wait", "trap", "pwd", "popd", "pushd", "dirs", "let", "eval":
		return true
	default:
		return false
	}
}
