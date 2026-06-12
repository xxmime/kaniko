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

package proot

import "testing"

func TestIsELF(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"elf", []byte{0x7f, 'E', 'L', 'F', 0x02}, true},
		{"placeholder text", []byte("kaniko embedded-proot placeholder"), false},
		{"too short", []byte{0x7f, 'E'}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isELF(c.in); got != c.want {
				t.Errorf("isELF(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestBinaryPlaceholder verifies that the committed placeholder asset is treated
// as "no embedded proot". Release builds replace the asset with a real binary.
func TestBinaryPlaceholder(t *testing.T) {
	if Binary() != nil {
		t.Error("expected Binary() to be nil for the committed placeholder asset")
	}
	if Available() {
		t.Error("expected Available() to be false for the committed placeholder asset")
	}
}
