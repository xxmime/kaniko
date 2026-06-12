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

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/xxmime/kaniko/pkg/constants"
	"github.com/xxmime/kaniko/pkg/proot"
)

// prootEmbeddedEnv disables extraction of the embedded proot when set to a
// falsey value ("skip", "0", "false", "no").
const prootEmbeddedEnv = "KANIKO_PROOT_EMBEDDED"

// provisionedProotPath caches the resolved path for the lifetime of the
// process so we extract at most once even though sandboxProotPath() runs for
// every RUN instruction.
var provisionedProotPath string

// provisionProot returns the path to a usable static proot binary by extracting
// the copy embedded in the kaniko executable. This is the common case when
// kaniko runs as a stand-alone binary rather than from its container image
// (which already ships /kaniko/proot).
//
// It is only reached after the cheaper local lookups in sandboxProotPath()
// fail, and only when proot is actually required (sandbox mode without a
// bind-mounted /proc). On any failure it returns "" so the caller transparently
// falls back to the stub /proc behaviour.
func provisionProot() string {
	if provisionedProotPath != "" {
		return provisionedProotPath
	}
	if disabled(os.Getenv(prootEmbeddedEnv)) {
		return ""
	}

	bin := proot.Binary()
	if len(bin) == 0 {
		logrus.Warn("Sandbox: no proot is embedded in this kaniko binary; RUN commands that need a correct /proc/self/exe (rustc, cargo, the glibc loader, ...) may fail. Provide one via KANIKO_PROOT, or use the kaniko container image which ships /kaniko/proot.")
		return ""
	}

	cachePath := prootCachePath()
	if cachePath == "" {
		logrus.Warn("Sandbox: cannot extract embedded proot (no writable cache directory); continuing without proot")
		return ""
	}
	if isExecutableFile(cachePath) {
		provisionedProotPath = cachePath
		return cachePath
	}

	if err := writeProot(bin, cachePath); err != nil {
		logrus.Warnf("Sandbox: failed to extract embedded proot to %s: %v; continuing without proot", cachePath, err)
		return ""
	}
	provisionedProotPath = cachePath
	logrus.Infof("Sandbox: extracted bundled proot to %s", cachePath)
	return cachePath
}

func disabled(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "skip", "0", "false", "no", "off", "disable", "disabled":
		return true
	default:
		return false
	}
}

// prootCachePath returns the path the extracted proot should be written to,
// preferring /kaniko (so it is picked up by the local lookup on later runs and
// lives outside the /kaniko/sandbox chroot) and falling back to the temp dir.
func prootCachePath() string {
	for _, dir := range []string{constants.DefaultKanikoPath, os.TempDir()} {
		if dirWritable(dir) {
			return filepath.Join(dir, "proot")
		}
	}
	return ""
}

func dirWritable(dir string) bool {
	if dir == "" {
		return false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".kaniko-proot-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// writeProot writes data to dest atomically, verifying that the binary runs
// (`proot --version`) before publishing it.
func writeProot(data []byte, dest string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".proot-extract-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if err := verifyProot(tmpName); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

// verifyProot makes sure the extracted file is a runnable proot binary for the
// current platform.
func verifyProot(path string) error {
	cmd := exec.Command(path, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("proot sanity check failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
