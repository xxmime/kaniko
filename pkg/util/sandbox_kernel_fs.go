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
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// SandboxKernelFSBindMounted is true when /proc, /sys and /dev were all
// bind-mounted into the sandbox successfully.
var SandboxKernelFSBindMounted bool

// SandboxProcSelfStub is true when /proc/self/exe is maintained as a symlink
// stub instead of a real procfs entry (no CAP_SYS_ADMIN and no user namespace).
var SandboxProcSelfStub bool

const sandboxUrandomStubSize = 1024 * 1024

// SetupSandboxStubKernelFilesystems creates minimal /proc, /sys and /dev trees
// inside the sandbox without mount(2). This is used when bind-mounting the host
// kernel filesystems is not permitted.
func SetupSandboxStubKernelFilesystems(sandboxPath string, needProc, needSys, needDev bool) error {
	var stubs []string
	if needProc {
		if err := setupSandboxStubProc(sandboxPath); err != nil {
			return errors.Wrap(err, "stub proc")
		}
		SandboxProcSelfStub = true
		stubs = append(stubs, "proc")
	}
	if needSys {
		if err := setupSandboxStubSys(sandboxPath); err != nil {
			return errors.Wrap(err, "stub sys")
		}
		stubs = append(stubs, "sys")
	}
	if needDev {
		if err := setupSandboxStubDev(sandboxPath); err != nil {
			return errors.Wrap(err, "stub dev")
		}
		stubs = append(stubs, "dev")
	}
	if len(stubs) > 0 {
		logrus.Infof("Sandbox: using stub kernel filesystem(s) without bind-mount: %s", strings.Join(stubs, ", "))
	}
	return nil
}

const sandboxStubCPUInfo = "processor\t: 0\nvendor_id\t: KanikoStub\nmodel name\t: Sandbox Stub CPU\ncpu cores\t: 1\n"

const sandboxStubMemInfo = "MemTotal:       8388608 kB\nMemFree:        4194304 kB\n"

func setupSandboxStubProc(sandboxPath string) error {
	procDir := filepath.Join(sandboxPath, "proc")
	if err := os.MkdirAll(filepath.Join(procDir, "self"), 0o555); err != nil {
		return err
	}
	for name, content := range map[string]string{
		"cpuinfo": sandboxStubCPUInfo,
		"meminfo": sandboxStubMemInfo,
	} {
		if err := os.WriteFile(filepath.Join(procDir, name), []byte(content), 0o444); err != nil {
			return err
		}
	}
	return nil
}

func setupSandboxStubSys(sandboxPath string) error {
	return os.MkdirAll(filepath.Join(sandboxPath, "sys"), 0o555)
}

func setupSandboxStubDev(sandboxPath string) error {
	devDir := filepath.Join(sandboxPath, "dev")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"null", "zero", "full"} {
		f, err := os.OpenFile(filepath.Join(devDir, name), os.O_CREATE|os.O_RDWR, 0o666)
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	for _, name := range []string{"stdin", "stdout", "stderr"} {
		target := filepath.Join(devDir, name)
		_ = os.Remove(target)
		if err := os.Symlink("/dev/null", target); err != nil {
			return err
		}
	}
	urandomPath := filepath.Join(devDir, "urandom")
	hostRand, err := os.Open("/dev/urandom")
	if err != nil {
		f, createErr := os.OpenFile(urandomPath, os.O_CREATE|os.O_RDWR, 0o444)
		if createErr != nil {
			return createErr
		}
		return f.Close()
	}
	defer hostRand.Close()
	out, err := os.OpenFile(urandomPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o444)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(out, hostRand, sandboxUrandomStubSize); err != nil && !errors.Is(err, io.EOF) {
		out.Close()
		return err
	}
	return out.Close()
}

// UpdateSandboxProcSelfStub points the stub /proc/self/exe symlink at the
// best-matching executable for this RUN and writes a minimal /proc/self/cmdline
// file. chrootExePath must be absolute from the chroot's perspective.
func UpdateSandboxProcSelfStub(sandboxPath, chrootExePath string, argv []string) error {
	if !SandboxProcSelfStub {
		return nil
	}
	if chrootExePath == "" {
		return nil
	}
	if !filepath.IsAbs(chrootExePath) {
		chrootExePath = filepath.Clean("/" + chrootExePath)
	}
	procSelf := filepath.Join(sandboxPath, "proc", "self")
	if err := os.MkdirAll(procSelf, 0o555); err != nil {
		return err
	}
	exePath := filepath.Join(procSelf, "exe")
	if err := os.Remove(exePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(chrootExePath, exePath); err != nil {
		return err
	}
	if len(argv) == 0 {
		argv = []string{chrootExePath}
	}
	var cmdline bytes.Buffer
	for i, arg := range argv {
		if i > 0 {
			cmdline.WriteByte(0)
		}
		cmdline.WriteString(arg)
	}
	cmdline.WriteByte(0)
	cmdlinePath := filepath.Join(procSelf, "cmdline")
	if err := os.WriteFile(cmdlinePath, cmdline.Bytes(), 0o444); err != nil {
		return err
	}
	return nil
}

// UpdateSandboxProcSelfExe is kept for tests; prefer UpdateSandboxProcSelfStub.
func UpdateSandboxProcSelfExe(sandboxPath, chrootExePath string) error {
	return UpdateSandboxProcSelfStub(sandboxPath, chrootExePath, []string{chrootExePath})
}
