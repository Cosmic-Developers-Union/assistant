package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/credentials"
	"assistant/internal/instances"
)

// 门禁只看记录在案的身份：非管理员立即拒绝，管理员或没有身份记录时不拦截。
func TestRequireAdminIdentity(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", "")

	if err := requireAdminIdentity(configPath, "https://a.example.com"); err != nil {
		t.Fatalf("没有身份记录时不应拦截：%v", err)
	}

	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: "https://a.example.com", User: "alice", IsAdmin: true})
	store.SetIdentity(credentials.Identity{Host: "https://b.example.com", User: "bob", IsAdmin: false})
	if err := credentials.Save(filepath.Join(dir, "credentials.json"), store); err != nil {
		t.Fatal(err)
	}
	if err := requireAdminIdentity(configPath, "https://a.example.com"); err != nil {
		t.Fatalf("管理员不应被拦截：%v", err)
	}
	err := requireAdminIdentity(configPath, "https://b.example.com")
	if err == nil {
		t.Fatal("非管理员应被拦截")
	}
	for _, want := range []string{"@bob", "不是 https://b.example.com 的实例管理员", "assistant login", "--admin-token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("门禁提示缺少 %q：%v", want, err)
		}
	}
}

// setup 在没有显式管理员凭据时，按记录在案的非管理员身份快速失败，且不做任何写操作。
func TestSetupRefusesRecordedNonAdmin(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	if err := instances.Save(configPath, &instances.File{Instances: []instances.Instance{
		{Host: "https://a.example.com"},
	}}); err != nil {
		t.Fatal(err)
	}
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: "https://a.example.com", User: "alice", IsAdmin: false})
	if err := credentials.Save(filepath.Join(dir, "credentials.json"), store); err != nil {
		t.Fatal(err)
	}
	before, err := instances.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	command := newSetupCommand(&configPath)
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--host", "https://a.example.com"})
	runErr := command.Execute()
	if runErr == nil || !strings.Contains(runErr.Error(), "实例管理员") {
		t.Fatalf("非管理员身份运行 setup 应被拒绝，got %v", runErr)
	}
	after, err := instances.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Instances) != len(before.Instances) ||
		after.Instances[0].Host != before.Instances[0].Host ||
		len(after.Instances[0].Repos) != 0 || after.Instances[0].AdminToken != "" {
		t.Fatalf("门禁失败时不应改动配置：%+v", after.Instances[0])
	}
}
