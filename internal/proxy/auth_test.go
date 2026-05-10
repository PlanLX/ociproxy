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
					{URL: "https://registry-1.docker.io", Default: true},
					{URL: "https://m.daocloud.io", BasePath: "/docker.io"},
				},
			},
		},
	}

	tests := []struct {
		name        string
		registry    string
		suffix      string
		wantURL     string
		wantPath    string
	}{
		{
			name:     "default mirror",
			registry: "docker.io",
			suffix:   "/library/busybox/manifests/latest",
			wantURL:  "https://registry-1.docker.io",
			wantPath: "/docker.io/library/busybox/manifests/latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &pathInfo{
				Registry: tt.registry,
				Suffix:   tt.suffix,
			}
			gotURL, gotPath := info.Resolve(cfg)
			if gotURL != tt.wantURL {
				t.Errorf("URL = %q; want %q", gotURL, tt.wantURL)
			}
			if gotPath != tt.wantPath {
				t.Errorf("path = %q; want %q", gotPath, tt.wantPath)
			}
		})
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
