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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestProotAutoDownloadDisabled(t *testing.T) {
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

// serveScript returns a test server that hands out an executable shell script
// standing in for the proot binary, so downloadProot's `--version` sanity check
// passes.
func serveScript(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "#!/bin/sh\necho 'proot stub 5.0'\n")
	}))
}

func TestDownloadProotSuccess(t *testing.T) {
	srv := serveScript(t)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "proot")
	if err := downloadProot(srv.URL, dest); err != nil {
		t.Fatalf("downloadProot: %v", err)
	}
	if !isExecutableFile(dest) {
		t.Fatalf("destination %s is not an executable file", dest)
	}
}

func TestDownloadProotHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "proot")
	if err := downloadProot(srv.URL, dest); err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("destination must not be created on failure, stat err = %v", err)
	}
}

func TestDownloadProotSHA256Mismatch(t *testing.T) {
	srv := serveScript(t)
	defer srv.Close()

	t.Setenv(prootSHA256Env, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	dest := filepath.Join(t.TempDir(), "proot")
	if err := downloadProot(srv.URL, dest); err == nil {
		t.Fatal("expected a checksum mismatch error")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("destination must not be created on checksum mismatch, stat err = %v", err)
	}
}
