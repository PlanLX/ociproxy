//go:build ignore
// +build ignore

// COMMENTED OUT: tests for legacy authTransport, replaced by go-containerregistry (2026-05-26).
// Kept for reference.
package proxy

import (
	"testing"

	"github.com/planlx/ociproxy/internal/config"
)

func TestParseAuthHeader(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		want    authChallenge
	}{
		{
			name:   "docker hub",
			header: `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/busybox:pull"`,
			want: authChallenge{
				realm:   "https://auth.docker.io/token",
				service: "registry.docker.io",
				scope:   "repository:library/busybox:pull",
			},
		},
		{
			name:   "no scope",
			header: `Bearer realm="https://auth.example.com/token",service="registry.example.com"`,
			want: authChallenge{
				realm:   "https://auth.example.com/token",
				service: "registry.example.com",
				scope:   "",
			},
		},
		{
			name:   "not bearer",
			header: `Basic realm="secure"`,
			want:   authChallenge{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseAuthHeader(tt.header)
			if got.realm != tt.want.realm {
				t.Errorf("realm = %q; want %q", got.realm, tt.want.realm)
			}
			if got.service != tt.want.service {
				t.Errorf("service = %q; want %q", got.service, tt.want.service)
			}
			if got.scope != tt.want.scope {
				t.Errorf("scope = %q; want %q", got.scope, tt.want.scope)
			}
		})
	}
}

func TestIsHostname(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"docker.io", true},
		{"registry-1.docker.io", true},
		{"dockerhub", false},
		{"daocloud", false},
		{"localhost", false},
		{"library", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isHostname(tt.input)
			if got != tt.want {
				t.Errorf("isHostname(%q) = %v; want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolvePath(t *testing.T) {
	cfg := &config.Config{
		DefaultRegistry: "docker.io",
		Registries: []config.Registry{
			{
				Name: "docker.io",
				Mirrors: []config.Mirror{
					{URL: "https://registry-1.docker.io", Default: true, Weight: 20},
					{URL: "https://m.daocloud.io", BasePath: "/docker.io", Weight: 10},
				},
			},
		},
	}

	info := &pathInfo{
		Registry: "docker.io",
		Suffix:   "/library/busybox/manifests/latest",
	}

	mirrors := info.resolveMirrors(cfg)
	if len(mirrors) != 2 {
		t.Fatalf("expected 2 mirrors, got %d", len(mirrors))
	}

	// Check each mirror's target path regardless of ordering
	for _, mt := range mirrors {
		gotPath := targetPath(mt.mirror, info.Suffix)
		switch mt.mirror.URL {
		case "https://registry-1.docker.io":
			wantPath := "/v2/library/busybox/manifests/latest"
			if gotPath != wantPath {
				t.Errorf("registry-1.docker.io: targetPath = %q; want %q", gotPath, wantPath)
			}
		case "https://m.daocloud.io":
			wantPath := "/v2/docker.io/library/busybox/manifests/latest"
			if gotPath != wantPath {
				t.Errorf("m.daocloud.io: targetPath = %q; want %q", gotPath, wantPath)
			}
		}
	}
}

func TestTrimTrailingSlash(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"https://example.com/", "https://example.com"},
		{"https://example.com", "https://example.com"},
		{"/", "/"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := trimTrailingSlash(tt.input)
			if got != tt.want {
				t.Errorf("got %q; want %q", got, tt.want)
			}
		})
	}
}
