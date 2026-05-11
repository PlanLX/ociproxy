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

	info := &pathInfo{
		Registry: "docker.io",
		Suffix:   "/library/busybox/manifests/latest",
	}

	mirrors := info.resolveMirrors(cfg)
	if len(mirrors) == 0 {
		t.Fatal("resolveMirrors returned no mirrors")
	}

	// Verify best mirror is the default
	best := mirrors[0].mirror
	if best.URL != "https://registry-1.docker.io" {
		t.Errorf("best mirror URL = %q; want %q", best.URL, "https://registry-1.docker.io")
	}

	// Verify target path without base_path
	gotPath := targetPath(best, info.Suffix)
	wantPath := "/v2/library/busybox/manifests/latest"
	if gotPath != wantPath {
		t.Errorf("targetPath = %q; want %q", gotPath, wantPath)
	}

	// Verify target path with base_path (mirror[1])
	gotPath2 := targetPath(mirrors[1].mirror, info.Suffix)
	wantPath2 := "/v2/docker.io/library/busybox/manifests/latest"
	if gotPath2 != wantPath2 {
		t.Errorf("targetPath with base_path = %q; want %q", gotPath2, wantPath2)
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
