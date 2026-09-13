package dispatcher

import "testing"

func TestParseGitRemoteURLHTTPForm(t *testing.T) {
	tests := []struct {
		input    string
		wantHost string
		wantRepo string
	}{
		{
			"http://gitea.example.com:63860/owner/repo.git",
			"http://gitea.example.com:63860",
			"owner/repo",
		},
		{
			"https://gitea.example.com/owner/repo",
			"https://gitea.example.com",
			"owner/repo",
		},
		{
			"http://gitea.example.com:8418/org/team/repo.git/",
			"http://gitea.example.com:8418",
			"org/team/repo",
		},
	}
	for _, test := range tests {
		remote, ok := ParseGitRemoteURL(test.input)
		if !ok {
			t.Errorf("ParseGitRemoteURL(%q) = _, false; want true", test.input)
			continue
		}
		if remote.Host != test.wantHost || remote.Repository != test.wantRepo {
			t.Errorf("ParseGitRemoteURL(%q) = %+v, want %s %s", test.input, remote, test.wantHost, test.wantRepo)
		}
	}
}

func TestParseGitRemoteURLSSHForm(t *testing.T) {
	tests := []struct {
		input    string
		wantHost string
		wantRepo string
	}{
		{"ssh://git@gitea.example.com:2222/owner/repo.git", "http://gitea.example.com:2222", "owner/repo"},
		{"git://git.example.com/owner/repo.git", "http://git.example.com", "owner/repo"},
	}
	for _, test := range tests {
		remote, ok := ParseGitRemoteURL(test.input)
		if !ok {
			t.Errorf("ParseGitRemoteURL(%q) = _, false; want true", test.input)
			continue
		}
		if remote.Host != test.wantHost || remote.Repository != test.wantRepo {
			t.Errorf("ParseGitRemoteURL(%q) = %+v, want %s %s", test.input, remote, test.wantHost, test.wantRepo)
		}
	}
}

func TestParseGitRemoteURLSCPForm(t *testing.T) {
	tests := []struct {
		input    string
		wantHost string
		wantRepo string
	}{
		{"git@gitea.example.com:owner/repo.git", "http://gitea.example.com", "owner/repo"},
		{"git@gitea.example.com:8418:owner/repo.git", "http://gitea.example.com:8418", "owner/repo"},
	}
	for _, test := range tests {
		remote, ok := ParseGitRemoteURL(test.input)
		if !ok {
			t.Errorf("ParseGitRemoteURL(%q) = _, false; want true", test.input)
			continue
		}
		if remote.Host != test.wantHost || remote.Repository != test.wantRepo {
			t.Errorf("ParseGitRemoteURL(%q) = %+v, want %s %s", test.input, remote, test.wantHost, test.wantRepo)
		}
	}
}

func TestParseGitRemoteURLUnrecognized(t *testing.T) {
	for _, input := range []string{"/local/path", "file:///srv/repo", ""} {
		if remote, ok := ParseGitRemoteURL(input); ok {
			t.Errorf("ParseGitRemoteURL(%q) = %+v, true; want false", input, remote)
		}
	}
}

func TestListRemotesAndSelectGitea(t *testing.T) {
	dir := t.TempDir()
	if _, err := runGit(dir, "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := runGit(dir, "remote", "add", "origin", "git@github.com:owner/repo.git"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "remote", "add", "gitea", "http://gitea.example.com:3000/owner/repo.git"); err != nil {
		t.Fatal(err)
	}
	remotes := ListRemotes(dir)
	if len(remotes) != 2 || remotes[0].Name != "origin" || remotes[0].Host != "http://github.com" {
		t.Fatalf("remotes = %+v", remotes)
	}
	remote, ok := SelectGiteaRemote(dir, func(host string) bool {
		return host == "http://gitea.example.com:3000"
	})
	if !ok || remote.Name != "gitea" || remote.Repository != "owner/repo" {
		t.Fatalf("SelectGiteaRemote() = %+v/%v, want gitea remote", remote, ok)
	}
	// 探测全不命中：回落 origin（供显式 host 场景复用仓库路径）
	fallback, ok := SelectGiteaRemote(dir, func(string) bool { return false })
	if ok || fallback.Name != "origin" {
		t.Fatalf("fallback = %+v/%v, want origin/false", fallback, ok)
	}
}
