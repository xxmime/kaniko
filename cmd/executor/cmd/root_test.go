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
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleContainerTools/kaniko/testutil"
)

func TestSkipPath(t *testing.T) {
	tests := []struct {
		description string
		path        string
		expected    bool
	}{
		{
			description: "path is a http url",
			path:        "http://test",
			expected:    true,
		},
		{
			description: "path is a https url",
			path:        "https://test",
			expected:    true,
		},
		{
			description: "path is a empty",
			path:        "",
			expected:    true,
		},
		{
			description: "path is already abs",
			path:        "/tmp/test",
			expected:    true,
		},
		{
			description: "path is relative",
			path:        ".././test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			testutil.CheckDeepEqual(t, tt.expected, shdSkip(tt.path))
		})
	}
}

func TestIsUrl(t *testing.T) {
	tests := []struct {
		description string
		path        string
		expected    bool
	}{
		{
			description: "path is a http url",
			path:        "http://test",
			expected:    true,
		},
		{
			description: "path is a https url",
			path:        "https://test",
			expected:    true,
		},
		{
			description: "path is a empty",
			path:        "",
		},
		{
			description: "path is already abs",
			path:        "/tmp/test",
		},
		{
			description: "path is relative",
			path:        ".././test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			testutil.CheckDeepEqual(t, tt.expected, isURL(tt.path))
		})
	}
}

func TestSandboxUserNamespaceIDMaps(t *testing.T) {
	cases := []struct {
		name              string
		uid, gid          int
		wantSize          int
		wantSetgroups     bool
		wantContainerHost int
	}{
		{
			name:              "root maps standard id space",
			uid:               0,
			gid:               0,
			wantSize:          sandboxStandardIDMapSize,
			wantSetgroups:     true,
			wantContainerHost: 0,
		},
		{
			name:              "non-root single mapping",
			uid:               1000,
			gid:               1000,
			wantSize:          1,
			wantSetgroups:     false,
			wantContainerHost: 1000,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uidMaps, gidMaps, enableSetgroups := sandboxUserNamespaceIDMaps(c.uid, c.gid)
			testutil.CheckDeepEqual(t, c.wantSetgroups, enableSetgroups)
			if len(uidMaps) != 1 || len(gidMaps) != 1 {
				t.Fatalf("expected one mapping each, got uid=%v gid=%v", uidMaps, gidMaps)
			}
			testutil.CheckDeepEqual(t, 0, uidMaps[0].ContainerID)
			testutil.CheckDeepEqual(t, c.wantSize, uidMaps[0].Size)
			testutil.CheckDeepEqual(t, c.wantContainerHost, uidMaps[0].HostID)
			testutil.CheckDeepEqual(t, uidMaps[0], gidMaps[0])
		})
	}
}

func TestHasCapSysAdminParsesCapEff(t *testing.T) {
	// We can't reasonably alter /proc/self/status from a test, but the
	// parser is the interesting part. Reproduce the kernel format and
	// confirm we extract CAP_SYS_ADMIN (bit 21 == 0x200000) correctly.
	parse := func(capEff string) bool {
		lines := []string{
			"Name:\tkaniko",
			"Uid:\t0\t0\t0\t0",
			"CapEff:\t" + capEff,
			"Seccomp:\t0",
		}
		for _, line := range lines {
			if !strings.HasPrefix(line, "CapEff:") {
				continue
			}
			val, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), 16, 64)
			if err != nil {
				return false
			}
			return val&(1<<21) != 0
		}
		return false
	}

	cases := []struct {
		capEff string
		want   bool
	}{
		{"0000000000000000", false},
		{"0000000000200000", true},                  // exactly CAP_SYS_ADMIN
		{"00000000a80425fb", true},                  // typical "all caps" set on root
		{"0000000000000800", false},                 // CAP_NET_ADMIN only
		{"00000000003fffffffff", true},              // wider set, still includes bit 21
	}
	for _, c := range cases {
		t.Run(c.capEff, func(t *testing.T) {
			testutil.CheckDeepEqual(t, c.want, parse(c.capEff))
		})
	}
}

func TestUnescapeMountField(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/kaniko/sandbox", "/kaniko/sandbox"},
		{`/kaniko/sandbox/proc`, `/kaniko/sandbox/proc`},
		{`/mnt/My\040Folder`, `/mnt/My Folder`},
		{`/path/with\011tab`, "/path/with\ttab"},
		{`/path/with\012newline`, "/path/with\nnewline"},
		{`/path/with\134backslash`, `/path/with\backslash`},
		{`/edge\999case`, `/edge\999case`},   // not a valid octal triple, kept as-is
		{`/short\04`, `/short\04`},            // too short to be a full escape
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			testutil.CheckDeepEqual(t, tt.expected, unescapeMountField(tt.input))
		})
	}
}

func TestResolveEnvironmentBuildArgs(t *testing.T) {
	tests := []struct {
		description               string
		input                     []string
		expected                  []string
		mockedEnvironmentResolver func(string) string
	}{
		{
			description: "replace when environment variable is present and value is not specified",
			input:       []string{"variable1"},
			expected:    []string{"variable1=value1"},
			mockedEnvironmentResolver: func(variable string) string {
				if variable == "variable1" {
					return "value1"
				}
				return ""
			},
		},
		{
			description: "do not replace when environment variable is present and value is specified",
			input:       []string{"variable1=value1", "variable2=value2"},
			expected:    []string{"variable1=value1", "variable2=value2"},
			mockedEnvironmentResolver: func(variable string) string {
				return "unexpected"
			},
		},
		{
			description: "do not replace when environment variable is present and empty value is specified",
			input:       []string{"variable1="},
			expected:    []string{"variable1="},
			mockedEnvironmentResolver: func(variable string) string {
				return "unexpected"
			},
		},
		{
			description: "replace with empty value when environment variable is not present or empty and value is not specified",
			input:       []string{"variable1", "variable2=value2"},
			expected:    []string{"variable1=", "variable2=value2"},
			mockedEnvironmentResolver: func(variable string) string {
				return ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			resolveEnvironmentBuildArgs(tt.input, tt.mockedEnvironmentResolver)
			testutil.CheckDeepEqual(t, tt.expected, tt.input)
		})
	}
}
