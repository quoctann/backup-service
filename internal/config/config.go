// Package config loads a deliberately small, strict configuration schema.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var safeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

type Config struct {
	Storage Storage  `yaml:"storage"`
	Targets []Target `yaml:"targets"`
}

type Storage struct {
	Root           string `yaml:"root"`
	ExpectedDiskID string `yaml:"expectedDiskID"`
	KeepSuccessful int    `yaml:"keepSuccessful"`
}

type Target struct {
	Name         string        `yaml:"name"`
	Host         string        `yaml:"host"`
	Port         int           `yaml:"port"`
	MajorVersion int           `yaml:"majorVersion"`
	Username     string        `yaml:"username"`
	Databases    []string      `yaml:"databases"`
	PasswordFile string        `yaml:"passwordFile"`
	SSLMode      string        `yaml:"sslMode"`
	Timeout      time.Duration `yaml:"timeout"`
}

type rawTarget struct {
	Name         string         `yaml:"name"`
	Host         string         `yaml:"host"`
	Port         *int           `yaml:"port"`
	MajorVersion int            `yaml:"majorVersion"`
	Username     string         `yaml:"username"`
	Databases    []string       `yaml:"databases"`
	PasswordFile string         `yaml:"passwordFile"`
	SSLMode      *string        `yaml:"sslMode"`
	Timeout      *time.Duration `yaml:"timeout"`
}

type rawStorage struct {
	Storage Storage     `yaml:"storage"`
	Targets []rawTarget `yaml:"targets"`
}

func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, errors.New("cannot open configuration")
	}
	defer f.Close()

	return Parse(f)
}

func Parse(r io.Reader) (Config, error) {
	raw := rawStorage{
		Storage: Storage{KeepSuccessful: 7},
	}

	decoder := yaml.NewDecoder(io.LimitReader(r, 1024*1024+1)) // ~1MB
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, errors.New("invalid configuration YAML (unknown, duplicate or malformed field)")
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("configuration must contain exactly one YAML document")
	}

	cfg := Config{Storage: raw.Storage}
	for _, raw := range raw.Targets {
		target := Target{
			Name:         raw.Name,
			Host:         raw.Host,
			Port:         5432,
			MajorVersion: raw.MajorVersion,
			Username:     raw.Username,
			Databases:    raw.Databases,
			PasswordFile: raw.PasswordFile,
			SSLMode:      "verify-full",
			Timeout:      time.Hour,
		}
		if raw.Port != nil {
			target.Port = *raw.Port
		}
		if raw.SSLMode != nil {
			target.SSLMode = *raw.SSLMode
		}
		if raw.Timeout != nil {
			target.Timeout = *raw.Timeout
		}
		cfg.Targets = append(cfg.Targets, target)
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	if !filepath.IsAbs(c.Storage.Root) || filepath.Clean(c.Storage.Root) != c.Storage.Root || c.Storage.Root == "/" {
		return errors.New("storage.root must be a clean absolute directory other than /")
	}
	if !safeName.MatchString(c.Storage.ExpectedDiskID) {
		return errors.New("storage.expectedDiskID must be a safe nonempty disk identifier")
	}
	if c.Storage.KeepSuccessful < 1 || c.Storage.KeepSuccessful > 10000 {
		return errors.New("storage.keepSuccessful must be between 1 and 10000")
	}
	if len(c.Targets) == 0 {
		return errors.New("at least one target is required")
	}

	names := make(map[string]bool)
	for i, t := range c.Targets {
		if err := t.validate(); err != nil {
			return fmt.Errorf("target %d: %w", i+1, err)
		}
		if names[t.Name] {
			return errors.New("duplicate target name")
		}
		names[t.Name] = true
	}

	return nil
}

func (t Target) validate() error {
	if !safeName.MatchString(t.Name) {
		return errors.New("name must match [a-z0-9][a-z0-9_-]{0,62}")
	}
	if !text(t.Host) || strings.ContainsAny(t.Host, " /\\=@") {
		return errors.New("host must be a DNS name or IP, not a connection string or socket path")
	}
	if !text(t.Username) {
		return errors.New("username is required and must not contain control characters")
	}
	if t.Port < 1 || t.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if t.MajorVersion != 14 && t.MajorVersion != 16 && t.MajorVersion != 17 {
		return errors.New("majorVersion must be 14, 16 or 17")
	}
	if !filepath.IsAbs(t.PasswordFile) || strings.ContainsAny(t.PasswordFile, "\x00\r\n") {
		return errors.New("passwordFile must be an absolute path")
	}
	if t.SSLMode != "disable" && t.SSLMode != "allow" && t.SSLMode != "prefer" && t.SSLMode != "require" && t.SSLMode != "verify-ca" && t.SSLMode != "verify-full" {
		return errors.New("sslMode must be disable, allow, prefer, require, verify-ca or verify-full")
	}
	if t.Timeout <= 0 || t.Timeout > 24*time.Hour {
		return errors.New("timeout must be greater than zero and at most 24h")
	}
	if len(t.Databases) == 0 {
		return errors.New("at least one explicit database is required")
	}

	seen := make(map[string]bool)
	for _, db := range t.Databases {
		// libpq treats dbname containing '=' or a URI prefix as a connection
		// string, even when passed as a single argv element. Disallow that form.
		if !text(db) || strings.Contains(db, "=") || strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://") {
			return errors.New("database must be a literal nonempty database name")
		}
		if seen[db] {
			return errors.New("duplicate database")
		}
		seen[db] = true
	}

	return nil
}

func text(s string) bool {
	return strings.TrimSpace(s) != "" && !strings.ContainsAny(s, "\x00\r\n\t") && len(s) <= 1024
}
