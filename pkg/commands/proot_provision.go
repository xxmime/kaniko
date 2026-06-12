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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xxmime/kaniko/pkg/constants"
)

const (
	// prootAutoDownloadEnv disables auto-provisioning when set to a falsey
	// value ("skip", "0", "false", "no").
	prootAutoDownloadEnv = "KANIKO_PROOT_AUTODOWNLOAD"
	// prootURLEnv overrides the URL the static proot binary is fetched from.
	prootURLEnv = "KANIKO_PROOT_URL"
	// prootSHA256Env, when set, is the expected SHA-256 of the downloaded
	// proot binary; provisioning fails if it does not match.
	prootSHA256Env = "KANIKO_PROOT_SHA256"

	prootDownloadTimeout = 5 * time.Minute
)

// defaultProotURLs maps GOARCH to the default static proot download URL. They
// point at the kaniko project's own release assets via GitHub's stable
// "latest" redirect, so the binary always tracks a matching proot build.
var defaultProotURLs = map[string]string{
	"amd64": "https://github.com/xxmime/kaniko/releases/latest/download/proot-linux-amd64",
	"arm64": "https://github.com/xxmime/kaniko/releases/latest/download/proot-linux-arm64",
}

// provisionedProotPath caches the resolved path for the lifetime of the
// process so we download at most once even though sandboxProotPath() runs for
// every RUN instruction.
var provisionedProotPath string

// provisionProot returns the path to a usable static proot binary, downloading
// one when none is bundled with the kaniko binary (the common case when kaniko
// runs as a stand-alone executable rather than from its container image).
//
// It is only reached after the cheaper local lookups in sandboxProotPath()
// fail, and only when proot is actually required (sandbox mode without a
// bind-mounted /proc). On any failure it returns "" so the caller transparently
// falls back to the stub /proc behaviour.
func provisionProot() string {
	if provisionedProotPath != "" {
		return provisionedProotPath
	}
	if disabled(os.Getenv(prootAutoDownloadEnv)) {
		return ""
	}

	cachePath := prootCachePath()
	if cachePath == "" {
		logrus.Warn("Sandbox: cannot auto-provision proot (no writable cache directory); continuing without proot")
		return ""
	}
	if isExecutableFile(cachePath) {
		provisionedProotPath = cachePath
		return cachePath
	}

	url := os.Getenv(prootURLEnv)
	if url == "" {
		url = defaultProotURLs[runtime.GOARCH]
	}
	if url == "" {
		logrus.Warnf("Sandbox: no proot binary found and no default download URL for GOARCH=%s; set %s to a static proot binary. RUN commands that need a correct /proc/self/exe (rustc, cargo, the glibc loader, ...) may fail.", runtime.GOARCH, prootURLEnv)
		return ""
	}

	logrus.Infof("Sandbox: proot not found locally; downloading a static proot from %s (set %s=skip to disable or %s to override)", url, prootAutoDownloadEnv, prootURLEnv)
	if err := downloadProot(url, cachePath); err != nil {
		logrus.Warnf("Sandbox: failed to auto-provision proot from %s: %v; continuing without proot", url, err)
		return ""
	}
	provisionedProotPath = cachePath
	logrus.Infof("Sandbox: using auto-downloaded proot at %s", cachePath)
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

// prootCachePath returns the path the downloaded proot should be written to,
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

// downloadProot fetches url into dest atomically, verifying an optional SHA-256
// and that the binary runs (`proot --version`) before publishing it.
func downloadProot(url, dest string) error {
	client := &http.Client{Timeout: prootDownloadTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".proot-download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if want := strings.TrimSpace(os.Getenv(prootSHA256Env)); want != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(want, got) {
			return fmt.Errorf("sha256 mismatch: expected %s, got %s", want, got)
		}
	}

	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if err := verifyProot(tmpName); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

// verifyProot makes sure the downloaded file is a runnable proot binary for the
// current platform (guards against HTML error pages, wrong-arch builds, etc.).
func verifyProot(path string) error {
	cmd := exec.Command(path, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("proot sanity check failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
