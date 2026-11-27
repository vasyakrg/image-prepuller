package agent

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsRegistryAuthError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{
			name: "containerd anonymous token",
			err:  errors.New("failed to fetch anonymous token: unexpected status from GET request to https://hub.example/jwt/auth?scope=repository%3Atools%2Fapp%3Apull: 403 Forbidden"),
			want: true,
		},
		{
			name: "cri-o authentication required",
			err:  errors.New("pulling image: rpc error: code = Unknown desc = authentication required"),
			want: true,
		},
		{
			name: "docker no basic auth",
			err:  errors.New("no basic auth credentials"),
			want: true,
		},
		{
			name: "uppercase unauthorized",
			err:  errors.New("Error response from daemon: 401 Unauthorized"),
			want: true,
		},
		{
			name: "failed to authorize",
			err:  errors.New("failed to authorize: failed to fetch oauth token"),
			want: true,
		},
		{name: "disk full", err: errors.New("no space left on device"), want: false},
		{name: "manifest unknown", err: errors.New("manifest unknown"), want: false},
		// A digest that happens to contain 401 must not be classified as auth.
		{name: "digest with 401", err: fmt.Errorf("unexpected digest sha256:401abcdef"), want: false},
		{name: "timeout", err: errors.New("context deadline exceeded"), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRegistryAuthError(tc.err); got != tc.want {
				t.Fatalf("IsRegistryAuthError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRegistryAuthEmpty(t *testing.T) {
	var nilAuth *RegistryAuth
	if !nilAuth.Empty() {
		t.Fatal("nil auth is not empty")
	}
	if !(&RegistryAuth{}).Empty() {
		t.Fatal("zero auth is not empty")
	}
	if (&RegistryAuth{Username: "u"}).Empty() {
		t.Fatal("username is ignored")
	}
	if (&RegistryAuth{Auth: "dTpw"}).Empty() {
		t.Fatal("auth blob is ignored")
	}
}
