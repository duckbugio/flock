package teambranch_test

import (
	"testing"

	"github.com/duckbugio/flock/core/teambranch"
)

func TestChatID(t *testing.T) {
	cases := []struct {
		ref  string
		want string
		ok   bool
	}{
		{"duck/100/feature-x", "100", true},
		{"duck/-5164159101/go-stage-6", "-5164159101", true}, // negative group ids are real
		{"duck/100", "100", true},                            // slug is optional
		{"duck//slug", "", false},                            // empty chat id routes nowhere
		{"duck/", "", false},
		{"main", "", false},
		{"ducks/100/x", "", false}, // prefix is exact
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := teambranch.ChatID(tc.ref)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ChatID(%q) = %q, %v; want %q, %v", tc.ref, got, ok, tc.want, tc.ok)
		}
	}
}
