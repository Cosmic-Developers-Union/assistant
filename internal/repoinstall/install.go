package repoinstall

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Options 控制 install/uninstall 的行为。
type Options struct {
	// Dir 是仓库检出根。
	Dir string
	// Tools 是要配置的 AI CLI：claude、opencode、codex（zcode 暂不支持）。
	Tools []string
	// CodexConfigPath 覆盖 ~/.codex/config.toml 位置（测试用）。
	CodexConfigPath string
	// Reviewer / Merger 是内容/状态评审账号名（缺省约定 ai / merge）。
	Reviewer string
	Merger   string
	// Image 是仓库 workflow 运行的 assistant 容器镜像（缺省官方镜像）。
	Image  string
	DryRun bool
	Log    func(string, ...any)
}

var supportedTools = []string{"claude", "opencode", "codex"}

// SupportedTools 返回支持的 AI CLI 列表。
func SupportedTools() []string {
	return append([]string(nil), supportedTools...)
}

func (o *Options) normalize() error {
	if o.Dir == "" {
		o.Dir = "."
	}
	absolute, err := filepath.Abs(o.Dir)
	if err != nil {
		return err
	}
	o.Dir = absolute
	if len(o.Tools) == 0 {
		o.Tools = append([]string(nil), supportedTools...)
	}
	for _, tool := range o.Tools {
		found := false
		for _, supported := range supportedTools {
			if tool == supported {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("不支持的 --tools 值：%s（支持 %s；zcode 暂不支持）",
				tool, strings.Join(supportedTools, ","))
		}
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.Reviewer == "" {
		o.Reviewer = ConventionReviewer
	}
	if o.Merger == "" {
		o.Merger = ConventionMerger
	}
	if o.Image == "" {
		o.Image = DefaultImage
	}
	if o.CodexConfigPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("定位用户目录: %w", err)
		}
		o.CodexConfigPath = filepath.Join(home, ".codex", "config.toml")
	}
	return nil
}

func (o *Options) write(path, content string) error {
	relative, err := filepath.Rel(o.Dir, path)
	if err != nil {
		relative = path
	}
	if o.DryRun {
		o.Log("dry-run：写入 %s", relative)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	o.Log("已写入 %s", relative)
	return nil
}

func (o *Options) remove(path string) error {
	relative, err := filepath.Rel(o.Dir, path)
	if err != nil {
		relative = path
	}
	if o.DryRun {
		o.Log("dry-run：删除 %s", relative)
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	o.Log("已删除 %s", relative)
	return nil
}

// Install 把仓库配置成 assistant 工作流。
func Install(ctx context.Context, options Options) error {
	if err := options.normalize(); err != nil {
		return err
	}
	data := TemplateData{Reviewer: options.Reviewer, Merger: options.Merger, Image: options.Image}
	// 1) 评审 skill / AGENTS.md / 仓库 workflow（模板绑定渲染）
	skill, err := renderTemplate(skillTemplateName, data)
	if err != nil {
		return err
	}
	if err := installFile(&options, filepath.Join(options.Dir, ManagedSkillPath()), skill); err != nil {
		return err
	}
	agents, err := renderTemplate(agentTemplateName, data)
	if err != nil {
		return err
	}
	if err := installSection(&options, filepath.Join(options.Dir, ManagedAgentPath()), agents); err != nil {
		return err
	}
	workflow, err := renderTemplate(workflowTemplateName, data)
	if err != nil {
		return err
	}
	for _, relative := range ManagedWorkflowPaths() {
		if err := installFile(&options, filepath.Join(options.Dir, relative), workflow); err != nil {
			return err
		}
	}
	// 旧版独立 automerge workflow 已合并进 assistant.yml：带 marker 时清理
	for _, relative := range LegacyWorkflowPaths() {
		if err := uninstallFile(&options, filepath.Join(options.Dir, relative)); err != nil {
			return err
		}
	}
	// 2) 各 AI CLI 的 MCP 配置
	for _, tool := range options.Tools {
		var err error
		switch tool {
		case "claude":
			err = installClaude(&options)
		case "opencode":
			err = installOpenCode(&options)
		case "codex":
			err = installCodex(&options)
		}
		if err != nil {
			return fmt.Errorf("%s MCP 配置: %w", tool, err)
		}
	}
	return nil
}

// Uninstall 移除 install 写入的内容（只触碰带 marker 的内容）。
func Uninstall(ctx context.Context, options Options) error {
	if err := options.normalize(); err != nil {
		return err
	}
	if err := uninstallFile(&options, filepath.Join(options.Dir, ManagedSkillPath())); err != nil {
		return err
	}
	if err := uninstallSection(&options, filepath.Join(options.Dir, ManagedAgentPath())); err != nil {
		return err
	}
	for _, relative := range ManagedWorkflowPaths() {
		if err := uninstallFile(&options, filepath.Join(options.Dir, relative)); err != nil {
			return err
		}
	}
	for _, relative := range LegacyWorkflowPaths() {
		if err := uninstallFile(&options, filepath.Join(options.Dir, relative)); err != nil {
			return err
		}
	}
	for _, tool := range options.Tools {
		var err error
		switch tool {
		case "claude":
			err = uninstallClaude(&options)
		case "opencode":
			err = uninstallOpenCode(&options)
		case "codex":
			err = uninstallCodex(&options)
		}
		if err != nil {
			return fmt.Errorf("%s MCP 配置: %w", tool, err)
		}
	}
	return nil
}

// installFile 写入整文件：不存在则创建；已存在则必须是 assistant 生成（含
// marker）才覆盖，避免覆盖用户手写内容。
func installFile(options *Options, path, content string) error {
	existing, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return options.write(path, content)
	case err != nil:
		return err
	case strings.Contains(string(existing), Marker):
		if string(existing) == content {
			options.Log("已是最新：%s", path)
			return nil
		}
		return options.write(path, content)
	default:
		return fmt.Errorf("%s 已存在且不是 assistant 生成（缺少 marker）；请先处理该文件", path)
	}
}

func uninstallFile(options *Options, path string) error {
	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.Contains(string(existing), Marker) {
		options.Log("跳过 %s（非 assistant 生成，缺少 marker）", path)
		return nil
	}
	return options.remove(path)
}

// installSection 用于 AGENTS.md：无 marker 时追加一个 marker 段落，已有则替换。
func installSection(options *Options, path, section string) error {
	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return options.write(path, section)
	}
	if err != nil {
		return err
	}
	content := string(existing)
	if strings.Contains(content, Marker) {
		if content == section || strings.Contains(content, section) {
			options.Log("已是最新：%s", path)
			return nil
		}
		return options.write(path, replaceManagedSection(content, section))
	}
	merged := strings.TrimRight(content, "\n") + "\n\n" + section
	return options.write(path, merged)
}

func uninstallSection(options *Options, path string) error {
	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	content := string(existing)
	if !strings.Contains(content, Marker) {
		options.Log("跳过 %s（无 assistant 段落）", path)
		return nil
	}
	remaining := replaceManagedSection(content, "")
	if strings.TrimSpace(remaining) == "" {
		return options.remove(path)
	}
	return options.write(path, remaining)
}

// replaceManagedSection 用 replacement 替换 marker 段落（begin..end），空串即删除。
func replaceManagedSection(content, replacement string) string {
	begin := strings.Index(content, "<!-- "+Marker)
	if begin < 0 {
		return content
	}
	endMarker := "<!-- /" + Marker + " -->"
	end := strings.Index(content[begin:], endMarker)
	if end < 0 {
		// 兼容只有起始 marker 的旧内容：删到文件尾
		return strings.TrimRight(content[:begin], "\n") + "\n"
	}
	end += begin + len(endMarker)
	prefix := strings.TrimRight(content[:begin], "\n")
	suffix := strings.TrimLeft(content[end:], "\n")
	parts := make([]string, 0, 2)
	if prefix != "" {
		parts = append(parts, prefix)
	}
	if replacement != "" {
		parts = append(parts, strings.TrimRight(replacement, "\n"))
	}
	if suffix != "" {
		parts = append(parts, suffix)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n") + "\n"
}

// ---- Claude ----

func installClaude(options *Options) error {
	mcpPath := filepath.Join(options.Dir, ".mcp.json")
	settingsPath := filepath.Join(options.Dir, ".claude", "settings.json")
	if err := updateJSON(mcpPath, func(document map[string]any) {
		servers, _ := document["mcpServers"].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
		}
		servers["gitea"] = map[string]any{"command": "assistant", "args": []any{"mcp", "gitea"}}
		document["mcpServers"] = servers
	}, options); err != nil {
		return err
	}
	return updateJSON(settingsPath, func(document map[string]any) {
		document["enableAllProjectMcpServers"] = true
		permissions, _ := document["permissions"].(map[string]any)
		if permissions == nil {
			permissions = map[string]any{}
		}
		allow := stringSlice(permissions["allow"])
		allow = appendUnique(allow, "mcp__gitea", "mcp__gitea__*")
		permissions["allow"] = allow
		document["permissions"] = permissions
	}, options)
}

func uninstallClaude(options *Options) error {
	if err := updateJSON(filepath.Join(options.Dir, ".mcp.json"), func(document map[string]any) {
		if servers, ok := document["mcpServers"].(map[string]any); ok {
			delete(servers, "gitea")
			if len(servers) == 0 {
				delete(document, "mcpServers")
			}
		}
	}, options); err != nil {
		return err
	}
	return updateJSON(filepath.Join(options.Dir, ".claude", "settings.json"), func(document map[string]any) {
		if permissions, ok := document["permissions"].(map[string]any); ok {
			allow := removeStrings(stringSlice(permissions["allow"]), "mcp__gitea", "mcp__gitea__*")
			if len(allow) == 0 {
				delete(permissions, "allow")
			} else {
				permissions["allow"] = allow
			}
			if len(permissions) == 0 {
				delete(document, "permissions")
			}
		}
	}, options)
}

// ---- opencode ----

func installOpenCode(options *Options) error {
	path := filepath.Join(options.Dir, "opencode.json")
	return updateJSON(path, func(document map[string]any) {
		if _, ok := document["$schema"]; !ok {
			document["$schema"] = "https://opencode.ai/config.json"
		}
		servers, _ := document["mcp"].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
		}
		servers["gitea"] = map[string]any{
			"type":    "local",
			"command": []any{"assistant", "mcp", "gitea"},
			"enabled": true,
		}
		document["mcp"] = servers
		permissions, _ := document["permission"].(map[string]any)
		if permissions == nil {
			permissions = map[string]any{}
		}
		// MCP 工具在 opencode 里名为 gitea_<tool>，通配放行
		permissions["gitea_*"] = "allow"
		document["permission"] = permissions
	}, options)
}

func uninstallOpenCode(options *Options) error {
	path := filepath.Join(options.Dir, "opencode.json")
	return updateJSON(path, func(document map[string]any) {
		if servers, ok := document["mcp"].(map[string]any); ok {
			delete(servers, "gitea")
			if len(servers) == 0 {
				delete(document, "mcp")
			}
		}
		if permissions, ok := document["permission"].(map[string]any); ok {
			delete(permissions, "gitea_*")
			if len(permissions) == 0 {
				delete(document, "permission")
			}
		}
	}, options)
}

// ---- Codex（全局 ~/.codex/config.toml）----

const codexServerBlock = `# managed-by: assistant
[mcp_servers.gitea]
command = "assistant"
args = ["mcp", "gitea"]
`

const codexApprovalLine = `# managed-by: assistant (auto-allow)
approval_policy = "never"
`

func installCodex(options *Options) error {
	existing, err := os.ReadFile(options.CodexConfigPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := string(existing)
	content = replaceCodexServerBlock(content)
	if !strings.Contains(content, "approval_policy") {
		content = codexApprovalLine + "\n" + strings.TrimLeft(content, "\n")
	}
	if options.DryRun {
		options.Log("dry-run：写入 %s（[mcp_servers.gitea] + approval_policy=never）", options.CodexConfigPath)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(options.CodexConfigPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(options.CodexConfigPath, []byte(content), 0o644); err != nil {
		return err
	}
	options.Log("已更新 %s（[mcp_servers.gitea]）", options.CodexConfigPath)
	return nil
}

func uninstallCodex(options *Options) error {
	existing, err := os.ReadFile(options.CodexConfigPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	lines := strings.Split(string(existing), "\n")
	kept := make([]string, 0, len(lines))
	skipBlock := false
	removeNextApproval := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "# managed-by: assistant":
			skipBlock = true
			continue
		case trimmed == "# managed-by: assistant (auto-allow)":
			removeNextApproval = true
			continue
		case removeNextApproval:
			removeNextApproval = false
			if strings.HasPrefix(trimmed, "approval_policy") {
				continue
			}
		case skipBlock:
			if strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, "[mcp_servers.gitea]") {
				skipBlock = false
			} else {
				continue
			}
		}
		if strings.HasPrefix(trimmed, "[mcp_servers.gitea]") {
			skipBlock = true
			continue
		}
		kept = append(kept, line)
	}
	content := strings.TrimSpace(strings.Join(kept, "\n"))
	if content != "" {
		content += "\n"
	}
	if options.DryRun {
		options.Log("dry-run：清理 %s 的 assistant 配置", options.CodexConfigPath)
		return nil
	}
	if content == "" {
		if err := os.Remove(options.CodexConfigPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := os.WriteFile(options.CodexConfigPath, []byte(content), 0o644); err != nil {
		return err
	}
	options.Log("已清理 %s 的 assistant 配置", options.CodexConfigPath)
	return nil
}

// replaceCodexServerBlock 移除旧 block（若存在）后追加新 block。
func replaceCodexServerBlock(content string) string {
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	removing := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "# managed-by: assistant":
			removing = true
			continue
		case removing:
			if strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, "[mcp_servers.gitea]") {
				removing = false
			} else {
				continue
			}
		}
		if strings.HasPrefix(trimmed, "[mcp_servers.gitea]") {
			removing = true
			continue
		}
		kept = append(kept, line)
	}
	base := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if base != "" {
		base += "\n\n"
	}
	return base + codexServerBlock
}

// ---- JSON 工具 ----

func updateJSON(path string, mutate func(map[string]any), options *Options) error {
	document := map[string]any{}
	existing, err := os.ReadFile(path)
	switch {
	case err == nil && len(strings.TrimSpace(string(existing))) > 0:
		if err := json.Unmarshal(existing, &document); err != nil {
			return fmt.Errorf("解析 %s: %w", path, err)
		}
	case err != nil && !os.IsNotExist(err):
		return err
	}
	before, _ := json.Marshal(document)
	mutate(document)
	after, _ := json.Marshal(document)
	changed := string(before) != string(after)
	relative, relErr := filepath.Rel(options.Dir, path)
	if relErr != nil {
		relative = path
	}
	if !changed {
		options.Log("已是最新：%s", relative)
		return nil
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if options.DryRun {
		options.Log("dry-run：写入 %s", relative)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return err
	}
	options.Log("已更新 %s", relative)
	return nil
}

func stringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		if strings, ok := value.([]string); ok {
			return strings
		}
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func appendUnique(values []string, additions ...string) []string {
	for _, addition := range additions {
		found := false
		for _, value := range values {
			if value == addition {
				found = true
				break
			}
		}
		if !found {
			values = append(values, addition)
		}
	}
	return values
}

func removeStrings(values []string, removals ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		remove := false
		for _, candidate := range removals {
			if value == candidate {
				remove = true
				break
			}
		}
		if !remove {
			result = append(result, value)
		}
	}
	return result
}
