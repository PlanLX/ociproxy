// Package config provides configuration for ociproxy.
package config

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Mirror is one physical endpoint for a registry namespace.
// Multiple mirrors serve the same logical namespace (e.g., docker.io).
type Mirror struct {
	// URL is the upstream base (e.g., "https://registry-1.docker.io" or "https://m.daocloud.io").
	URL string `yaml:"url"`
	// BasePath is an optional path prefix to prepend (e.g., "/docker.io").
	// When empty, the registry namespace is used as the base path.
	BasePath string `yaml:"base_path,omitempty"`
	// Default marks this mirror as the primary for its registry group.
	Default bool `yaml:"default,omitempty"`
	// Credentials for the upstream.
	Credentials *Credentials `yaml:"credentials,omitempty"`
	// Weight controls selection probability (default 1). Higher = more likely.
	Weight int `yaml:"weight,omitempty"`

	// Runtime health state (not serialized).
	mu          sync.Mutex
	failCount   int
	lastFailAt  time.Time
	healthyAt   time.Time
}

// MarkFailed records a failure for this mirror.
func (m *Mirror) MarkFailed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failCount++
	m.lastFailAt = time.Now()
}

// MarkSuccess resets failure tracking.
func (m *Mirror) MarkSuccess() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failCount = 0
	m.healthyAt = time.Now()
}

// IsHealthy returns false if the mirror has recent failures and hasn't recovered.
// A mirror with >= 3 consecutive failures within the last 60s is considered unhealthy.
func (m *Mirror) IsHealthy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failCount >= 3 && time.Since(m.lastFailAt) < 60*time.Second {
		return false
	}
	return true
}

// Credentials for upstream authentication.
type Credentials struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Registry is a group of mirrors serving the same namespace.
// E.g., "docker.io" with mirrors: dockerhub, dockerproxy.net, daocloud, etc.
type Registry struct {
	// Name is the registry namespace and the URL path prefix (e.g., "docker.io").
	Name string `yaml:"name"`
	// Mirrors are the physical endpoints for this namespace.
	Mirrors []Mirror `yaml:"mirrors"`
}

// Config is the top-level configuration.
type Config struct {
	// DefaultRegistry is the namespace used when the URL path starts without a registry prefix.
	DefaultRegistry string `yaml:"default_registry,omitempty"`
	// Registries lists all available namespace groups.
	Registries []Registry `yaml:"registries"`
	// BlockImages lists image patterns to block.
	BlockImages []string `yaml:"block_images,omitempty"`
	// Storage holds S3 storage configuration for the embedded registry cache.
	Storage *StorageConfig `yaml:"storage,omitempty"`
}

// StorageConfig holds S3 storage configuration for the embedded registry.
type StorageConfig struct {
	// Type is the storage driver type (e.g., "s3", "filesystem").
	Type string `yaml:"type"`
	// Region is the S3 region.
	Region string `yaml:"region"`
	// Bucket is the S3 bucket name.
	Bucket string `yaml:"bucket"`
	// AccessKey is the S3 access key.
	AccessKey string `yaml:"accesskey"`
	// SecretKey is the S3 secret key.
	SecretKey string `yaml:"secretkey"`
	// RootDirectory is the S3 key prefix (e.g., "/ociproxy").
	RootDirectory string `yaml:"rootdirectory"`
	// Secure enables HTTPS for S3.
	Secure bool `yaml:"secure"`
	// ForcePathStyle forces path-style S3 URLs (needed for MinIO).
	ForcePathStyle bool `yaml:"forcepathstyle"`
	// RegionEndpoint is a custom S3 endpoint (for MinIO, etc.).
	RegionEndpoint string `yaml:"regionendpoint"`
	// Encrypt enables server-side encryption.
	Encrypt bool `yaml:"encrypt"`
}

// LoadFile reads config from a YAML file, expanding ${VAR} env references.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	// Expand environment variables like ${REGISTRY_STORAGE_S3_BUCKET}
	expanded := os.ExpandEnv(string(data))
	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadFlags builds config from CLI flags.
func LoadFlags() (*Config, error) {
	var cfg Config

	flag.StringVar(&cfg.DefaultRegistry, "default-registry", "docker.io", "default registry namespace")
	registry := flag.String("registry", "docker.io=https://registry-1.docker.io", "registry namespace and default mirror: name=url")
	mirrors := flag.String("mirrors", "", "additional mirrors for the default registry, comma-separated URLs")

	flag.Parse()

	// Parse the primary registry
	name, rawURL, _ := strings.Cut(*registry, "=")
	if name == "" {
		name = cfg.DefaultRegistry
	}

	reg := Registry{
		Name: name,
		Mirrors: []Mirror{
			{URL: rawURL, Default: true},
		},
	}

	// Add additional mirrors
	if *mirrors != "" {
		for _, m := range strings.Split(*mirrors, ",") {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			reg.Mirrors = append(reg.Mirrors, Mirror{URL: m})
		}
	}

	cfg.Registries = append(cfg.Registries, reg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks the config for errors.
func (c *Config) Validate() error {
	if len(c.Registries) == 0 {
		return fmt.Errorf("at least one registry is required")
	}

	names := make(map[string]bool)
	for _, reg := range c.Registries {
		if reg.Name == "" {
			return fmt.Errorf("registry name is required")
		}
		if names[reg.Name] {
			return fmt.Errorf("duplicate registry name %q", reg.Name)
		}
		names[reg.Name] = true

		if len(reg.Mirrors) == 0 {
			return fmt.Errorf("registry %q must have at least one mirror", reg.Name)
		}

		defaults := 0
		for i := range reg.Mirrors {
			m := &reg.Mirrors[i]
			if m.URL == "" {
				return fmt.Errorf("mirror URL is required for registry %q, mirror %d", reg.Name, i)
			}
			if _, err := url.Parse(m.URL); err != nil {
				return fmt.Errorf("invalid mirror URL %q for registry %q: %w", m.URL, reg.Name, err)
			}
			if m.Default {
				defaults++
			}
		}

		if defaults == 0 {
			return fmt.Errorf("registry %q must have exactly one default mirror", reg.Name)
		}
		if defaults > 1 {
			return fmt.Errorf("registry %q has multiple default mirrors", reg.Name)
		}
	}

	return nil
}

// RegistryByName returns the registry with the given name, or nil.
func (c *Config) RegistryByName(name string) *Registry {
	for i := range c.Registries {
		if c.Registries[i].Name == name {
			return &c.Registries[i]
		}
	}
	return nil
}

// DefaultRegistry returns the registry marked as having a default mirror,
// or falls back to DefaultRegistry name.
func (c *Config) DefaultRegistryConfig() *Registry {
	// Find the registry that has a default mirror (should be exactly one across all)
	for i := range c.Registries {
		for j := range c.Registries[i].Mirrors {
			if c.Registries[i].Mirrors[j].Default {
				return &c.Registries[i]
			}
		}
	}
	// Fallback to DefaultRegistry name
	if c.DefaultRegistry != "" {
		return c.RegistryByName(c.DefaultRegistry)
	}
	return nil
}

// DefaultMirror returns the default mirror for the given registry name.
func (r *Registry) DefaultMirror() *Mirror {
	for i := range r.Mirrors {
		if r.Mirrors[i].Default {
			return &r.Mirrors[i]
		}
	}
	return nil
}

// SelectMirror picks the best available mirror using weighted selection with health filtering.
// Mirrors with >= 3 recent failures (within 60s) are skipped.
// Among healthy mirrors, selection is weighted — higher weight = more likely to be chosen.
// Returns nil if no healthy mirrors are available.
func (r *Registry) SelectMirror() *Mirror {
	var healthy []*Mirror
	for i := range r.Mirrors {
		if r.Mirrors[i].IsHealthy() {
			healthy = append(healthy, &r.Mirrors[i])
		}
	}
	if len(healthy) == 0 {
		// All unhealthy — fall back to all mirrors
		healthy = make([]*Mirror, len(r.Mirrors))
		for i := range r.Mirrors {
			healthy[i] = &r.Mirrors[i]
		}
	}

	// Weighted selection: sum weights, pick random proportional to weight
	totalWeight := 0
	for _, m := range healthy {
		w := m.Weight
		if w <= 0 {
			w = 1
		}
		totalWeight += w
	}

	if totalWeight <= 0 {
		return healthy[0]
	}

	// Simple deterministic weighted pick: use the default mirror's position
	// as the starting point, then walk weighted. For true randomness at
	// request time, use SelectMirrorWithSeed().
	return weightedPick(healthy, totalWeight, time.Now().UnixNano())
}

func weightedPick(mirrors []*Mirror, totalWeight int, seed int64) *Mirror {
	// Simple pseudo-random from seed
	pos := seed % int64(totalWeight)
	for _, m := range mirrors {
		w := int64(m.Weight)
		if w <= 0 {
			w = 1
		}
		pos -= w
		if pos < 0 {
			return m
		}
	}
	return mirrors[len(mirrors)-1]
}

// MirrorByIndex returns the mirror at the given index, or the default if out of range.
func (r *Registry) MirrorByIndex(idx int) *Mirror {
	if idx < 0 || idx >= len(r.Mirrors) {
		return r.DefaultMirror()
	}
	return &r.Mirrors[idx]
}
