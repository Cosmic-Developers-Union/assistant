package config

import "testing"

func TestLoad(t *testing.T) {
	environment := map[string]string{
		"GITEA_HOST":                    " https://gitea.example.com/ ",
		"GITEA_ACCESS_TOKEN":            " secret ",
		"GITEA_BRANCH_PROTECTION_TOKEN": " admin-secret ",
	}

	got, err := Load(func(key string) string { return environment[key] })
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Host != "https://gitea.example.com" {
		t.Errorf("Host = %q", got.Host)
	}
	if got.AccessToken != "secret" {
		t.Errorf("AccessToken = %q", got.AccessToken)
	}
	if got.BranchProtectionToken != "admin-secret" {
		t.Errorf("BranchProtectionToken = %q", got.BranchProtectionToken)
	}
}

func TestLoadBranchProtectionTokenOptional(t *testing.T) {
	environment := map[string]string{
		"GITEA_HOST":         "https://gitea.example.com",
		"GITEA_ACCESS_TOKEN": "secret",
	}

	got, err := Load(func(key string) string { return environment[key] })
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.BranchProtectionToken != "" {
		t.Errorf("BranchProtectionToken = %q, want empty", got.BranchProtectionToken)
	}
}

func TestLoadRejectsInvalidEnvironment(t *testing.T) {
	tests := []struct {
		name        string
		environment map[string]string
	}{
		{name: "missing host", environment: map[string]string{"GITEA_ACCESS_TOKEN": "secret"}},
		{name: "invalid host", environment: map[string]string{"GITEA_HOST": "gitea.example.com", "GITEA_ACCESS_TOKEN": "secret"}},
		{name: "host with query", environment: map[string]string{"GITEA_HOST": "https://gitea.example.com?debug=1", "GITEA_ACCESS_TOKEN": "secret"}},
		{name: "missing token", environment: map[string]string{"GITEA_HOST": "https://gitea.example.com"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(func(key string) string { return test.environment[key] })
			if err == nil {
				t.Fatal("Load() error = nil")
			}
		})
	}
}
