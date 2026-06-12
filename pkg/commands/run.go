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
	"syscall"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	kConfig "github.com/xxmime/kaniko/pkg/config"
	"github.com/xxmime/kaniko/pkg/constants"
	"github.com/xxmime/kaniko/pkg/dockerfile"
	"github.com/xxmime/kaniko/pkg/util"
)

type RunCommand struct {
	BaseCommand
	cmd      *instructions.RunCommand
	shdCache bool
}

// for testing
var (
	userLookup = util.LookupUser
)

func (r *RunCommand) IsArgsEnvsRequiredInCache() bool {
	return true
}

func (r *RunCommand) ExecuteCommand(config *v1.Config, buildArgs *dockerfile.BuildArgs) error {
	return runCommandInExec(config, buildArgs, r.cmd)
}

func runCommandInExec(config *v1.Config, buildArgs *dockerfile.BuildArgs, cmdRun *instructions.RunCommand) error {
	var newCommand []string
	if cmdRun.PrependShell {
		// This is the default shell on Linux
		var shell []string
		if len(config.Shell) > 0 {
			shell = config.Shell
		} else {
			shell = append(shell, "/bin/sh", "-c")
		}

		newCommand = append(shell, strings.Join(cmdRun.CmdLine, " "))
	} else {
		newCommand = cmdRun.CmdLine
		// Find and set absolute path of executable by setting PATH temporary
		replacementEnvs := buildArgs.ReplacementEnvs(config.Env)
		for _, v := range replacementEnvs {
			entry := strings.SplitN(v, "=", 2)
			if entry[0] != "PATH" {
				continue
			}
			oldPath := os.Getenv("PATH")
			defer os.Setenv("PATH", oldPath)
			os.Setenv("PATH", entry[1])
			path, err := lookPath(newCommand[0], entry[1])
			if err == nil {
				newCommand[0] = path
			}
		}
	}

	logrus.Infof("Cmd: %s", newCommand[0])
	logrus.Infof("Args: %s", newCommand[1:])

	replacementEnvs := buildArgs.ReplacementEnvs(config.Env)
	workDir := setWorkDirIfExists(config.WorkingDir)

	u := config.User
	userAndGroup := strings.Split(u, ":")
	userStr, err := util.ResolveEnvironmentReplacement(userAndGroup[0], replacementEnvs, false)
	if err != nil {
		return errors.Wrapf(err, "resolving user %s", userAndGroup[0])
	}

	env, err := addDefaultHOME(userStr, replacementEnvs)
	if err != nil {
		return errors.Wrap(err, "adding default HOME variable")
	}

	var cmd *exec.Cmd
	usingProot := false
	if prootPath := sandboxProotPath(); prootPath != "" {
		usingProot = true
		// No real /proc could be bind-mounted (no CAP_SYS_ADMIN and no usable
		// user namespace). Run the command through proot, which emulates the
		// chroot and, crucially, a correct per-process /proc/self/exe in user
		// space so tools that call current_exe() (rustc, cargo, zig, the glibc
		// loader, ...) work. proot fakes uid/gid itself, so here we neither
		// chroot nor set credentials nor maintain the /proc/self/exe stub.
		cmd = buildSandboxProotCommand(prootPath, kConfig.RootDir, workDir, userStr, newCommand)
		logrus.Infof("Sandbox: running RUN through proot (%s) because /proc could not be bind-mounted", prootPath)
	} else {
		cmd = exec.Command(newCommand[0], newCommand[1:]...)
		cmd.Dir = workDir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if filepath.Clean(kConfig.RootDir) != "/" {
			cmd.SysProcAttr.Chroot = kConfig.RootDir
			if cmd.Dir == "" {
				cmd.Dir = "/"
			}
			lookPathFn := func(name string) (string, error) {
				return lookPath(name, pathEnvFrom(replacementEnvs))
			}
			validateFn := func(absPath string) error {
				return validateExecutableInRoot(absPath)
			}
			procExe := util.ResolveSandboxProcSelfExeTarget(newCommand, cmdRun.PrependShell, lookPathFn, validateFn)
			if err := util.UpdateSandboxProcSelfStub(kConfig.RootDir, procExe, newCommand); err != nil {
				logrus.Warnf("Sandbox: could not update /proc/self stub: %v", err)
			}
		}
		// If specified, run the command as a specific user.
		if userStr != "" {
			cmd.SysProcAttr.Credential, err = util.SyscallCredentials(userStr)
			if err != nil {
				return errors.Wrap(err, "credentials")
			}
		}
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env

	logrus.Infof("Running: %s", cmd.Args)
	if err := cmd.Start(); err != nil {
		if usingProot {
			return errors.Wrap(err, "starting command through proot (proot relies on the ptrace syscall; ensure the runner's seccomp profile permits ptrace)")
		}
		return errors.Wrap(err, "starting command")
	}

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return errors.Wrap(err, "getting group id for process")
	}
	if err := cmd.Wait(); err != nil {
		return errors.Wrap(err, "waiting for process to exit")
	}

	//it's not an error if there are no grandchildren
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && err.Error() != "no such process" {
		return err
	}
	return nil
}

// addDefaultHOME adds the default value for HOME if it isn't already set
func addDefaultHOME(u string, envs []string) ([]string, error) {
	for _, env := range envs {
		split := strings.SplitN(env, "=", 2)
		if split[0] == constants.HOME {
			return envs, nil
		}
	}

	// If user isn't set, set default value of HOME
	if u == "" || u == constants.RootUser {
		return append(envs, fmt.Sprintf("%s=%s", constants.HOME, constants.DefaultHOMEValue)), nil
	}

	// If user is set to username, set value of HOME to /home/${user}
	// Otherwise the user is set to uid and HOME is /
	userObj, err := userLookup(u)
	if err != nil {
		return nil, fmt.Errorf("lookup user %v: %w", u, err)
	}

	return append(envs, fmt.Sprintf("%s=%s", constants.HOME, userObj.HomeDir)), nil
}

// String returns some information about the command for the image config
func (r *RunCommand) String() string {
	return r.cmd.String()
}

func (r *RunCommand) FilesToSnapshot() []string {
	return nil
}

func (r *RunCommand) ProvidesFilesToSnapshot() bool {
	return false
}

// CacheCommand returns true since this command should be cached
func (r *RunCommand) CacheCommand(img v1.Image) DockerCommand {

	return &CachingRunCommand{
		img:       img,
		cmd:       r.cmd,
		extractFn: util.ExtractFile,
	}
}

func (r *RunCommand) MetadataOnly() bool {
	return false
}

func (r *RunCommand) RequiresUnpackedFS() bool {
	return true
}

func (r *RunCommand) ShouldCacheOutput() bool {
	return r.shdCache
}

type CachingRunCommand struct {
	BaseCommand
	caching
	img            v1.Image
	extractedFiles []string
	cmd            *instructions.RunCommand
	extractFn      util.ExtractFunction
}

func (cr *CachingRunCommand) IsArgsEnvsRequiredInCache() bool {
	return true
}

func (cr *CachingRunCommand) ExecuteCommand(config *v1.Config, buildArgs *dockerfile.BuildArgs) error {
	logrus.Infof("Found cached layer, extracting to filesystem")
	var err error

	if cr.img == nil {
		return errors.New(fmt.Sprintf("command image is nil %v", cr.String()))
	}

	layers, err := cr.img.Layers()
	if err != nil {
		return errors.Wrap(err, "retrieving image layers")
	}

	if len(layers) != 1 {
		return errors.New(fmt.Sprintf("expected %d layers but got %d", 1, len(layers)))
	}

	cr.layer = layers[0]

	cr.extractedFiles, err = util.GetFSFromLayers(
		kConfig.RootDir,
		layers,
		util.ExtractFunc(cr.extractFn),
		util.IncludeWhiteout(),
	)
	if err != nil {
		return errors.Wrap(err, "extracting fs from image")
	}

	return nil
}

func (cr *CachingRunCommand) FilesToSnapshot() []string {
	f := cr.extractedFiles
	logrus.Debugf("%d files extracted by caching run command", len(f))
	logrus.Tracef("Extracted files: %s", f)

	return f
}

func (cr *CachingRunCommand) String() string {
	if cr.cmd == nil {
		return "nil command"
	}
	return cr.cmd.String()
}

func (cr *CachingRunCommand) MetadataOnly() bool {
	return false
}

// sandboxProotPath returns the path to a proot binary when RUN commands should
// be executed through proot, or "" otherwise.
//
// proot is used only when building in sandbox mode (RootDir != "/") and a real
// /proc could NOT be bind-mounted (no CAP_SYS_ADMIN and no usable user
// namespace). In that situation the stub /proc/self/exe cannot satisfy tools
// that read it per-process (rustc, cargo, zig, the glibc loader, ...), but
// proot emulates both the chroot and a correct /proc/self/exe in user space.
// When a real /proc is available (host has CAP_SYS_ADMIN, or we re-exec'd into
// a user namespace) the native chroot is faster and equally correct, so proot
// is skipped.
func sandboxProotPath() string {
	if filepath.Clean(kConfig.RootDir) == "/" {
		return ""
	}
	if util.SandboxKernelFSBindMounted {
		return ""
	}
	if p := os.Getenv("KANIKO_PROOT"); p != "" {
		if isExecutableFile(p) {
			return p
		}
		logrus.Warnf("Sandbox: KANIKO_PROOT=%s is not an executable file; ignoring", p)
	}
	for _, cand := range []string{"/kaniko/proot", "/usr/local/bin/proot", "/busybox/proot"} {
		if isExecutableFile(cand) {
			return cand
		}
	}
	if p, err := exec.LookPath("proot"); err == nil {
		return p
	}
	return ""
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}

// buildSandboxProotCommand wraps command in a proot invocation that roots into
// root, binds the host's kernel filesystems (so /proc/self/exe resolves
// correctly) and fakes the requested uid/gid. proot performs the chroot and id
// mapping itself, so the returned *exec.Cmd must not set Chroot or Credential.
func buildSandboxProotCommand(prootPath, root, workDir, userStr string, command []string) *exec.Cmd {
	args := []string{"-r", root, "-b", "/proc", "-b", "/dev", "-b", "/sys"}
	if workDir != "" {
		args = append(args, "-w", workDir)
	}
	if spec := prootUserSpec(userStr); spec != "" {
		args = append(args, "-i", spec)
	} else {
		args = append(args, "-0")
	}
	args = append(args, command...)
	cmd := exec.Command(prootPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// prootUserSpec returns the "uid:gid" string proot should impersonate, or ""
// to run as fake root (proot -0). Most RUN steps need root (apk, adduser,
// chown); a non-root USER is resolved via the same credential lookup the
// native path uses.
func prootUserSpec(userStr string) string {
	switch userStr {
	case "", "root", "0", "0:0":
		return ""
	}
	cred, err := util.SyscallCredentials(userStr)
	if err != nil {
		logrus.Warnf("Sandbox: could not resolve user %q for proot, running as root: %v", userStr, err)
		return ""
	}
	return fmt.Sprintf("%d:%d", cred.Uid, cred.Gid)
}

// todo: this should create the workdir if it doesn't exist, atleast this is what docker does
func setWorkDirIfExists(workdir string) string {
	if workdir == "" {
		return ""
	}
	if _, err := os.Lstat(util.RootedPath(workdir)); err == nil {
		return workdir
	}
	return ""
}

func lookPath(file, pathEnv string) (string, error) {
	if filepath.Clean(kConfig.RootDir) == "/" {
		return exec.LookPath(file)
	}
	if strings.Contains(file, string(os.PathSeparator)) {
		if err := validateExecutableInRoot(file); err != nil {
			return "", err
		}
		return file, nil
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, file)
		if err := validateExecutableInRoot(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

func validateExecutableInRoot(file string) error {
	info, err := os.Stat(util.RootedPath(file))
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return os.ErrPermission
	}
	return nil
}

func pathEnvFrom(envs []string) string {
	for _, env := range envs {
		if after, ok := strings.CutPrefix(env, "PATH="); ok {
			return after
		}
	}
	return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
}
