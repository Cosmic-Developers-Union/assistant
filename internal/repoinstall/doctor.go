package repoinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"assistant/content"
	"assistant/internal/claudecfg"
)

// Finding 状态：doctor 按这些状态报告每一处 install 管理产物的现状。
const (
	// StatusOK 表示与当前模板一致。
	StatusOK = "ok"
	// StatusMissing 表示尚未安装（或文件缺失）。
	StatusMissing = "missing"
	// StatusOutdated 表示已安装但内容与当前模板/镜像不一致。
	StatusOutdated = "outdated"
	// StatusUnmanaged 表示文件存在但不含 marker，install 不会覆盖。
	StatusUnmanaged = "unmanaged"
	// StatusLegacy 表示旧版本产物，install/uninstall 会清理。
	StatusLegacy = "legacy"
)

// Finding 是一处配置检测结果。
type Finding struct {
	Path   string
	Status string
	Detail string
}

// OK 表示该处无需处理。
func (f Finding) OK() bool { return f.Status == StatusOK }

// Doctor 检测仓库当前的 assistant 配置状态：对每个 install 管理的产物比对
// 当前代码渲染出的期望内容（身份约定 ai/merge、镜像取 options.Image），报告
// 缺失/过期/非托管/遗留。只读，不修改任何文件。
func Doctor(options Options) ([]Finding, error) {
	if err := options.normalize(); err != nil {
		return nil, err
	}
	data := TemplateData{Reviewer: options.Reviewer, Merger: options.Merger, Image: options.Image}
	agents, err := renderContent("agents", content.AgentsSection, data)
	if err != nil {
		return nil, err
	}
	workflow, err := renderTemplate(workflowTemplateName, data)
	if err != nil {
		return nil, err
	}

	findings := []Finding{
		checkSection(&options, ManagedAgentPath(), wrapManagedSection(agents)),
		checkClaudeMD(&options),
		checkReviewFile(&options),
	}
	for _, relative := range ManagedWorkflowPaths() {
		findings = append(findings, checkFile(&options, relative, workflow))
	}
	findings = append(findings, skillsFindings(&options)...)
	findings = append(findings, checkLegacyWorkflows(&options)...)
	for _, tool := range options.Tools {
		switch tool {
		case "claude":
			findings = append(findings, checkClaude(&options)...)
		case "opencode":
			findings = append(findings, checkOpenCode(&options)...)
		case "codex":
			findings = append(findings, checkCodex(&options)...)
		}
	}
	return findings, nil
}

// checkFile 比对整文件产物（skill/workflow）。
func checkFile(options *Options, relative, expected string) Finding {
	path := filepath.Join(options.Dir, relative)
	existing, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return Finding{Path: relative, Status: StatusMissing, Detail: "未安装"}
	case err != nil:
		return Finding{Path: relative, Status: StatusMissing, Detail: err.Error()}
	case !strings.Contains(string(existing), Marker):
		return Finding{Path: relative, Status: StatusUnmanaged, Detail: "文件存在但不含 marker（非 assistant 生成）"}
	case string(existing) == expected:
		return Finding{Path: relative, Status: StatusOK}
	default:
		return Finding{Path: relative, Status: StatusOutdated, Detail: "内容与当前模板不一致"}
	}
}

// checkSection 比对段落产物（AGENTS.md）。
func checkSection(options *Options, relative, expected string) Finding {
	path := filepath.Join(options.Dir, relative)
	existing, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return Finding{Path: relative, Status: StatusMissing, Detail: "未安装"}
	case err != nil:
		return Finding{Path: relative, Status: StatusMissing, Detail: err.Error()}
	case !strings.Contains(string(existing), Marker):
		return Finding{Path: relative, Status: StatusUnmanaged, Detail: "文件存在但不含 assistant 段落"}
	}
	current := managedSection(string(existing))
	if strings.TrimSpace(current) == strings.TrimSpace(expected) {
		return Finding{Path: relative, Status: StatusOK}
	}
	return Finding{Path: relative, Status: StatusOutdated, Detail: "assistant 段落与当前模板不一致"}
}

// checkReviewFile 检查 .assistant/review.md：缺托管段落、内容过期或用户自有。
// 托管段落是 install 注入评审会话的默认约定；用户自有内容（段落外）不受影响。
func checkReviewFile(options *Options) Finding {
	path := filepath.Join(options.Dir, ManagedReviewPath())
	existing, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return Finding{Path: ManagedReviewPath(), Status: StatusMissing, Detail: "未安装"}
	case err != nil:
		return Finding{Path: ManagedReviewPath(), Status: StatusMissing, Detail: err.Error()}
	}
	text := string(existing)
	if !strings.Contains(text, Marker) {
		return Finding{Path: ManagedReviewPath(), Status: StatusUnmanaged, Detail: "已存在用户自有 review.md（未改动）"}
	}
	review, err := renderTemplate(reviewTemplateName, TemplateData{
		Reviewer: options.Reviewer,
		Merger:   options.Merger,
		Image:    options.Image,
	})
	if err != nil {
		return Finding{Path: ManagedReviewPath(), Status: StatusOutdated, Detail: err.Error()}
	}
	if !strings.Contains(text, strings.TrimSpace(review)) {
		return Finding{Path: ManagedReviewPath(), Status: StatusOutdated, Detail: "托管段落与当前模板不一致"}
	}
	return Finding{Path: ManagedReviewPath(), Status: StatusOK}
}

// checkClaudeMD 检查 CLAUDE.md：缺导入行、被改写或用户自有。
func checkClaudeMD(options *Options) Finding {
	path := filepath.Join(options.Dir, ManagedClaudePath())
	existing, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return Finding{Path: ManagedClaudePath(), Status: StatusMissing, Detail: "未安装"}
	case err != nil:
		return Finding{Path: ManagedClaudePath(), Status: StatusMissing, Detail: err.Error()}
	case strings.Contains(string(existing), "@AGENTS.md"):
		return Finding{Path: ManagedClaudePath(), Status: StatusOK}
	case strings.Contains(string(existing), Marker):
		return Finding{Path: ManagedClaudePath(), Status: StatusOutdated, Detail: "内容与当前模板不一致"}
	default:
		return Finding{Path: ManagedClaudePath(), Status: StatusUnmanaged, Detail: "已存在用户自有 CLAUDE.md（未改动）"}
	}
}

// checkLegacyWorkflows 报告带 marker 的旧版 workflow（install 会清理）。
func checkLegacyWorkflows(options *Options) []Finding {
	var findings []Finding
	for _, relative := range LegacyWorkflowPaths() {
		existing, err := os.ReadFile(filepath.Join(options.Dir, relative))
		if err != nil || !strings.Contains(string(existing), Marker) {
			continue
		}
		findings = append(findings, Finding{
			Path: relative, Status: StatusLegacy, Detail: "旧版文件，重复 install 会移除",
		})
	}
	return findings
}

// managedSection 返回文件中 assistant 管理的段落（含 marker）。
func managedSection(content string) string {
	begin := strings.Index(content, "<!-- "+Marker)
	if begin < 0 {
		return ""
	}
	endMarker := "<!-- /" + Marker + " -->"
	end := strings.Index(content[begin:], endMarker)
	if end < 0 {
		return content[begin:]
	}
	return content[begin : begin+end+len(endMarker)]
}

// loadJSON 读取并解析 JSON 文件；found 区分「不存在」与「解析失败」。
func loadJSON(path string) (document map[string]any, found bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	parsed := map[string]any{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, true, err
	}
	return parsed, true, nil
}

func jsonStringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
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

func containsAll(values []string, wanted ...string) bool {
	for _, want := range wanted {
		found := false
		for _, value := range values {
			if value == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func checkClaude(options *Options) []Finding {
	findings := []Finding{
		checkClaudeMCP(options),
		checkClaudeSettings(options),
	}
	return findings
}

func checkClaudeMCP(options *Options) Finding {
	path := filepath.Join(options.Dir, ".mcp.json")
	document, found, err := loadJSON(path)
	switch {
	case !found:
		return Finding{Path: ".mcp.json", Status: StatusMissing, Detail: "未安装"}
	case err != nil:
		return Finding{Path: ".mcp.json", Status: StatusUnmanaged, Detail: "JSON 解析失败: " + err.Error()}
	}
	servers, _ := document["mcpServers"].(map[string]any)
	entry, _ := servers["gitea"].(map[string]any)
	if entry == nil {
		return Finding{Path: ".mcp.json", Status: StatusMissing, Detail: "缺少 gitea MCP 条目"}
	}
	if entry["command"] != "assistant" || !containsAll(jsonStringSlice(entry["args"]), "mcp", "gitea") {
		return Finding{Path: ".mcp.json", Status: StatusOutdated, Detail: "gitea MCP 条目与当前配置不一致"}
	}
	return Finding{Path: ".mcp.json", Status: StatusOK}
}

func checkClaudeSettings(options *Options) Finding {
	path := filepath.Join(options.Dir, ".claude", "settings.json")
	document, found, err := loadJSON(path)
	switch {
	case !found:
		return Finding{Path: ".claude/settings.json", Status: StatusMissing, Detail: "未安装"}
	case err != nil:
		return Finding{Path: ".claude/settings.json", Status: StatusUnmanaged, Detail: "JSON 解析失败: " + err.Error()}
	}
	var problems []string
	enabled, _ := document["enableAllProjectMcpServers"].(bool)
	if !enabled {
		problems = append(problems, "未启用项目 MCP")
	}
	permissions, _ := document["permissions"].(map[string]any)
	allow := jsonStringSlice(permissions["allow"])
	if missing := missingStrings(allow, claudecfg.Allow...); len(missing) > 0 {
		problems = append(problems, "缺少权限放行："+strings.Join(missing, ", "))
	}
	env, _ := document["env"].(map[string]any)
	var envMissing []string
	for key, value := range claudecfg.Env {
		if env[key] != value {
			envMissing = append(envMissing, key)
		}
	}
	slices.Sort(envMissing)
	if len(envMissing) > 0 {
		problems = append(problems, "环境变量缺失/不一致："+strings.Join(envMissing, ", "))
	}
	if len(problems) > 0 {
		return Finding{Path: ".claude/settings.json", Status: StatusOutdated, Detail: strings.Join(problems, "；")}
	}
	return Finding{Path: ".claude/settings.json", Status: StatusOK}
}

// missingStrings 返回 wanted 中不在 values 里的条目（保持 wanted 顺序）。
func missingStrings(values []string, wanted ...string) []string {
	var missing []string
	for _, want := range wanted {
		found := false
		for _, value := range values {
			if value == want {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, want)
		}
	}
	return missing
}

func checkOpenCode(options *Options) []Finding {
	path := filepath.Join(options.Dir, "opencode.json")
	document, found, err := loadJSON(path)
	switch {
	case !found:
		return []Finding{{Path: "opencode.json", Status: StatusMissing, Detail: "未安装"}}
	case err != nil:
		return []Finding{{Path: "opencode.json", Status: StatusUnmanaged, Detail: "JSON 解析失败: " + err.Error()}}
	}
	finding := Finding{Path: "opencode.json", Status: StatusOK}
	servers, _ := document["mcp"].(map[string]any)
	entry, _ := servers["gitea"].(map[string]any)
	permissions, _ := document["permission"].(map[string]any)
	switch {
	case entry == nil:
		finding.Status = StatusMissing
		finding.Detail = "缺少 gitea MCP 条目"
	case entry["type"] != "local" || entry["enabled"] != true ||
		!containsAll(jsonStringSlice(entry["command"]), "assistant", "mcp", "gitea"):
		finding.Status = StatusOutdated
		finding.Detail = "gitea MCP 条目与当前配置不一致"
	case permissions["gitea_*"] != "allow":
		finding.Status = StatusOutdated
		finding.Detail = "缺少 gitea_* 工具放行"
	}
	return []Finding{finding}
}

func checkCodex(options *Options) []Finding {
	display := options.CodexConfigPath
	data, err := os.ReadFile(display)
	switch {
	case os.IsNotExist(err):
		return []Finding{{Path: display, Status: StatusMissing, Detail: "未安装"}}
	case err != nil:
		return []Finding{{Path: display, Status: StatusMissing, Detail: err.Error()}}
	}
	content := string(data)
	switch {
	case strings.Contains(content, strings.TrimSpace(codexServerBlock)):
		return []Finding{{Path: display, Status: StatusOK}}
	case strings.Contains(content, "[mcp_servers.gitea]"):
		return []Finding{{Path: display, Status: StatusOutdated, Detail: "gitea MCP 段与当前配置不一致"}}
	default:
		return []Finding{{Path: display, Status: StatusMissing, Detail: "缺少 gitea MCP 段"}}
	}
}

// Summary 生成人类可读的单行摘要（CLI 之外也可用）。
func Summary(findings []Finding) string {
	problems := 0
	for _, finding := range findings {
		if !finding.OK() {
			problems++
		}
	}
	if problems == 0 {
		return "配置正常"
	}
	return fmt.Sprintf("%d 处配置问题", problems)
}
