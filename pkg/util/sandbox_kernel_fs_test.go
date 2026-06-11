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
	"os"
	"path/filepath"
	"testing"
)

func TestSetupSandboxStubKernelFilesystems(t *testing.T) {
	root := t.TempDir()
	SandboxProcSelfStub = false
	t.Cleanup(func() { SandboxProcSelfStub = false })

	if err := SetupSandboxStubKernelFilesystems(root, true, true, true); err != nil {
		t.Fatal(err)
	}
	if !SandboxProcSelfStub {
		t.Fatal("expected SandboxProcSelfStub")
	}
	for _, path := range []string{
		filepath.Join(root, "proc", "self"),
		filepath.Join(root, "proc", "cpuinfo"),
		filepath.Join(root, "sys"),
		filepath.Join(root, "dev", "null"),
		filepath.Join(root, "dev", "urandom"),
		filepath.Join(root, "dev", "stdin"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
}

func TestUpdateSandboxProcSelfExe(t *testing.T) {
	root := t.TempDir()
	SandboxProcSelfStub = true
	t.Cleanup(func() { SandboxProcSelfStub = false })

	if err := SetupSandboxStubKernelFilesystems(root, true, false, false); err != nil {
		t.Fatal(err)
	}
	if err := UpdateSandboxProcSelfExe(root, "/usr/bin/zig"); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(filepath.Join(root, "proc", "self", "exe"))
	if err != nil {
		t.Fatal(err)
	}
	if link != "/usr/bin/zig" {
		t.Fatalf("got link %q, want /usr/bin/zig", link)
	}
}
