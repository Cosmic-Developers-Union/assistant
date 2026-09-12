package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvSearchesParentDirectories(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "one", "two")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	dotEnvPath := filepath.Join(root, ".env")
	if err := os.WriteFile(dotEnvPath, []byte("GITEA_HOST=https://gitea.example.com\nGITEA_ACCESS_TOKEN=from-file\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	unsetEnvironmentVariable(t, "GITEA_HOST")
	unsetEnvironmentVariable(t, "GITEA_ACCESS_TOKEN")

	loadedPath, err := LoadDotEnv(nested)
	if err != nil {
		t.Fatalf("LoadDotEnv() error = %v", err)
	}
	if loadedPath != dotEnvPath {
		t.Errorf("LoadDotEnv() path = %q, want %q", loadedPath, dotEnvPath)
	}
	if got := os.Getenv("GITEA_HOST"); got != "https://gitea.example.com" {
		t.Errorf("GITEA_HOST = %q", got)
	}
	if got := os.Getenv("GITEA_ACCESS_TOKEN"); got != "from-file" {
		t.Errorf("GITEA_ACCESS_TOKEN = %q", got)
	}
}

func TestLoadDotEnvDoesNotOverrideProcessEnvironment(t *testing.T) {
	directory := t.TempDir()
	dotEnvPath := filepath.Join(directory, ".env")
	if err := os.WriteFile(dotEnvPath, []byte("GITEA_HOST=https://file.example.com\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("GITEA_HOST", "https://environment.example.com")

	if _, err := LoadDotEnv(directory); err != nil {
		t.Fatalf("LoadDotEnv() error = %v", err)
	}
	if got := os.Getenv("GITEA_HOST"); got != "https://environment.example.com" {
		t.Errorf("GITEA_HOST = %q", got)
	}
}

func TestLoadDotEnvAllowsMissingFile(t *testing.T) {
	directory := t.TempDir()
	loadedPath, err := LoadDotEnv(directory)
	if err != nil {
		t.Fatalf("LoadDotEnv() error = %v", err)
	}
	if loadedPath != "" {
		t.Errorf("LoadDotEnv() path = %q", loadedPath)
	}
}

func unsetEnvironmentVariable(t *testing.T, name string) {
	t.Helper()
	value, exists := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("Unsetenv(%q) error = %v", name, err)
	}
	t.Cleanup(func() {
		if exists {
			if err := os.Setenv(name, value); err != nil {
				t.Errorf("Setenv(%q) error = %v", name, err)
			}
			return
		}
		if err := os.Unsetenv(name); err != nil {
			t.Errorf("Unsetenv(%q) error = %v", name, err)
		}
	})
}
