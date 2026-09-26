package repoinstall

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func statusOf(t *testing.T, findings []Finding, path string) string {
	t.Helper()
	for _, finding := range findings {
		if finding.Path == path {
			return finding.Status
		}
	}
	t.Fatalf("no finding for %s: %+v", path, findings)
	return ""
}

// doctor 对 install 产物做只读体检：安装前缺失，安装后全绿；文件被改写、
// 删除、替换为用户自有或遗留旧文件时给出对应状态。
func TestDoctorDetectsConfigurationDrift(t *testing.T) {
	options := testOptions(t)

	findings, err := Doctor(options)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	for _, path := range []string{ManagedSkillPath(), ManagedAgentPath(), ManagedClaudePath(), ManagedReviewPath(), ManagedWorkflowPaths()[0], ".mcp.json", "opencode.json", options.CodexConfigPath} {
		if status := statusOf(t, findings, path); status != StatusMissing {
			t.Errorf("未安装时 %s 状态 = %s, want missing", path, status)
		}
	}

	if err := Install(context.Background(), options); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	findings, err = Doctor(options)
	if err != nil {
		t.Fatalf("Doctor() after install error = %v", err)
	}
	for _, finding := range findings {
		if !finding.OK() {
			t.Errorf("install 后应全部正常: %+v", finding)
		}
	}

	// skill 被改写（保留 marker）→ outdated
	skillPath := filepath.Join(options.Dir, ManagedSkillPath())
	if err := os.WriteFile(skillPath, []byte("<!-- managed-by: assistant -->\n# hacked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// workflow 被删除 → missing
	workflowPath := filepath.Join(options.Dir, ManagedWorkflowPaths()[0])
	if err := os.Remove(workflowPath); err != nil {
		t.Fatal(err)
	}
	// AGENTS.md 被替换为用户自有（无 marker）→ unmanaged
	if err := os.WriteFile(filepath.Join(options.Dir, ManagedAgentPath()), []byte("# user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// CLAUDE.md 被替换为用户自有（无 @AGENTS.md）→ unmanaged
	if err := os.WriteFile(filepath.Join(options.Dir, ManagedClaudePath()), []byte("# user claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// review.md 被替换为用户自有（无 marker）→ unmanaged
	if err := os.WriteFile(filepath.Join(options.Dir, ManagedReviewPath()), []byte("# user review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 旧版 automerge.yml（带 marker）→ legacy
	legacyPath := filepath.Join(options.Dir, LegacyWorkflowPaths()[0])
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("<!-- managed-by: assistant -->\nold\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// opencode 放行被移除 → outdated
	if err := os.WriteFile(
		filepath.Join(options.Dir, "opencode.json"),
		[]byte(`{"mcp":{"gitea":{"type":"local","command":["assistant","mcp","gitea"],"enabled":true}}}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	// claude settings 被砍成只剩 MCP 放行（缺 env/只读权限）→ outdated
	if err := os.WriteFile(
		filepath.Join(options.Dir, ".claude", "settings.json"),
		[]byte(`{"enableAllProjectMcpServers":true,"permissions":{"allow":["mcp__gitea","mcp__gitea__*"]}}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	findings, err = Doctor(options)
	if err != nil {
		t.Fatalf("Doctor() after drift error = %v", err)
	}
	for path, want := range map[string]string{
		ManagedSkillPath():        StatusOutdated,
		ManagedWorkflowPaths()[0]: StatusMissing,
		ManagedAgentPath():        StatusUnmanaged,
		ManagedClaudePath():       StatusUnmanaged,
		ManagedReviewPath():       StatusUnmanaged,
		LegacyWorkflowPaths()[0]:  StatusLegacy,
		"opencode.json":           StatusOutdated,
		".claude/settings.json":   StatusOutdated,
	} {
		if status := statusOf(t, findings, path); status != want {
			t.Errorf("%s 状态 = %s, want %s（%+v）", path, status, want, findings)
		}
	}
}

// Summary 是 CLI 之外也会用到的展示口径：全 OK 说「配置正常」，否则报问题数。
func TestSummary(t *testing.T) {
	if got := Summary(nil); got != "配置正常" {
		t.Errorf("Summary(nil) = %q, want 配置正常", got)
	}
	allOK := []Finding{{Path: "a", Status: StatusOK}, {Path: "b", Status: StatusOK}}
	if got := Summary(allOK); got != "配置正常" {
		t.Errorf("Summary(全 OK) = %q, want 配置正常", got)
	}
	// 只有 OK 算正常；任一非 OK 状态都计入问题数
	withLegacy := append(append([]Finding(nil), allOK...), Finding{Path: "c", Status: StatusLegacy})
	if got := Summary(withLegacy); got != "1 处配置问题" {
		t.Errorf("Summary(含 legacy) = %q, want 1 处配置问题", got)
	}
	mixed := []Finding{
		{Path: "a", Status: StatusOK},
		{Path: "b", Status: StatusMissing},
		{Path: "c", Status: StatusOutdated},
		{Path: "d", Status: StatusUnmanaged},
	}
	if got := Summary(mixed); got != "3 处配置问题" {
		t.Errorf("Summary(mixed) = %q, want 3 处配置问题", got)
	}
}

// checkFile 的四种形态：缺失、无 marker（用户自有）、内容一致、内容过期。
// 这四条是 doctor 判断「这份产物还能不能自动更新」的全部依据。
func TestCheckFileStates(t *testing.T) {
	const relative = "managed.txt"
	const expected = Marker + "\n托管内容\n"

	t.Run("缺失", func(t *testing.T) {
		options := testOptions(t)
		if got := checkFile(&options, relative, expected); got.Status != StatusMissing {
			t.Errorf("Status = %q, want %q", got.Status, StatusMissing)
		}
	})

	t.Run("存在但无 marker 视为用户自有", func(t *testing.T) {
		options := testOptions(t)
		path := filepath.Join(options.Dir, relative)
		if err := os.WriteFile(path, []byte("我自己写的\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := checkFile(&options, relative, expected)
		if got.Status != StatusUnmanaged {
			t.Errorf("Status = %q, want %q（无 marker 不得自动覆盖）", got.Status, StatusUnmanaged)
		}
	})

	t.Run("内容一致为 OK", func(t *testing.T) {
		options := testOptions(t)
		path := filepath.Join(options.Dir, relative)
		if err := os.WriteFile(path, []byte(expected), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := checkFile(&options, relative, expected); got.Status != StatusOK {
			t.Errorf("Status = %q, want %q", got.Status, StatusOK)
		}
	})

	t.Run("内容漂移为过期", func(t *testing.T) {
		options := testOptions(t)
		path := filepath.Join(options.Dir, relative)
		if err := os.WriteFile(path, []byte(Marker+"\n旧内容\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := checkFile(&options, relative, expected)
		if got.Status != StatusOutdated {
			t.Errorf("Status = %q, want %q", got.Status, StatusOutdated)
		}
	})
}

// loadJSON 三分支：不存在（found=false）、解析失败（found=true + err）、正常。
// found 与 err 必须能区分——调用方据此决定「首次安装」还是「报配置损坏」。
func TestLoadJSONStates(t *testing.T) {
	t.Run("不存在", func(t *testing.T) {
		document, found, err := loadJSON(filepath.Join(t.TempDir(), "missing.json"))
		if err != nil || found || document != nil {
			t.Errorf("document/found/err = %v/%v/%v, want nil/false/nil", document, found, err)
		}
	})

	t.Run("解析失败", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "broken.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		document, found, err := loadJSON(path)
		if err == nil {
			t.Error("err = nil, want 解析失败")
		}
		if !found {
			t.Error("found = false, want true（文件存在，只是坏了）")
		}
		if document != nil {
			t.Errorf("document = %v, want nil", document)
		}
	})

	t.Run("正常", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ok.json")
		if err := os.WriteFile(path, []byte(`{"a":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
		document, found, err := loadJSON(path)
		if err != nil || !found {
			t.Fatalf("found/err = %v/%v, want true/nil", found, err)
		}
		if document["a"] != float64(1) {
			t.Errorf("document = %v, want a=1", document)
		}
	})
}

// managedSection 的边界：无起始 marker 返回空；无闭合 marker 时取到文件末尾
// （宁可把后续用户内容一并当托管段，也不静默丢掉整段）。
func TestManagedSectionBoundaries(t *testing.T) {
	full := "前言\n<!-- " + Marker + " -->\n托管\n<!-- /" + Marker + " -->\n后记"
	if got := managedSection(full); !strings.Contains(got, "托管") || strings.Contains(got, "后记") {
		t.Errorf("managedSection() = %q, want 只含托管段", got)
	}
	if got := managedSection("没有 marker 的文件"); got != "" {
		t.Errorf("无 marker 时应返回空，实际 %q", got)
	}
	unclosed := "前言\n<!-- " + Marker + " -->\n托管内容"
	if got := managedSection(unclosed); !strings.Contains(got, "托管内容") {
		t.Errorf("未闭合时应取到末尾，实际 %q", got)
	}
}

// Doctor 必须报出来但**不改写**用户自有文件：doctor 是只读体检，碰到无 marker
// 的文件只能标记 UNMANAGED，绝不能顺手覆盖。
func TestDoctorIsReadOnlyOnUserFiles(t *testing.T) {
	options := testOptions(t)
	path := filepath.Join(options.Dir, ManagedAgentPath())
	userContent := "# 我自己维护的协作约定\n"
	if err := os.WriteFile(path, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}

	findings, err := Doctor(options)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if got := statusOf(t, findings, ManagedAgentPath()); got == StatusOK {
		t.Error("用户自有文件不应被判为 OK")
	}
	if got := readFile(t, path); got != userContent {
		t.Errorf("doctor 改写了用户文件：%q", got)
	}
}
