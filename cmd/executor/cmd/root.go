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

package cmd

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/platforms"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/xxmime/kaniko/pkg/buildcontext"
	"github.com/xxmime/kaniko/pkg/config"
	"github.com/xxmime/kaniko/pkg/constants"
	"github.com/xxmime/kaniko/pkg/executor"
	"github.com/xxmime/kaniko/pkg/logging"
	"github.com/xxmime/kaniko/pkg/timing"
	"github.com/xxmime/kaniko/pkg/util"
	"github.com/xxmime/kaniko/pkg/util/proc"
	"golang.org/x/sys/unix"
)

var (
	opts         = &config.KanikoOptions{}
	ctxSubPath   string
	force        bool
	logLevel     string
	logFormat    string
	logTimestamp bool
)

func init() {
	RootCmd.PersistentFlags().StringVarP(&logLevel, "verbosity", "v", logging.DefaultLevel, "Log level (trace, debug, info, warn, error, fatal, panic)")
	RootCmd.PersistentFlags().StringVar(&logFormat, "log-format", logging.FormatColor, "Log format (text, color, json)")
	RootCmd.PersistentFlags().BoolVar(&logTimestamp, "log-timestamp", logging.DefaultLogTimestamp, "Timestamp in log output")
	RootCmd.PersistentFlags().BoolVarP(&force, "force", "", false, "Force building outside of a container")

	addKanikoOptionsFlags()
	addHiddenFlags(RootCmd)
	RootCmd.PersistentFlags().BoolVarP(&opts.IgnoreVarRun, "whitelist-var-run", "", true, "Ignore /var/run directory when taking image snapshot. Set it to false to preserve /var/run/ in destination image.")
	RootCmd.PersistentFlags().MarkDeprecated("whitelist-var-run", "Please use ignore-var-run instead.")
}

func validateFlags() {
	checkNoDeprecatedFlags()

	// Allow setting --registry-mirror using an environment variable.
	if val, ok := os.LookupEnv("KANIKO_REGISTRY_MIRROR"); ok {
		opts.RegistryMirrors.Set(val)
	}

	// Allow setting --no-push using an environment variable.
	if val, ok := os.LookupEnv("KANIKO_NO_PUSH"); ok {
		valBoolean, err := strconv.ParseBool(val)
		if err != nil {
			errors.New("invalid value (true/false) for KANIKO_NO_PUSH environment variable")
		}
		opts.NoPush = valBoolean
	}

	// Allow setting --registry-maps using an environment variable.
	if val, ok := os.LookupEnv("KANIKO_REGISTRY_MAP"); ok {
		opts.RegistryMaps.Set(val)
	}

	for _, target := range opts.RegistryMirrors {
		opts.RegistryMaps.Set(fmt.Sprintf("%s=%s", name.DefaultRegistry, target))
	}

	if len(opts.RegistryMaps) > 0 {
		for src, dsts := range opts.RegistryMaps {
			logrus.Debugf("registry-map remaps %s to %s.", src, strings.Join(dsts, ", "))
		}
	}

	// Default the custom platform flag to our current platform, and validate it.
	if opts.CustomPlatform == "" {
		opts.CustomPlatform = platforms.Format(platforms.Normalize(platforms.DefaultSpec()))
	}
	if _, err := v1.ParsePlatform(opts.CustomPlatform); err != nil {
		logrus.Fatalf("Invalid platform %q: %v", opts.CustomPlatform, err)
	}
}

// RootCmd is the kaniko command that is run
var RootCmd = &cobra.Command{
	Use: "executor",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Use == "executor" {

			if err := logging.Configure(logLevel, logFormat, logTimestamp); err != nil {
				return err
			}

			validateFlags()

			// Command line flag takes precedence over the KANIKO_DIR environment variable.
			dir := config.KanikoDir
			if opts.KanikoDir != constants.DefaultKanikoPath {
				dir = opts.KanikoDir
			}

			if err := checkKanikoDir(dir); err != nil {
				return err
			}
			if opts.Sandbox {
				if err := setupSandbox(); err != nil {
					return err
				}
			}

			resolveEnvironmentBuildArgs(opts.BuildArgs, os.Getenv)

			if !opts.NoPush && len(opts.Destinations) == 0 {
				return errors.New("you must provide --destination, or use --no-push")
			}
			if err := cacheFlagsValid(); err != nil {
				return errors.Wrap(err, "cache flags invalid")
			}
			if err := resolveSourceContext(); err != nil {
				return errors.Wrap(err, "error resolving source context")
			}
			if err := resolveDockerfilePath(); err != nil {
				return errors.Wrap(err, "error resolving dockerfile path")
			}
			if len(opts.Destinations) == 0 && opts.ImageNameDigestFile != "" {
				return errors.New("you must provide --destination if setting ImageNameDigestFile")
			}
			if len(opts.Destinations) == 0 && opts.ImageNameTagDigestFile != "" {
				return errors.New("you must provide --destination if setting ImageNameTagDigestFile")
			}
			// Update ignored paths
			if opts.IgnoreVarRun {
				// /var/run is a special case. It's common to mount in /var/run/docker.sock
				// or something similar which leads to a special mount on the /var/run/docker.sock
				// file itself, but the directory to exist in the image with no way to tell if it came
				// from the base image or not.
				logrus.Trace("Adding /var/run to default ignore list")
				util.AddToDefaultIgnoreList(util.IgnoreListEntry{
					Path:            "/var/run",
					PrefixMatchOnly: false,
				})
			}
			for _, p := range opts.IgnorePaths {
				util.AddToDefaultIgnoreList(util.IgnoreListEntry{
					Path:            p,
					PrefixMatchOnly: false,
				})
			}
		}
		return nil
	},
	Run: func(cmd *cobra.Command, args []string) {
		if !checkContained() {
			if !force {
				exit(errors.New("kaniko should only be run inside of a container, run with the --force flag if you are sure you want to continue"))
			}
			logrus.Warn("Kaniko is being run outside of a container. This can have dangerous effects on your system")
		}
		if !opts.NoPush || opts.CacheRepo != "" {
			if err := executor.CheckPushPermissions(opts); err != nil {
				exit(errors.Wrap(err, "error checking push permissions -- make sure you entered the correct tag name, and that you are authenticated correctly, and try again"))
			}
		}
		if err := resolveRelativePaths(); err != nil {
			exit(errors.Wrap(err, "error resolving relative paths to absolute paths"))
		}
		if err := os.Chdir("/"); err != nil {
			exit(errors.Wrap(err, "error changing to root dir"))
		}
		image, err := executor.DoBuild(opts)
		if err != nil {
			exit(errors.Wrap(err, "error building image"))
		}
		if err := executor.DoPush(image, opts); err != nil {
			exit(errors.Wrap(err, "error pushing image"))
		}

		if opts.Sandbox {
			teardownSandboxIfActive()
		}

		benchmarkFile := os.Getenv("BENCHMARK_FILE")
		// false is a keyword for integration tests to turn off benchmarking
		if benchmarkFile != "" && benchmarkFile != "false" {
			s, err := timing.JSON()
			if err != nil {
				logrus.Warnf("Unable to write benchmark file: %s", err)
				return
			}
			if strings.HasPrefix(benchmarkFile, "gs://") {
				logrus.Info("Uploading to gcs")
				if err := buildcontext.UploadToBucket(strings.NewReader(s), benchmarkFile); err != nil {
					logrus.Infof("Unable to upload %s due to %v", benchmarkFile, err)
				}
				logrus.Infof("Benchmark file written at %s", benchmarkFile)
			} else {
				f, err := os.Create(benchmarkFile)
				if err != nil {
					logrus.Warnf("Unable to create benchmarking file %s: %s", benchmarkFile, err)
					return
				}
				defer f.Close()
				f.WriteString(s)
				logrus.Infof("Benchmark file written at %s", benchmarkFile)
			}
		}
	},
}

// addKanikoOptionsFlags configures opts
func addKanikoOptionsFlags() {
	RootCmd.PersistentFlags().StringVarP(&opts.DockerfilePath, "dockerfile", "f", "Dockerfile", "Path to the dockerfile to be built.")
	RootCmd.PersistentFlags().StringVarP(&opts.SrcContext, "context", "c", "/workspace/", "Path to the dockerfile build context.")
	RootCmd.PersistentFlags().StringVarP(&ctxSubPath, "context-sub-path", "", "", "Sub path within the given context.")
	RootCmd.PersistentFlags().StringVarP(&opts.Bucket, "bucket", "b", "", "Name of the GCS bucket from which to access build context as tarball.")
	RootCmd.PersistentFlags().VarP(&opts.Destinations, "destination", "d", "Registry the final image should be pushed to. Set it repeatedly for multiple destinations.")
	RootCmd.PersistentFlags().StringVarP(&opts.SnapshotMode, "snapshot-mode", "", "full", "Change the file attributes inspected during snapshotting")
	RootCmd.PersistentFlags().StringVarP(&opts.CustomPlatform, "custom-platform", "", "", "Specify the build platform if different from the current host")
	RootCmd.PersistentFlags().VarP(&opts.BuildArgs, "build-arg", "", "This flag allows you to pass in ARG values at build time. Set it repeatedly for multiple values.")
	RootCmd.PersistentFlags().BoolVarP(&opts.Insecure, "insecure", "", false, "Push to insecure registry using plain HTTP")
	RootCmd.PersistentFlags().BoolVarP(&opts.SkipTLSVerify, "skip-tls-verify", "", false, "Push to insecure registry ignoring TLS verify")
	RootCmd.PersistentFlags().BoolVarP(&opts.InsecurePull, "insecure-pull", "", false, "Pull from insecure registry using plain HTTP")
	RootCmd.PersistentFlags().BoolVarP(&opts.SkipTLSVerifyPull, "skip-tls-verify-pull", "", false, "Pull from insecure registry ignoring TLS verify")
	RootCmd.PersistentFlags().IntVar(&opts.PushRetry, "push-retry", 0, "Number of retries for the push operation")
	RootCmd.PersistentFlags().BoolVar(&opts.PushIgnoreImmutableTagErrors, "push-ignore-immutable-tag-errors", false, "If true, known tag immutability errors are ignored and the push finishes with success.")
	RootCmd.PersistentFlags().IntVar(&opts.ImageFSExtractRetry, "image-fs-extract-retry", 0, "Number of retries for image FS extraction")
	RootCmd.PersistentFlags().IntVar(&opts.ImageDownloadRetry, "image-download-retry", 0, "Number of retries for downloading the remote image")
	RootCmd.PersistentFlags().StringVarP(&opts.KanikoDir, "kaniko-dir", "", constants.DefaultKanikoPath, "Path to the kaniko directory, this takes precedence over the KANIKO_DIR environment variable.")
	RootCmd.PersistentFlags().StringVarP(&opts.TarPath, "tar-path", "", "", "Path to save the image in as a tarball instead of pushing")
	RootCmd.PersistentFlags().BoolVarP(&opts.SingleSnapshot, "single-snapshot", "", false, "Take a single snapshot at the end of the build.")
	RootCmd.PersistentFlags().BoolVarP(&opts.Reproducible, "reproducible", "", false, "Strip timestamps out of the image to make it reproducible")
	RootCmd.PersistentFlags().StringVarP(&opts.Target, "target", "", "", "Set the target build stage to build")
	RootCmd.PersistentFlags().BoolVarP(&opts.NoPush, "no-push", "", false, "Do not push the image to the registry")
	RootCmd.PersistentFlags().BoolVarP(&opts.NoPushCache, "no-push-cache", "", false, "Do not push the cache layers to the registry")
	RootCmd.PersistentFlags().StringVarP(&opts.CacheRepo, "cache-repo", "", "", "Specify a repository to use as a cache, otherwise one will be inferred from the destination provided; when prefixed with 'oci:' the repository will be written in OCI image layout format at the path provided")
	RootCmd.PersistentFlags().StringVarP(&opts.CacheDir, "cache-dir", "", "/cache", "Specify a local directory to use as a cache.")
	RootCmd.PersistentFlags().StringVarP(&opts.DigestFile, "digest-file", "", "", "Specify a file to save the digest of the built image to.")
	RootCmd.PersistentFlags().StringVarP(&opts.ImageNameDigestFile, "image-name-with-digest-file", "", "", "Specify a file to save the image name w/ digest of the built image to.")
	RootCmd.PersistentFlags().StringVarP(&opts.ImageNameTagDigestFile, "image-name-tag-with-digest-file", "", "", "Specify a file to save the image name w/ image tag w/ digest of the built image to.")
	RootCmd.PersistentFlags().StringVarP(&opts.OCILayoutPath, "oci-layout-path", "", "", "Path to save the OCI image layout of the built image.")
	RootCmd.PersistentFlags().VarP(&opts.Compression, "compression", "", "Compression algorithm (gzip, zstd)")
	RootCmd.PersistentFlags().IntVarP(&opts.CompressionLevel, "compression-level", "", -1, "Compression level")
	RootCmd.PersistentFlags().BoolVarP(&opts.Cache, "cache", "", false, "Use cache when building image")
	RootCmd.PersistentFlags().BoolVarP(&opts.CompressedCaching, "compressed-caching", "", true, "Compress the cached layers. Decreases build time, but increases memory usage.")
	RootCmd.PersistentFlags().BoolVarP(&opts.Cleanup, "cleanup", "", false, "Clean the filesystem at the end")
	RootCmd.PersistentFlags().DurationVarP(&opts.CacheTTL, "cache-ttl", "", time.Hour*336, "Cache timeout, requires value and unit of duration -> ex: 6h. Defaults to two weeks.")
	RootCmd.PersistentFlags().VarP(&opts.InsecureRegistries, "insecure-registry", "", "Insecure registry using plain HTTP to push and pull. Set it repeatedly for multiple registries.")
	RootCmd.PersistentFlags().VarP(&opts.SkipTLSVerifyRegistries, "skip-tls-verify-registry", "", "Insecure registry ignoring TLS verify to push and pull. Set it repeatedly for multiple registries.")
	opts.RegistriesCertificates = make(map[string]string)
	RootCmd.PersistentFlags().VarP(&opts.RegistriesCertificates, "registry-certificate", "", "Use the provided certificate for TLS communication with the given registry. Expected format is 'my.registry.url=/path/to/the/server/certificate'.")
	opts.RegistriesClientCertificates = make(map[string]string)
	RootCmd.PersistentFlags().VarP(&opts.RegistriesClientCertificates, "registry-client-cert", "", "Use the provided client certificate for mutual TLS (mTLS) communication with the given registry. Expected format is 'my.registry.url=/path/to/client/cert,/path/to/client/key'.")
	opts.RegistryMaps = make(map[string][]string)
	RootCmd.PersistentFlags().VarP(&opts.RegistryMaps, "registry-map", "", "Registry map of mirror to use as pull-through cache instead. Expected format is 'orignal.registry=new.registry;other-original.registry=other-remap.registry'")
	RootCmd.PersistentFlags().VarP(&opts.RegistryMirrors, "registry-mirror", "", "Registry mirror to use as pull-through cache instead of docker.io. Set it repeatedly for multiple mirrors.")
	RootCmd.PersistentFlags().BoolVarP(&opts.SkipDefaultRegistryFallback, "skip-default-registry-fallback", "", false, "If an image is not found on any mirrors (defined with registry-mirror) do not fallback to the default registry. If registry-mirror is not defined, this flag is ignored.")
	RootCmd.PersistentFlags().BoolVarP(&opts.IgnoreVarRun, "ignore-var-run", "", true, "Ignore /var/run directory when taking image snapshot. Set it to false to preserve /var/run/ in destination image.")
	RootCmd.PersistentFlags().VarP(&opts.Labels, "label", "", "Set metadata for an image. Set it repeatedly for multiple labels.")
	RootCmd.PersistentFlags().BoolVarP(&opts.SkipUnusedStages, "skip-unused-stages", "", false, "Build only used stages if defined to true. Otherwise it builds by default all stages, even the unnecessaries ones until it reaches the target stage / end of Dockerfile")
	RootCmd.PersistentFlags().BoolVarP(&opts.RunV2, "use-new-run", "", false, "Use the experimental run implementation for detecting changes without requiring file system snapshots.")
	RootCmd.PersistentFlags().Var(&opts.Git, "git", "Branch to clone if build context is a git repository")
	RootCmd.PersistentFlags().BoolVarP(&opts.CacheCopyLayers, "cache-copy-layers", "", false, "Caches copy layers")
	RootCmd.PersistentFlags().BoolVarP(&opts.CacheRunLayers, "cache-run-layers", "", true, "Caches run layers")
	RootCmd.PersistentFlags().VarP(&opts.IgnorePaths, "ignore-path", "", "Ignore these paths when taking a snapshot. Set it repeatedly for multiple paths.")
	RootCmd.PersistentFlags().BoolVarP(&opts.ForceBuildMetadata, "force-build-metadata", "", false, "Force add metadata layers to build image")
	RootCmd.PersistentFlags().BoolVarP(&opts.SkipPushPermissionCheck, "skip-push-permission-check", "", false, "Skip check of the push permission")
	RootCmd.PersistentFlags().BoolVarP(&opts.Sandbox, "sandbox", "", false, "Build the image filesystem inside /kaniko/sandbox instead of the container root filesystem.")

	// Deprecated flags.
	RootCmd.PersistentFlags().StringVarP(&opts.SnapshotModeDeprecated, "snapshotMode", "", "", "This flag is deprecated. Please use '--snapshot-mode'.")
	RootCmd.PersistentFlags().StringVarP(&opts.CustomPlatformDeprecated, "customPlatform", "", "", "This flag is deprecated. Please use '--custom-platform'.")
	RootCmd.PersistentFlags().StringVarP(&opts.TarPath, "tarPath", "", "", "This flag is deprecated. Please use '--tar-path'.")
}

// addHiddenFlags marks certain flags as hidden from the executor help text
func addHiddenFlags(cmd *cobra.Command) {
	// This flag is added in a vendored directory, hide so that it doesn't come up via --help
	pflag.CommandLine.MarkHidden("azure-container-registry-config")
	// Hide this flag as we want to encourage people to use the --context flag instead
	cmd.PersistentFlags().MarkHidden("bucket")
}

// Environment variable used to mark the user-namespace re-exec child so we
// don't infinitely re-exec.  Set to "skip" to opt out of automatic user
// namespace re-exec entirely (e.g. when the runtime really does have
// CAP_SYS_ADMIN but lies in /proc/self/status, or for debugging).
const sandboxUserNSEnvVar = "KANIKO_SANDBOX_USERNS"

// Values written into KANIKO_SANDBOX_USERNS by reExecInUserNamespace.
const (
	sandboxUserNSFull = "1"      // child was started with CLONE_NEWUSER|CLONE_NEWNS
	sandboxUserNSOnly = "userns" // child was started with CLONE_NEWUSER; must CLONE_NEWNS locally
)

func setupSandbox() error {
	sandboxPath := filepath.Clean(constants.DefaultSandboxPath)
	if sandboxPath == string(os.PathSeparator) || sandboxPath == "." {
		return errors.Errorf("refusing to use unsafe sandbox path %s", sandboxPath)
	}
	if !strings.HasPrefix(sandboxPath, filepath.Clean(constants.DefaultKanikoPath)+string(os.PathSeparator)) {
		return errors.Errorf("sandbox path %s must be under %s", sandboxPath, constants.DefaultKanikoPath)
	}
	if info, err := os.Lstat(constants.DefaultKanikoPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.Errorf("kaniko directory %s must not be a symlink when sandbox mode is enabled", constants.DefaultKanikoPath)
	}

	// If we lack CAP_SYS_ADMIN we cannot bind-mount /proc, /sys, /dev into
	// the sandbox, and RUN commands that depend on /proc/self/exe (zig,
	// python, the glibc loader, ...) will fail with FileNotFound. The
	// portable fix is to re-execute kaniko inside a user+mount namespace,
	// where we have full caps with respect to that namespace and can
	// bind-mount freely. The kernel destroys all mounts when the namespace
	// goes away, so there is nothing to clean up.
	nsState := os.Getenv(sandboxUserNSEnvVar)
	switch nsState {
	case "":
		if !hasCapSysAdmin() {
			logrus.Info("Sandbox: CAP_SYS_ADMIN not available; re-executing inside a user namespace so /proc, /sys, /dev can be bind-mounted (set KANIKO_SANDBOX_USERNS=skip to disable)")
			if err := reExecInUserNamespace(); err != nil {
				logrus.Warnf("Sandbox: user-namespace re-exec unavailable (%v); continuing without it — bind-mounting /proc, /sys, /dev requires CAP_SYS_ADMIN or a working user namespace", err)
			}
		}
	case "skip":
		if !hasCapSysAdmin() {
			logrus.Warn("Sandbox: CAP_SYS_ADMIN not available and user-namespace re-exec was skipped (KANIKO_SANDBOX_USERNS=skip); RUN commands that need /proc, /sys or /dev may fail with FileNotFound")
		}
	case sandboxUserNSFull:
		logrus.Debug("Sandbox: running inside user+mount namespace re-exec child")
	case sandboxUserNSOnly:
		logrus.Debug("Sandbox: creating mount namespace inside user-namespace re-exec child")
		if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
			logrus.Warnf("Sandbox: unshare(CLONE_NEWNS) failed after user-namespace re-exec: %v", err)
		}
	default:
		logrus.Debugf("Sandbox: running inside user-namespace re-exec child (%s=%s)", sandboxUserNSEnvVar, nsState)
	}

	if info, err := os.Lstat(sandboxPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.Errorf("sandbox path %s must not be a symlink", sandboxPath)
		}
		if !info.IsDir() {
			return errors.Errorf("sandbox path %s exists and is not a directory", sandboxPath)
		}
	} else if os.IsNotExist(err) {
		if err := os.MkdirAll(sandboxPath, 0o755); err != nil {
			return errors.Wrap(err, "creating sandbox directory")
		}
	} else {
		return errors.Wrap(err, "checking sandbox directory")
	}

	// Detach leftover bind-mounts and wipe contents from a previous run.
	if err := wipeSandboxContents(sandboxPath); err != nil {
		return errors.Wrap(err, "resetting sandbox")
	}

	config.RootDir = sandboxPath
	mountSandboxKernelFilesystems(sandboxPath)
	setupSandboxRuntimeFiles(sandboxPath)
	activeSandboxPath = sandboxPath
	installSandboxCleanup()
	logrus.Infof("Sandbox mode enabled. Image filesystem root: %s", config.RootDir)
	return nil
}

const capSysAdminBit = 21 // CAP_SYS_ADMIN, see capabilities(7).

// hasCapSysAdmin returns true when CAP_SYS_ADMIN is present in the effective
// or permitted capability set. Some runtimes drop it from CapEff while leaving
// it in CapPrm; checking both avoids a pointless user-namespace re-exec.
func hasCapSysAdmin() bool {
	return capStatusHasSysAdmin("CapEff") || capStatusHasSysAdmin("CapPrm")
}

func capStatusHasSysAdmin(field string) bool {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	prefix := field + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		val, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 16, 64)
		if err != nil {
			return false
		}
		return val&(1<<capSysAdminBit) != 0
	}
	return false
}

// sandboxStandardIDMapSize is how many host uid/gid values are mapped 1:1 into
// the user namespace when kaniko runs as root. 65536 covers typical image tar
// entries (e.g. alpine /etc/shadow gid=42) and RUN steps such as adduser/chown.
const sandboxStandardIDMapSize = 65536

// sandboxUserNamespaceIDMaps returns uid_map / gid_map entries for the
// user-namespace re-exec child. When the caller is root we map the standard
// 16-bit id space 1:1 so RUN adduser/chown and base-layer extraction both work.
// Non-root callers only get a single uid/gid mapped (kernel restriction).
func sandboxUserNamespaceIDMaps(callerUID, callerGID int) (uidMaps, gidMaps []syscall.SysProcIDMap, enableSetgroups bool) {
	if callerUID == 0 && callerGID == 0 {
		m := []syscall.SysProcIDMap{{ContainerID: 0, HostID: 0, Size: sandboxStandardIDMapSize}}
		return m, m, true
	}
	return []syscall.SysProcIDMap{{ContainerID: 0, HostID: callerUID, Size: 1}},
		[]syscall.SysProcIDMap{{ContainerID: 0, HostID: callerGID, Size: 1}},
		false
}

type sandboxReExecStrategy struct {
	name       string
	cloneflags uintptr
	nsEnvValue string
	fullIDMap  bool
}

// reExecInUserNamespace tries several user-namespace strategies and re-executes
// kaniko via /proc/self/exe. The child inherits a full capability set with
// respect to its own user namespace, so it can bind-mount /proc, /sys and /dev
// into /kaniko/sandbox without host-level CAP_SYS_ADMIN.
//
// On success this function does NOT return; it exits the parent with the
// child's exit code. When every strategy fails it returns an error so the
// caller can continue without user-namespace isolation.
func reExecInUserNamespace() error {
	strategies := []sandboxReExecStrategy{
		{
			name:       "user+mount namespace, full uid map",
			cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
			nsEnvValue: sandboxUserNSFull,
			fullIDMap:  true,
		},
		{
			name:       "user namespace only, full uid map",
			cloneflags: syscall.CLONE_NEWUSER,
			nsEnvValue: sandboxUserNSOnly,
			fullIDMap:  true,
		},
		{
			name:       "user+mount namespace, single uid map",
			cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
			nsEnvValue: sandboxUserNSFull,
			fullIDMap:  false,
		},
		{
			name:       "user namespace only, single uid map",
			cloneflags: syscall.CLONE_NEWUSER,
			nsEnvValue: sandboxUserNSOnly,
			fullIDMap:  false,
		},
	}
	var parts []string
	for _, s := range strategies {
		if err := tryReExecInUserNamespace(s); err != nil {
			logrus.Debugf("Sandbox: re-exec strategy %q failed: %v", s.name, err)
			parts = append(parts, fmt.Sprintf("%s: %v", s.name, err))
			continue
		}
	}
	return errors.Errorf(
		"all user-namespace re-exec strategies failed (%s); grant CAP_SYS_ADMIN to the kaniko container or enable unprivileged user namespaces",
		strings.Join(parts, "; "),
	)
}

func tryReExecInUserNamespace(s sandboxReExecStrategy) error {
	// Always exec through /proc/self/exe. Resolving the path with readlink can
	// point at a noexec mount or a location blocked by LSM/seccomp.
	cmd := exec.Command("/proc/self/exe", os.Args[1:]...)
	cmd.Env = append(os.Environ(), sandboxUserNSEnvVar+"="+s.nsEnvValue)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	var uidMaps, gidMaps []syscall.SysProcIDMap
	var enableSetgroups bool
	if s.fullIDMap {
		uidMaps, gidMaps, enableSetgroups = sandboxUserNamespaceIDMaps(os.Getuid(), os.Getgid())
		if enableSetgroups {
			logrus.Infof("Sandbox: trying %s — mapping uids/gids 0-%d 1:1", s.name, sandboxStandardIDMapSize-1)
		} else {
			logrus.Warnf("Sandbox: trying %s — mapping only container uid/gid 0 -> host %d/%d", s.name, os.Getuid(), os.Getgid())
		}
	} else {
		uidMaps = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		gidMaps = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
		logrus.Infof("Sandbox: trying %s — mapping uid/gid 0 -> host %d/%d", s.name, os.Getuid(), os.Getgid())
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 s.cloneflags,
		UidMappings:                uidMaps,
		GidMappings:                gidMaps,
		GidMappingsEnableSetgroups: enableSetgroups,
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	// Forward common signals from the parent so e.g. CI cancellation kills
	// the child cleanly. PID namespace is shared so this also lets us avoid
	// orphaning the child if our parent dies.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-sigCh:
				if cmd.Process != nil {
					_ = cmd.Process.Signal(sig)
				}
			case <-done:
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	close(done)
	signal.Stop(sigCh)

	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return waitErr
	}
	os.Exit(0)
	return nil // unreachable; satisfies the compiler.
}

// activeSandboxPath is set once setupSandbox succeeds. teardownSandboxIfActive
// uses it to unmount kernel bind-mounts and remove build artifacts when kaniko
// exits (success, failure, or signal).
var activeSandboxPath string

// sandboxCleanupOnce guards installSandboxCleanup so repeated calls (e.g. when
// running unit tests through the same process) don't stack signal handlers.
var sandboxCleanupOnce sync.Once

// sandboxTeardownOnce ensures we only wipe the sandbox once per process.
var sandboxTeardownOnce sync.Once

// wipeSandboxContents lazily unmounts kernel bind-mounts under sandboxPath,
// then deletes every entry inside the directory. The sandbox directory itself
// is kept so the next run can reuse it.
func wipeSandboxContents(sandboxPath string) error {
	if err := unmountSandboxMounts(sandboxPath); err != nil {
		logrus.Warnf("Sandbox: failed to unmount filesystems under %s: %v", sandboxPath, err)
	}
	entries, err := os.ReadDir(sandboxPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrap(err, "reading sandbox directory")
	}
	var firstErr error
	for _, entry := range entries {
		entryPath := filepath.Join(sandboxPath, entry.Name())
		if err := os.RemoveAll(entryPath); err != nil {
			logrus.Warnf("Sandbox: failed to remove %s: %v", entryPath, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// teardownSandboxIfActive unmounts /proc, /sys, /dev bind-mounts and removes
// all files left in /kaniko/sandbox. Safe to call multiple times.
func teardownSandboxIfActive() {
	path := activeSandboxPath
	if path == "" {
		return
	}
	sandboxTeardownOnce.Do(func() {
		logrus.Infof("Sandbox: cleaning up %s", path)
		if err := wipeSandboxContents(path); err != nil {
			logrus.Warnf("Sandbox: cleanup incomplete: %v", err)
			return
		}
		logrus.Infof("Sandbox: cleanup complete (%s is empty)", path)
	})
}

// installSandboxCleanup arranges for the sandbox to be wiped when kaniko
// receives SIGINT or SIGTERM so a cancelled CI run does not leave bind-mounts
// or a full unpacked rootfs under /kaniko/sandbox/.
func installSandboxCleanup() {
	sandboxCleanupOnce.Do(func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			sig := <-ch
			logrus.Infof("Sandbox: received %s, cleaning up sandbox", sig)
			teardownSandboxIfActive()
			// Re-raise the signal with the default handler so we exit with
			// the usual 128+signo status instead of a clean 0.
			signal.Reset(sig.(syscall.Signal))
			_ = syscall.Kill(syscall.Getpid(), sig.(syscall.Signal))
		}()
	})
}

// sandboxKernelMounts lists the host pseudo-filesystems that must be visible
// inside the chrooted sandbox so that programs invoked by RUN can resolve
// /proc/self/exe, open /dev/null, etc. Without /proc, statically linked tools
// like Zig fail with FileNotFound when they try to locate their own install
// directory.
var sandboxKernelMounts = []struct {
	source string
	target string
	mode   os.FileMode
}{
	{"/proc", "proc", 0o555},
	{"/sys", "sys", 0o555},
	{"/dev", "dev", 0o755},
}

// mountSandboxKernelFilesystems bind-mounts /proc, /sys and /dev from the host
// into the sandbox so that RUN commands (chrooted into /kaniko/sandbox) can
// resolve runtime paths like /proc/self/exe, open /dev/null, /dev/urandom,
// etc. The mounts are made MS_PRIVATE|MS_REC right after binding so a later
// unmount cannot propagate back to the host's /proc, /sys, /dev. Mount
// targets are always added to the default ignore list (even when the mount
// itself fails) so they never end up in the resulting image.
//
// Failures are logged but not fatal: kaniko may run without CAP_SYS_ADMIN,
// in which case RUN commands that depend on these paths will surface their
// own errors (typically "FileNotFound"). The warning makes that diagnosis
// obvious.
func mountSandboxKernelFilesystems(sandboxPath string) {
	util.SandboxKernelFSBindMounted = false
	util.SandboxProcSelfStub = false
	mounted := make([]string, 0, len(sandboxKernelMounts))
	mountedTargets := make(map[string]bool, len(sandboxKernelMounts))
	for _, m := range sandboxKernelMounts {
		target := filepath.Join(sandboxPath, m.target)

		// Always ignore the target, even if we fail to mount on top of it,
		// otherwise an empty directory would be baked into the resulting image.
		util.AddToDefaultIgnoreList(util.IgnoreListEntry{
			Path:            target,
			PrefixMatchOnly: false,
		})

		if _, err := os.Stat(m.source); err != nil {
			logrus.Warnf("Sandbox kernel fs %s not available on host; skipping bind mount: %v", m.source, err)
			continue
		}
		if err := os.MkdirAll(target, m.mode); err != nil {
			logrus.Warnf("Sandbox: cannot create bind-mount target %s for %s: %v", target, m.source, err)
			continue
		}
		if err := unix.Mount(m.source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			logrus.Warnf("Sandbox: bind-mount %s -> %s failed (RUN commands relying on %s may fail with FileNotFound; ensure the kaniko container has CAP_SYS_ADMIN): %v", m.source, target, m.source, err)
			continue
		}
		// Detach the new mount from the source's propagation group so that a
		// later lazy unmount cannot propagate back to the host's /proc,
		// /sys, /dev. This is the standard pattern used by container
		// runtimes when bind-mounting kernel filesystems.
		if err := unix.Mount("none", target, "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
			logrus.Warnf("Sandbox: could not make %s private (host propagation risk): %v", target, err)
		}
		mounted = append(mounted, target)
		mountedTargets[m.target] = true
		logrus.Infof("Sandbox: bind-mounted %s into %s", m.source, target)
	}
	if len(mounted) > 0 {
		logrus.Infof("Sandbox: %d kernel filesystem(s) bind-mounted: %s", len(mounted), strings.Join(mounted, ", "))
	}
	util.SandboxKernelFSBindMounted = len(mounted) == len(sandboxKernelMounts)
	needProc := !mountedTargets["proc"]
	needSys := !mountedTargets["sys"]
	needDev := !mountedTargets["dev"]
	if needProc || needSys || needDev {
		if err := util.SetupSandboxStubKernelFilesystems(sandboxPath, needProc, needSys, needDev); err != nil {
			logrus.Warnf("Sandbox: failed to set up stub kernel filesystems: %v", err)
		}
	}
}

// unmountSandboxMounts lazily unmounts every filesystem currently mounted at
// or under sandboxPath. We use MNT_DETACH so leftover open file descriptors
// from a previous run (e.g. a kaniko process that crashed) don't keep us from
// resetting the sandbox. mountinfo's mount-point field can contain escaped
// characters (e.g. \040 for space, \011 for tab) which we decode before
// comparing against sandboxPath.
func unmountSandboxMounts(sandboxPath string) error {
	mountInfo := config.MountInfoPath
	if mountInfo == "" {
		mountInfo = constants.MountInfoPath
	}
	data, err := os.ReadFile(mountInfo)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	prefix := sandboxPath + string(os.PathSeparator)
	var mountPoints []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, " ")
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMountField(fields[4])
		if mp == sandboxPath || strings.HasPrefix(mp, prefix) {
			mountPoints = append(mountPoints, mp)
		}
	}
	// Unmount children before parents so we don't leave dangling submounts.
	for i := len(mountPoints) - 1; i >= 0; i-- {
		if err := unix.Unmount(mountPoints[i], unix.MNT_DETACH); err != nil {
			logrus.Debugf("Failed to lazily unmount %s: %v", mountPoints[i], err)
		}
	}
	return nil
}

// unescapeMountField decodes the octal escapes (\040, \011, \012, \134) the
// kernel uses for spaces, tabs, newlines and backslashes inside the mount
// point field of /proc/self/mountinfo.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			c1, c2, c3 := s[i+1], s[i+2], s[i+3]
			if c1 >= '0' && c1 <= '7' && c2 >= '0' && c2 <= '7' && c3 >= '0' && c3 <= '7' {
				b.WriteByte((c1-'0')<<6 | (c2-'0')<<3 | (c3 - '0'))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func setupSandboxRuntimeFiles(sandboxPath string) {
	for _, path := range []string{"/etc/resolv.conf", "/etc/hosts", "/etc/hostname"} {
		util.AddToDefaultIgnoreList(util.IgnoreListEntry{
			Path:            path,
			PrefixMatchOnly: false,
		})
		if err := copySandboxRuntimeFile(path, sandboxPath); err != nil {
			logrus.Debugf("Not copying runtime file %s into sandbox: %v", path, err)
		}
	}
}

func copySandboxRuntimeFile(path, sandboxPath string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dest := filepath.Join(sandboxPath, strings.TrimPrefix(filepath.Clean(path), string(os.PathSeparator)))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dest, contents, 0o644)
}

// checkKanikoDir will check whether the executor is operating in the default '/kaniko' directory,
// conducting the relevant operations if it is not
func checkKanikoDir(dir string) error {
	if dir != constants.DefaultKanikoPath {

		// The destination directory may be across a different partition, so we cannot simply rename/move the directory in this case.
		if _, err := util.CopyDir(constants.DefaultKanikoPath, dir, util.FileContext{}, util.DoNotChangeUID, util.DoNotChangeGID, fs.FileMode(0o600), true); err != nil {
			return err
		}

		if err := os.RemoveAll(constants.DefaultKanikoPath); err != nil {
			return err
		}
		// After remove DefaultKankoPath, the DOKCER_CONFIG env will point to a non-exist dir, so we should update DOCKER_CONFIG env to new dir
		if err := os.Setenv("DOCKER_CONFIG", filepath.Join(dir, "/.docker")); err != nil {
			return err
		}
	}
	return nil
}

func checkContained() bool {
	return proc.GetContainerRuntime(0, 0) != proc.RuntimeNotFound
}

// checkNoDeprecatedFlags return an error if deprecated flags are used.
func checkNoDeprecatedFlags() {
	// In version >=2.0.0 make it fail (`Warn` -> `Fatal`)
	if opts.CustomPlatformDeprecated != "" {
		logrus.Warn("Flag --customPlatform is deprecated. Use: --custom-platform")
		opts.CustomPlatform = opts.CustomPlatformDeprecated
	}

	if opts.SnapshotModeDeprecated != "" {
		logrus.Warn("Flag --snapshotMode is deprecated. Use: --snapshot-mode")
		opts.SnapshotMode = opts.SnapshotModeDeprecated
	}

	if opts.TarPathDeprecated != "" {
		logrus.Warn("Flag --tarPath is deprecated. Use: --tar-path")
		opts.TarPath = opts.TarPathDeprecated
	}
}

// cacheFlagsValid makes sure the flags passed in related to caching are valid
func cacheFlagsValid() error {
	if !opts.Cache {
		return nil
	}
	// If --cache=true and --no-push=true, then cache repo must be provided
	// since cache can't be inferred from destination
	if opts.CacheRepo == "" && opts.NoPush {
		return errors.New("if using cache with --no-push, specify cache repo with --cache-repo")
	}
	return nil
}

// resolveDockerfilePath resolves the Dockerfile path to an absolute path
func resolveDockerfilePath() error {
	if isURL(opts.DockerfilePath) {
		return nil
	}
	if util.FilepathExists(opts.DockerfilePath) {
		abs, err := filepath.Abs(opts.DockerfilePath)
		if err != nil {
			return errors.Wrap(err, "getting absolute path for dockerfile")
		}
		opts.DockerfilePath = abs
		return copyDockerfile()
	}
	// Otherwise, check if the path relative to the build context exists
	if util.FilepathExists(filepath.Join(opts.SrcContext, opts.DockerfilePath)) {
		abs, err := filepath.Abs(filepath.Join(opts.SrcContext, opts.DockerfilePath))
		if err != nil {
			return errors.Wrap(err, "getting absolute path for src context/dockerfile path")
		}
		opts.DockerfilePath = abs
		return copyDockerfile()
	}
	return errors.New("please provide a valid path to a Dockerfile within the build context with --dockerfile")
}

// resolveEnvironmentBuildArgs replace build args without value by the same named environment variable
func resolveEnvironmentBuildArgs(arguments []string, resolver func(string) string) {
	for index, argument := range arguments {
		i := strings.Index(argument, "=")
		if i < 0 {
			value := resolver(argument)
			arguments[index] = fmt.Sprintf("%s=%s", argument, value)
		}
	}
}

// copy Dockerfile to /kaniko/Dockerfile so that if it's specified in the .dockerignore
// it won't be copied into the image
func copyDockerfile() error {
	if _, err := util.CopyFile(opts.DockerfilePath, config.DockerfilePath, util.FileContext{}, util.DoNotChangeUID, util.DoNotChangeGID, fs.FileMode(0o600), true); err != nil {
		return errors.Wrap(err, "copying dockerfile")
	}
	dockerignorePath := opts.DockerfilePath + ".dockerignore"
	if util.FilepathExists(dockerignorePath) {
		if _, err := util.CopyFile(dockerignorePath, config.DockerfilePath+".dockerignore", util.FileContext{}, util.DoNotChangeUID, util.DoNotChangeGID, fs.FileMode(0o600), true); err != nil {
			return errors.Wrap(err, "copying Dockerfile.dockerignore")
		}
	}
	opts.DockerfilePath = config.DockerfilePath
	return nil
}

// resolveSourceContext unpacks the source context if it is a tar in a bucket or in kaniko container
// it resets srcContext to be the path to the unpacked build context within the image
func resolveSourceContext() error {
	if opts.SrcContext == "" && opts.Bucket == "" {
		return errors.New("please specify a path to the build context with the --context flag or a bucket with the --bucket flag")
	}
	if opts.SrcContext != "" && !strings.Contains(opts.SrcContext, "://") {
		return nil
	}
	if opts.Bucket != "" {
		if !strings.Contains(opts.Bucket, "://") {
			// if no prefix use Google Cloud Storage as default for backwards compatibility
			opts.SrcContext = constants.GCSBuildContextPrefix + opts.Bucket
		} else {
			opts.SrcContext = opts.Bucket
		}
	}
	contextExecutor, err := buildcontext.GetBuildContext(opts.SrcContext, buildcontext.BuildOptions{
		GitBranch:            opts.Git.Branch,
		GitSingleBranch:      opts.Git.SingleBranch,
		GitRecurseSubmodules: opts.Git.RecurseSubmodules,
		InsecureSkipTLS:      opts.Git.InsecureSkipTLS,
	})
	if err != nil {
		return err
	}
	logrus.Debugf("Getting source context from %s", opts.SrcContext)
	opts.SrcContext, err = contextExecutor.UnpackTarFromBuildContext()
	if err != nil {
		return err
	}
	if ctxSubPath != "" {
		opts.SrcContext = filepath.Join(opts.SrcContext, ctxSubPath)
		if _, err := os.Stat(opts.SrcContext); os.IsNotExist(err) {
			return err
		}
	}
	logrus.Debugf("Build context located at %s", opts.SrcContext)
	return nil
}

func resolveRelativePaths() error {
	optsPaths := []*string{
		&opts.DockerfilePath,
		&opts.SrcContext,
		&opts.CacheDir,
		&opts.TarPath,
		&opts.DigestFile,
		&opts.ImageNameDigestFile,
		&opts.ImageNameTagDigestFile,
	}

	for _, p := range optsPaths {
		if path := *p; shdSkip(path) {
			logrus.Debugf("Skip resolving path %s", path)
			continue
		}

		// Resolve relative path to absolute path
		var err error
		relp := *p // save original relative path
		if *p, err = filepath.Abs(*p); err != nil {
			return errors.Wrapf(err, "Couldn't resolve relative path %s to an absolute path", *p)
		}
		logrus.Debugf("Resolved relative path %s to %s", relp, *p)
	}
	return nil
}

func exit(err error) {
	var execErr *exec.ExitError
	if errors.As(err, &execErr) {
		// if there is an exit code propagate it
		exitWithCode(err, execErr.ExitCode())
	}
	// otherwise exit with catch all 1
	exitWithCode(err, 1)
}

// exits with the given error and exit code
func exitWithCode(err error, exitCode int) {
	teardownSandboxIfActive()
	fmt.Fprintln(os.Stderr, err)
	os.Exit(exitCode)
}

func isURL(path string) bool {
	if match, _ := regexp.MatchString("^https?://", path); match {
		return true
	}
	return false
}

func shdSkip(path string) bool {
	return path == "" || isURL(path) || filepath.IsAbs(path)
}
