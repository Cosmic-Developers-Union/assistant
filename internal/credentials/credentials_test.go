package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sample() *File {
	return &File{Instances: Instances{Gitea: []Gitea{{Name: "site", URL: "https://site", Username: "dev", Token: "private-token"}}, QQ: []QQ{{Name: "qq", AppID: "1", AppSecret: "secret"}}, Weixin: []Weixin{{Name: "wx", URL: "https://wx", UserID: "user", BotID: "bot", Token: "secret"}}, Telegram: []Telegram{{Name: "tg", Username: "bot", Token: "secret"}}}}
}
func TestCredentialRoundTripAndStrictSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "credentials.json")
	empty, err := Load(path)
	if err != nil || len(empty.Hosts()) != 0 {
		t.Fatal(err)
	}
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if token, err := loaded.GiteaToken("https://site/"); err != nil || token != "private-token" {
		t.Fatalf("%s %v", token, err)
	}
	if token, err := loaded.GiteaToken("https://missing"); err != nil || token != "" {
		t.Fatal(err)
	}
	loaded.Instances.Gitea = append(loaded.Instances.Gitea, Gitea{Name: "second", URL: "https://site/", Username: "other", Token: "other"})
	if len(loaded.Hosts()) != 1 {
		t.Fatal(loaded.Hosts())
	}
	if _, err := loaded.GiteaToken("https://site"); err == nil {
		t.Fatal("猜测多账号")
	}
	for _, data := range []string{`{"instances":{"gitea":[{"name":"a","url":"https://site","username":"user","token":"secret","password":"secret"}]}}`, `{"instances":{"unknown":[]}}`, `{"version":1,"credentials":[]}`, `{} {}`, `bad`, `{"instances":{"gitea":[{}]}}`} {
		_ = os.WriteFile(path, []byte(data), 0o600)
		if _, err := Load(path); err == nil {
			t.Errorf("非法凭据被接受: %s", data)
		}
	}
	if _, err := Load(filepath.Dir(path)); err == nil {
		t.Fatal("目录被当作文件")
	}
	blocker := filepath.Join(t.TempDir(), "blocker")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	if err := Save(filepath.Join(blocker, "file"), sample()); err == nil {
		t.Fatal("保存失败被吞掉")
	}
	if err := Save(t.TempDir(), sample()); err == nil {
		t.Fatal("替换目录被接受")
	}
}
func TestCredentialsValidation(t *testing.T) {
	for _, change := range []func(*File){
		func(f *File) { f.Instances.Gitea[0].Name = "" }, func(f *File) { f.Instances.Gitea[0].URL = "bad" }, func(f *File) { f.Instances.Gitea[0].Token = "" },
		func(f *File) { f.Instances.QQ[0].Name = "site" }, func(f *File) { f.Instances.QQ[0].AppSecret = "" },
		func(f *File) { f.Instances.Weixin[0].Name = "" }, func(f *File) { f.Instances.Weixin[0].URL = "bad" }, func(f *File) { f.Instances.Weixin[0].UserID = "" },
		func(f *File) { f.Instances.Telegram[0].Name = "" }, func(f *File) { f.Instances.Telegram[0].Token = "" },
	} {
		file := sample()
		change(file)
		if err := file.Validate(); err == nil {
			t.Fatal("坏凭据被接受")
		}
		if err := Save(filepath.Join(t.TempDir(), "file"), file); err == nil {
			t.Fatal("写入坏凭据")
		}
	}
	for _, url := range []string{"ftp://site", "https://a:b@site", "https://site?a=1", "https://site#part", "https://"} {
		if err := ValidateHost(url); err == nil {
			t.Fatal(url)
		}
	}
	for _, repo := range []string{"repo", "/repo", "a/", "a/b/c", "../repo", "a/..", "a\\b/c", "a/b c"} {
		if _, _, err := ParseRepoName(repo); err == nil {
			t.Fatal(repo)
		}
	}
	if owner, name, err := ParseRepoName("acme/repo"); err != nil || owner != "acme" || name != "repo" {
		t.Fatal(err)
	}
}
func TestCredentialPath(t *testing.T) {
	override := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", override)
	if path, err := Path(); err != nil || path != override {
		t.Fatal(path, err)
	}
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := Path()
	if err != nil || !strings.HasSuffix(path, "Cosmic-Developers-Union/assistant/credentials.json") {
		t.Fatal(path, err)
	}
	if _, err := Load(override); !errors.Is(err, nil) {
		t.Fatal(err)
	}
}
