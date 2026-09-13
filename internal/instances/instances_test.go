package instances

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadNormalizesDefaultsAndRepoShorthand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
  "instances": [
    {
      "host": "https://gitea.example.com/",
      "admin_token": "admin",
      "repos": ["owner/repo", {"name": "owner/another", "dir": "/srv/another"}]
    }
  ]
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	instance := file.Instances[0]
	if instance.Host != "https://gitea.example.com" {
		t.Errorf("Host = %q, want trailing slash trimmed", instance.Host)
	}
	if instance.Reviewer.Name != DefaultReviewerName || instance.Merger.Name != DefaultMergerName {
		t.Errorf("account defaults = %+v/%+v", instance.Reviewer, instance.Merger)
	}
	if len(instance.Repos) != 2 {
		t.Fatalf("Repos = %+v", instance.Repos)
	}
	if instance.Repos[0].Name != "owner/repo" || instance.Repos[0].Dir != "" {
		t.Errorf("Repos[0] = %+v", instance.Repos[0])
	}
	if instance.Repos[1].Name != "owner/another" || instance.Repos[1].Dir != "/srv/another" {
		t.Errorf("Repos[1] = %+v", instance.Repos[1])
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	tests := map[string]string{
		"empty instances": `{"instances": []}`,
		"bad host":        `{"instances": [{"host": "gitea.example.com", "repos": []}]}`,
		"duplicate host": `{"instances": [
			{"host": "https://a.example.com", "repos": []},
			{"host": "https://a.example.com", "repos": []}
		]}`,
		"bad repository": `{"instances": [{"host": "https://a.example.com", "repos": ["nope"]}]}`,
		"same account": `{"instances": [{"host": "https://a.example.com", "repos": [],
			"reviewer": {"name": "bot"}, "merger": {"name": "bot"}}]}`,
		"oauth without refresh": `{"instances": [{"host": "https://a.example.com", "repos": [],
			"admin_oauth": {"client_id": "c1"}}]}`,
		"unknown field": `{"instances": [{"host": "https://a.example.com", "repos": [], "token": "x"}]}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Errorf("Load(%s) error = nil, want error", content)
			}
		})
	}
}

func TestSaveWritesRestrictedFileAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	file := &File{Instances: []Instance{{
		Host:       "https://gitea.example.com",
		AdminToken: "admin",
		AdminOAuth: &OAuthCredential{ClientID: "client-1", ClientSecret: "secret-1", RefreshToken: "refresh-1"},
		Reviewer:   Account{Name: "ai", Token: "reviewer-token"},
		Merger:     Account{Name: "merge", Token: "merger-token"},
		Repos:      []Repo{{Name: "owner/repo", MergerToken: "repo-merger-token"}, {Name: "owner/another", Dir: "/srv/another"}},
	}}}
	if err := Save(path, file); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if permission := info.Mode().Perm(); permission != 0o600 {
		t.Errorf("permissions = %o, want 600", permission)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Instances[0].Reviewer.Token != "reviewer-token" {
		t.Errorf("token round-trip failed: %+v", loaded.Instances[0].Reviewer)
	}
	if loaded.Instances[0].AdminOAuth == nil || loaded.Instances[0].AdminOAuth.RefreshToken != "refresh-1" {
		t.Errorf("admin_oauth round-trip failed: %+v", loaded.Instances[0].AdminOAuth)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"repo-merger-token"`) || !strings.Contains(string(raw), `"merger_token"`) {
		t.Errorf("repo merger token should be persisted:\n%s", raw)
	}
}

func TestRepoJSONShapes(t *testing.T) {
	var shorthand Repo
	if err := json.Unmarshal([]byte(`"owner/repo"`), &shorthand); err != nil {
		t.Fatal(err)
	}
	if shorthand.Name != "owner/repo" || shorthand.Dir != "" {
		t.Errorf("shorthand = %+v", shorthand)
	}
	encoded, err := json.Marshal(shorthand)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `"owner/repo"` {
		t.Errorf("encoded = %s", encoded)
	}
	var object Repo
	if err := json.Unmarshal([]byte(`{"name":"owner/repo","dir":"/srv/repo","merger_token":"tok-1"}`), &object); err != nil {
		t.Fatal(err)
	}
	if object.Dir != "/srv/repo" || object.MergerToken != "tok-1" {
		t.Errorf("object = %+v", object)
	}
	// 有 dir/merger_token 时序列化为对象（不丢字段）
	encoded, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"merger_token":"tok-1"`) {
		t.Errorf("encoded = %s", encoded)
	}
}
