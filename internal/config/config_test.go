package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFile(t *testing.T) {
	data := `
default_registry: docker.io
registries:
  - name: docker.io
    mirrors:
      - url: https://registry-1.docker.io
        default: true
      - url: https://m.daocloud.io
        base_path: /docker.io
      - url: https://docker.gh-proxy.org
        base_path: /docker.io
`
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	if cfg.DefaultRegistry != "docker.io" {
		t.Errorf("default_registry = %q; want %q", cfg.DefaultRegistry, "docker.io")
	}
	if len(cfg.Registries) != 1 {
		t.Fatalf("len(registries) = %d; want 1", len(cfg.Registries))
	}

	reg := cfg.Registries[0]
	if reg.Name != "docker.io" {
		t.Errorf("registry name = %q; want %q", reg.Name, "docker.io")
	}
	if len(reg.Mirrors) != 3 {
		t.Fatalf("len(mirrors) = %d; want 3", len(reg.Mirrors))
	}

	def := reg.DefaultMirror()
	if def == nil || def.URL != "https://registry-1.docker.io" {
		t.Errorf("default mirror URL = %v; want https://registry-1.docker.io", def)
	}

	dao := reg.MirrorByIndex(1)
	if dao == nil || dao.BasePath != "/docker.io" {
		t.Errorf("mirror[1] base_path = %q; want /docker.io", dao.BasePath)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
		errSub  string
	}{
		{
			name: "valid config",
			cfg: Config{
				Registries: []Registry{
					{
						Name: "docker.io",
						Mirrors: []Mirror{
							{URL: "https://registry-1.docker.io", Default: true},
							{URL: "https://m.daocloud.io", BasePath: "/docker.io"},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name:    "no registries",
			cfg:     Config{},
			wantErr: true,
			errSub:  "at least one registry",
		},
		{
			name: "missing name",
			cfg: Config{
				Registries: []Registry{
					{
						Name: "",
						Mirrors: []Mirror{
							{URL: "https://registry-1.docker.io", Default: true},
						},
					},
				},
			},
			wantErr: true,
			errSub:  "registry name is required",
		},
		{
			name: "no mirrors",
			cfg: Config{
				Registries: []Registry{
					{Name: "docker.io", Mirrors: []Mirror{}},
				},
			},
			wantErr: true,
			errSub:  "must have at least one mirror",
		},
		{
			name: "no default mirror",
			cfg: Config{
				Registries: []Registry{
					{
						Name: "docker.io",
						Mirrors: []Mirror{
							{URL: "https://a.com"},
							{URL: "https://b.com"},
						},
					},
				},
			},
			wantErr: true,
			errSub:  "must have exactly one default mirror",
		},
		{
			name: "multiple default mirrors",
			cfg: Config{
				Registries: []Registry{
					{
						Name: "docker.io",
						Mirrors: []Mirror{
							{URL: "https://a.com", Default: true},
							{URL: "https://b.com", Default: true},
						},
					},
				},
			},
			wantErr: true,
			errSub:  "multiple default mirrors",
		},
		{
			name: "duplicate registry names",
			cfg: Config{
				Registries: []Registry{
					{
						Name: "docker.io",
						Mirrors: []Mirror{
							{URL: "https://a.com", Default: true},
						},
					},
					{
						Name: "docker.io",
						Mirrors: []Mirror{
							{URL: "https://b.com", Default: true},
						},
					},
				},
			},
			wantErr: true,
			errSub:  "duplicate registry name",
		},
		{
			name: "missing mirror URL",
			cfg: Config{
				Registries: []Registry{
					{
						Name: "docker.io",
						Mirrors: []Mirror{
							{URL: "", Default: true},
						},
					},
				},
			},
			wantErr: true,
			errSub:  "mirror URL is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errSub != "" && !containsStr(err.Error(), tt.errSub) {
					t.Errorf("error = %q; want substring %q", err.Error(), tt.errSub)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestRegistryByName(t *testing.T) {
	cfg := &Config{
		Registries: []Registry{
			{
				Name: "docker.io",
				Mirrors: []Mirror{
					{URL: "https://registry-1.docker.io", Default: true},
				},
			},
			{
				Name: "ghcr.io",
				Mirrors: []Mirror{
					{URL: "https://ghcr.io", Default: true},
				},
			},
		},
	}

	reg := cfg.RegistryByName("docker.io")
	if reg == nil {
		t.Fatal("expected registry, got nil")
	}
	if len(reg.Mirrors) != 1 {
		t.Errorf("len(mirrors) = %d; want 1", len(reg.Mirrors))
	}

	if cfg.RegistryByName("unknown") != nil {
		t.Error("expected nil for unknown registry")
	}
}

func TestDefaultRegistryConfig(t *testing.T) {
	cfg := &Config{
		DefaultRegistry: "docker.io",
		Registries: []Registry{
			{
				Name: "docker.io",
				Mirrors: []Mirror{
					{URL: "https://a.com", Default: true},
					{URL: "https://b.com"},
				},
			},
		},
	}

	def := cfg.DefaultRegistryConfig()
	if def == nil || def.Name != "docker.io" {
		t.Errorf("default registry = %v; want docker.io", def)
	}
}

func TestDefaultMirror(t *testing.T) {
	reg := &Registry{
		Name: "docker.io",
		Mirrors: []Mirror{
			{URL: "https://a.com"},
			{URL: "https://b.com", Default: true},
			{URL: "https://c.com", BasePath: "/docker.io"},
		},
	}

	def := reg.DefaultMirror()
	if def == nil || def.URL != "https://b.com" {
		t.Errorf("default mirror = %v; want https://b.com", def)
	}
}

func TestMirrorByIndex(t *testing.T) {
	reg := &Registry{
		Name: "docker.io",
		Mirrors: []Mirror{
			{URL: "https://a.com", Default: true},
			{URL: "https://b.com", BasePath: "/docker.io"},
		},
	}

	// Valid index
	m := reg.MirrorByIndex(1)
	if m == nil || m.URL != "https://b.com" {
		t.Errorf("mirror[1] = %v; want https://b.com", m)
	}

	// Out of range → falls back to default
	m2 := reg.MirrorByIndex(99)
	if m2 == nil || m2.URL != "https://a.com" {
		t.Errorf("mirror[99] = %v; want default https://a.com", m2)
	}

	// Negative → falls back to default
	m3 := reg.MirrorByIndex(-1)
	if m3 == nil || m3.URL != "https://a.com" {
		t.Errorf("mirror[-1] = %v; want default https://a.com", m3)
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || findSub(s, sub))
}

func findSub(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
