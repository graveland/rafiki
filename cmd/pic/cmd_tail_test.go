package main

import (
	"testing"
)

func TestIsChildExited(t *testing.T) {
	tests := []struct {
		name    string
		frame   string
		childID string
		want    bool
	}{
		{"matching child", `{"type":"ctrl_child_exited","childId":"c_01HX"}`, "c_01HX", true},
		{"other child", `{"type":"ctrl_child_exited","childId":"c_other"}`, "c_01HX", false},
		{"not exited", `{"type":"ctrl_event","childId":"c_01HX"}`, "c_01HX", false},
		{"malformed", `not json`, "c_01HX", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isChildExited([]byte(tt.frame), tt.childID)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
