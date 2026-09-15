package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"assistant/schema"
)

// writeCredentials 写一份 credentials.json：每个 host 一条登录身份 + 四种用途令牌
// （config init 的预填只看身份；令牌让随后的校验不再报缺令牌）。
func writeCredentials(t *testing.T, dir string, hosts ...string) {
	t.Helper()
	identities := make([]map[string]any, 0, len(hosts))
	credentials := make([]map[string]any, 0, len(hosts)*4)
	for _, host := range hosts {
		identities = append(identities, map[string]any{"host": host, "user": "Ge", "is_admin": true})
		for _, purpose := range []string{"review", "merge", "admin", "mcp"} {
			credentials = append(credentials, map[string]any{
				"host": host, "user": "bot-" + purpose, "purpose": purpose, "token": "token-" + purpose,
			})
		}
	}
	document := map[string]any{"version": 1, "identity": identities, "credentials": credentials}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// execConfigInit 以固定配置路径执行 `assistant config init`。
func execConfigInit(t *testing.T, path string, stdin string, args ...string) (string, error) {
	t.Helper()
	command := newConfigCommand(&path)
	command.SetArgs(append([]string{"init"}, args...))
	command.SetIn(strings.NewReader(stdin))
	output := &bytes.Buffer{}
	command.SetOut(output)
	command.SetErr(output)
	err := command.Execute()
	return output.String(), err
}

func readConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("生成的配置不是合法 JSON：%v\n%s", err, data)
	}
	return document
}

// 首次生成：用 credentials.json 里已登录的平台预填 instances，补 $schema 并把
// schema 写到旁边；没有 provider 时校验会报出来（写入仍然完成）。
func TestConfigInitPrefillsFromCredentials(t *testing.T) {
	dir := t.TempDir()
	writeCredentials(t, dir, "https://gitea.mms.vincentge.top", "https://gitea.aicler.com")
	path := filepath.Join(dir, "config.json")

	output, err := execConfigInit(t, path, "")
	if err == nil || !strings.Contains(err.Error(), "已写入") {
		t.Fatalf("没有 provider 时应当写入后报校验问题：err=%v\n%s", err, output)
	}
	if !strings.Contains(output, "+ instances[] https://gitea.mms.vincentge.top") ||
		!strings.Contains(output, "+ $schema: "+schema.Reference) {
		t.Errorf("输出缺少补全项：\n%s", output)
	}
	document := readConfig(t, path)
	if document["$schema"] != schema.Reference {
		t.Errorf("$schema = %v", document["$schema"])
	}
	instances, _ := document["instances"].([]any)
	if len(instances) != 2 {
		t.Fatalf("instances = %v", document["instances"])
	}
	var prefilled map[string]any
	for _, raw := range instances {
		if instance, _ := raw.(map[string]any); instance["host"] == "https://gitea.mms.vincentge.top" {
			prefilled = instance
		}
	}
	if prefilled == nil {
		t.Fatalf("未预填登录过的平台：%v", document["instances"])
	}
	if reviewer, _ := prefilled["reviewer"].(map[string]any); reviewer["name"] != "ai" {
		t.Errorf("reviewer 应由约定值补齐：%v", prefilled["reviewer"])
	}
	if merger, _ := prefilled["merger"].(map[string]any); merger["name"] != "merge" {
		t.Errorf("merger 应由约定值补齐：%v", prefilled["merger"])
	}
	// schema 随二进制写到旁边，内容与内嵌版本一致
	written, err := os.ReadFile(filepath.Join(dir, schema.FileName))
	if err != nil {
		t.Fatalf("schema 未写出：%v", err)
	}
	if !bytes.Equal(written, schema.Config) {
		t.Error("写出的 schema 与内嵌版本不一致")
	}
}

// --provider + --api-key-stdin：写 provider 桩、切默认 provider、密钥只进文件。
func TestConfigInitProviderAndAPIKey(t *testing.T) {
	dir := t.TempDir()
	writeCredentials(t, dir, "https://gitea.example.com")
	path := filepath.Join(dir, "config.json")

	output, err := execConfigInit(t, path, "sk-cp-from-stdin\n", "--provider", "minimax", "--api-key-stdin")
	if err != nil {
		t.Fatalf("runConfigInit() error = %v\n%s", err, output)
	}
	document := readConfig(t, path)
	if document["default_provider"] != "minimax" {
		t.Errorf("default_provider = %v", document["default_provider"])
	}
	providers, _ := document["providers"].(map[string]any)
	minimax, _ := providers["minimax"].(map[string]any)
	if minimax["api_key"] != "sk-cp-from-stdin" {
		t.Errorf("providers.minimax = %v", minimax)
	}
	if !strings.Contains(output, "providers.minimax.api_key") {
		t.Errorf("输出未说明写入了密钥：\n%s", output)
	}
	// 密钥不能出现在 stdout（只报「写入了」，不回显值）
	if strings.Contains(output, "sk-cp-from-stdin") {
		t.Errorf("输出回显了密钥：\n%s", output)
	}
}

// 已有值一律不动：只补缺失项，重复执行无改动；旧文件在首次补全时备份。
func TestConfigInitKeepsUserValuesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeCredentials(t, dir, "https://gitea.example.com")
	path := filepath.Join(dir, "config.json")
	existing := `{
  "$schema": "./config.schema.json",
  "default_provider": "zhipu",
  "providers": { "zhipu": { "api_key": "keep-me" } },
  "optimizations": { "env": { "API_TIMEOUT_MS": "3000000" } },
  "instances": [
    {
      "host": "https://gitea.example.com",
      "provider": "zhipu",
      "reviewer": { "name": "reviewer-bot" },
      "merger": { "name": "merge-bot" },
      "repos": [ { "name": "acme/video", "dir": "/srv/video" } ]
    }
  ]
}
`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := execConfigInit(t, path, "")
	if err != nil {
		t.Fatalf("已经完整的配置不该报错：%v\n%s", err, output)
	}
	if !strings.Contains(output, "配置已是最新") {
		t.Errorf("应当报告无改动：\n%s", output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != existing {
		t.Errorf("用户配置被改动了：\n%s", data)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Error("无改动时不该产生备份")
	}
}

// --dry-run 不写任何文件。
func TestConfigInitDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	writeCredentials(t, dir, "https://gitea.example.com")
	path := filepath.Join(dir, "config.json")

	output, err := execConfigInit(t, path, "", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run 不该报错：%v\n%s", err, output)
	}
	for _, name := range []string{"config.json", schema.FileName, "config.json.bak"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("dry-run 写入了 %s", name)
		}
	}
	if !strings.Contains(output, "dry-run：将写入") || !strings.Contains(output, "\"$schema\"") {
		t.Errorf("dry-run 应打印差异与合并结果：\n%s", output)
	}
}

// 补全时旧文件备份为 config.json.bak，且内容是备份当时的旧值。
func TestConfigInitBacksUpExistingFile(t *testing.T) {
	dir := t.TempDir()
	writeCredentials(t, dir, "https://gitea.example.com")
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"instances": [{"host": "https://other.example.com", "repos": []}]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 这里的校验会报「zhipu 没有 api_key」（预期：测试没给密钥），写入本身必须完成
	output, err := execConfigInit(t, path, "", "--provider", "zhipu")
	if err != nil && !strings.Contains(err.Error(), "已写入") {
		t.Fatalf("runConfigInit() error = %v\n%s", err, output)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("缺少备份：%v", err)
	}
	if !strings.Contains(string(backup), "other.example.com") {
		t.Errorf("备份内容不对：%s", backup)
	}
	if !strings.Contains(output, "备份为 config.json.bak") {
		t.Errorf("输出未提示备份：\n%s", output)
	}
	document := readConfig(t, path)
	if instances, _ := document["instances"].([]any); len(instances) != 2 {
		t.Errorf("应保留原有平台并补上新登录的平台：%v", document["instances"])
	}
}
