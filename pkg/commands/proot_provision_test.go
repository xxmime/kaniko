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
	"os"
	"path/filepath"
	"testing"
)

func TestProotEmbeddedDisabled(t *testing.T) {
	for _, v := range []string{"skip", "0", "false", "NO", "Off", " disable "} {
		if !disabled(v) {
			t.Errorf("disabled(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "1", "true", "yes", "auto"} {
		if disabled(v) {
			t.Errorf("disabled(%q) = true, want false", v)
		}
	}
}

func TestWriteProotSuccess(t *testing.T) {
	// A shell script stands in for the proot binary so the `--version` sanity
	// check passes.
	data := []byte("#!/bin/sh\necho 'proot stub 5.0'\n")
	dest := filepath.Join(t.TempDir(), "proot")
	if err := writeProot(data, dest); err != nil {
		t.Fatalf("writeProot: %v", err)
	}
	if !isExecutableFile(dest) {
		t.Fatalf("destination %s is not an executable file", dest)
	}
}

func TestWriteProotVerifyFailure(t *testing.T) {
	// Not a runnable program: exec fails, so writeProot must report an error
	// and must not publish the destination file.
	data := []byte("this is not an executable\n")
	dest := filepath.Join(t.TempDir(), "proot")
	if err := writeProot(data, dest); err == nil {
		t.Fatal("expected an error for a non-executable payload")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("destination must not be created on verify failure, stat err = %v", err)
	}
}
