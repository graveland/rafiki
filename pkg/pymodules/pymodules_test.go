// SPDX-License-Identifier: Apache-2.0

package pymodules

import (
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "normal name", input: "chart_helpers", wantErr: false},
		{name: "empty string", input: "", wantErr: true},
		{name: "starts with digit", input: "1foo", wantErr: true},
		{name: "contains slash", input: "foo/bar", wantErr: true},
		{name: "contains dotdot", input: "../etc", wantErr: true},
		{name: "contains hyphen", input: "my-module", wantErr: true},
		{
			name:    "65 characters",
			input:   strings.Repeat("_", 65),
			wantErr: true,
		},
		{
			name:    "64 characters all underscore",
			input:   strings.Repeat("_", 64),
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidName(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ValidName(%q) = nil, want error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidName(%q) = %v, want nil", tt.input, err)
			}
		})
	}
}
