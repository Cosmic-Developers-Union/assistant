package gitea

import (
	"os"
	"path/filepath"
	"testing"
)

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

// ListRemotes 的两个跳过出口：非 git 目录直接放弃；remote 配置读不出来
// （.git/config 被改坏）时跳过该 remote 而不是整体失败。
func TestListRemotesSkipsUnreadableEntries(t *testing.T) {
	if remotes := ListRemotes(t.TempDir()); remotes != nil {
		t.Errorf("ListRemotes(非 git 目录) = %+v, want nil（git remote 失败即放弃）", remotes)
	}

	dir := t.TempDir()
	if _, err := runGit(dir, "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	// remote 只登记了名字而缺 url 段：git remote 能列出它，config --get 失败
	config := filepath.Join(dir, ".git", "config")
	file, err := os.OpenFile(config, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("[remote \"broken\"]\n\tfetch = +refs/heads/*:refs/remotes/broken/*\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "remote", "add", "origin", "http://gitea.example.com:3000/owner/repo.git"); err != nil {
		t.Fatal(err)
	}
	remotes := ListRemotes(dir)
	if len(remotes) != 1 || remotes[0].Name != "origin" {
		t.Errorf("remotes = %+v, want 仅 origin（缺 url 的 remote 应跳过）", remotes)
	}
}

// SelectGiteaRemote：无 remote 时返回零值 false；probe 为 nil 时不探测，
// 直接回落第一个可解析 remote。
func TestSelectGiteaRemoteWithoutRemotesAndWithoutProbe(t *testing.T) {
	if remote, ok := SelectGiteaRemote(t.TempDir(), func(string) bool { return true }); ok || remote.Name != "" {
		t.Errorf("SelectGiteaRemote(无 remote) = %+v/%v, want 零值 false", remote, ok)
	}

	dir := t.TempDir()
	if _, err := runGit(dir, "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := runGit(dir, "remote", "add", "upstream", "http://gitea.example.com:3000/owner/repo.git"); err != nil {
		t.Fatal(err)
	}
	remote, ok := SelectGiteaRemote(dir, nil)
	if ok || remote.Name != "upstream" {
		t.Errorf("SelectGiteaRemote(probe=nil) = %+v/%v, want upstream/false", remote, ok)
	}
}

// 非 git 目录直接返回 nil（git remote 失败即放弃，不猜）。
// url 解析不出的 remote 跳过；可解析但 probe 全不命中时回落第一个可解析 remote。
func TestListRemotesAndSelectGiteaFailurePaths(t *testing.T) {
	if remotes := ListRemotes(t.TempDir()); remotes != nil {
		t.Errorf("ListRemotes(非 git 目录) = %+v, want nil", remotes)
	}

	// url 不是支持的形态（本地路径）：跳过该 remote，只留可解析的那个
	dir := t.TempDir()
	if _, err := runGit(dir, "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := runGit(dir, "remote", "add", "origin", "git@github.com:owner/repo.git"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "remote", "add", "local", "/srv/bare/repo.git"); err != nil {
		t.Fatal(err)
	}
	remotes := ListRemotes(dir)
	if len(remotes) != 1 || remotes[0].Name != "origin" || remotes[0].Host != "http://github.com" {
		t.Fatalf("remotes = %+v, want 仅 origin（本地路径 remote 应跳过）", remotes)
	}

	// origin 的 url 不可解析：排序里没有 origin 可提前，保持 git 的原始顺序，
	// probe 全不命中时回落 remotes[0] 并返回 ok=false（供调用方判断是否真命中）
	dir = t.TempDir()
	if _, err := runGit(dir, "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := runGit(dir, "remote", "add", "origin", "/srv/bare/repo.git"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "remote", "add", "upstream", "http://gitea.example.com:3000/owner/repo.git"); err != nil {
		t.Fatal(err)
	}
	remotes = ListRemotes(dir)
	if len(remotes) != 1 || remotes[0].Name != "upstream" {
		t.Fatalf("remotes = %+v, want 仅 upstream", remotes)
	}
	remote, ok := SelectGiteaRemote(dir, func(string) bool { return false })
	if ok || remote.Name != "upstream" {
		t.Errorf("SelectGiteaRemote(probe 全不命中) = %+v/%v, want upstream/false", remote, ok)
	}
}

// OriginRemote 供解析 remote.origin.url 复用（无 origin 时返回 false，不 panic）。
func TestOriginRemote(t *testing.T) {
	dir := t.TempDir()
	if _, err := runGit(dir, "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if url, ok := OriginRemote(dir); ok {
		t.Errorf("OriginRemote() = %q/%v, want false（未配置 origin）", url, ok)
	}
	if _, err := runGit(dir, "remote", "add", "origin", "http://gitea.example.com:8418/owner/repo.git"); err != nil {
		t.Fatal(err)
	}
	url, ok := OriginRemote(dir)
	if !ok || url != "http://gitea.example.com:8418/owner/repo.git" {
		t.Errorf("OriginRemote() = %q/%v, want origin url", url, ok)
	}
	// 非 git 目录：git 命令失败即返回 false
	if url, ok := OriginRemote(t.TempDir()); ok {
		t.Errorf("OriginRemote(非 git 目录) = %q/%v, want false", url, ok)
	}
}

// RepoRoot 用于定位宿主检出根：子目录里同样解析到根，非 git 目录返回 false。
func TestRepoRoot(t *testing.T) {
	dir := t.TempDir()
	if _, err := runGit(dir, "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	nested := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	root, ok := RepoRoot(nested)
	if !ok || root != dir {
		t.Errorf("RepoRoot(子目录) = %q/%v, want %q", root, ok, dir)
	}
	if root, ok := RepoRoot(t.TempDir()); ok {
		t.Errorf("RepoRoot(非 git 目录) = %q/%v, want false", root, ok)
	}
}
