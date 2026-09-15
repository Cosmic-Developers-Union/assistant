package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validateFixture 写一份 config.json + credentials.json（内容由调用方定制）。
func validateFixture(t *testing.T, config map[string]any, purposes []string) string {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	document, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(document, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	host := "https://gitea.example.com"
	credentialsFile := map[string]any{
		"version":     1,
		"identity":    []map[string]any{{"host": host, "user": "developer", "is_admin": true}},
		"credentials": []map[string]any{},
	}
	for _, purpose := range purposes {
		credentialsFile["credentials"] = append(credentialsFile["credentials"].([]map[string]any), map[string]any{
			"host": host, "user": "bot-" + purpose, "purpose": purpose, "token": "token-" + purpose,
		})
	}
	encoded, err := json.MarshalIndent(credentialsFile, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func validValidateConfig() map[string]any {
	return map[string]any{
		"default_provider": "minimax",
		"providers": map[string]any{
			"minimax": map[string]any{"api_key": "sk-cp-test"},
		},
		"instances": []map[string]any{{
			"host":     "https://gitea.example.com",
			"reviewer": map[string]any{"name": "ai"},
			"merger":   map[string]any{"name": "merge"},
			"repos":    []map[string]any{{"name": "acme/video"}},
		}},
	}
}

// 合法配置：内联 provider（api_key 直接写在 config.json 里）+ 齐备的用途令牌。
func TestValidatePasses(t *testing.T) {
	path := validateFixture(t, validValidateConfig(), []string{"review", "merge", "admin", "mcp"})
	stdout := &bytes.Buffer{}
	if err := runValidate(stdout, path); err != nil {
		t.Fatalf("runValidate() error = %v\n%s", err, stdout)
	}
	output := stdout.String()
	for _, want := range []string{
		"OK        providers.minimax",
		"内置预设",
		"凭据：provider env ANTHROPIC_AUTH_TOKEN",
		"OK        credentials https://gitea.example.com",
		"校验通过",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("输出缺少 %q：\n%s", want, output)
		}
	}
}

// 选中了 provider 却没有 api_key、且缺 review 令牌：两处都要报出来。
func TestValidateReportsMissingCredentialAndToken(t *testing.T) {
	config := validValidateConfig()
	config["providers"] = map[string]any{"minimax": map[string]any{}}
	path := validateFixture(t, config, []string{"admin"})
	stdout := &bytes.Buffer{}
	err := runValidate(stdout, path)
	if err == nil {
		t.Fatalf("应当报错：\n%s", stdout)
	}
	output := stdout.String()
	for _, want := range []string{
		"ERROR     providers.minimax",
		"没有 api_key/auth_token",
		"ERROR     credentials https://gitea.example.com",
		"缺 review",
		"assistant setup --host https://gitea.example.com",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("输出缺少 %q：\n%s", want, output)
		}
	}
}

// 定义了 provider 但没有任何地方引用它：最常见的失误，必须指出来（且不算错误）。
func TestValidateWarnsUnreferencedProvider(t *testing.T) {
	config := validValidateConfig()
	config["providers"] = map[string]any{
		"minimax": map[string]any{"api_key": "sk-cp-test"},
		"zhipu":   map[string]any{"api_key": "unused"},
	}
	path := validateFixture(t, config, []string{"review", "merge", "admin", "mcp"})
	stdout := &bytes.Buffer{}
	if err := runValidate(stdout, path); err != nil {
		t.Fatalf("未引用的 provider 只是提示，不该报错：%v\n%s", err, stdout)
	}
	output := stdout.String()
	if !strings.Contains(output, "WARN      providers.zhipu") ||
		!strings.Contains(output, "已定义但没有任何地方引用") {
		t.Errorf("缺少未引用提示：\n%s", output)
	}
	if strings.Contains(output, "WARN      providers.minimax") {
		t.Errorf("被引用的 provider 不该提示：\n%s", output)
	}
}

// 一个 provider 都没选中、进程环境也没有凭据：直接报错，而不是等会话认证失败。
func TestValidateReportsNoProviderSelected(t *testing.T) {
	config := validValidateConfig()
	delete(config, "default_provider")
	config["providers"] = map[string]any{"minimax": map[string]any{"api_key": "sk-cp-test"}}
	path := validateFixture(t, config, []string{"review", "merge", "admin", "mcp"})
	stdout := &bytes.Buffer{}
	if err := runValidate(stdout, path); err == nil {
		t.Fatalf("应当报错：\n%s", stdout)
	}
	if !strings.Contains(stdout.String(), "没有选中任何 provider") {
		t.Errorf("缺少未选中 provider 的报错：\n%s", stdout)
	}
}

// 非法 JSON 由配置加载器报出（带文件路径），validate 原样转述，不再加一层前缀。
func TestValidateSurfacesLoadError(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout := &bytes.Buffer{}
	err := runValidate(stdout, path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %v, want 文件路径", err)
	}
}
