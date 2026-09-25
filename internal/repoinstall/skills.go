package repoinstall

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Cosmic-Developers-Union/assistant/skills"
)

// skillsAgents 把内部工具名映射为 skills CLI 的 agent 名（去重）。
func skillsAgents(tools []string) []string {
	agents := make([]string, 0, len(tools))
	seen := map[string]bool{}
	for _, tool := range tools {
		agent := ""
		switch tool {
		case "claude":
			agent = "claude-code"
		case "opencode":
			agent = "opencode"
		case "codex":
			agent = "codex"
		}
		if agent != "" && !seen[agent] {
			seen[agent] = true
			agents = append(agents, agent)
		}
	}
	return agents
}

// installSkills 调 skills CLI 安装/更新 review 技能（幂等）。
func installSkills(options *Options) error {
	if options.SkillsSource == "" || options.SkillsSource == "none" {
		options.Log("跳过技能安装（--skills-source none）")
		return nil
	}
	agents := skillsAgents(options.Tools)
	if len(agents) == 0 {
		return nil
	}
	request := SkillsRequest{Dir: options.Dir, Source: options.SkillsSource, Agents: agents}
	if options.DryRun {
		options.Log("dry-run：bunx skills add %s --skill review --copy -a %s -y",
			request.Source, strings.Join(agents, " -a "))
		return nil
	}
	if err := options.RunSkills(request); err != nil {
		return fmt.Errorf("安装 review 技能失败（bunx skills add %s --skill review）: %w", request.Source, err)
	}
	options.Log("已安装 review 技能（%s）", strings.Join(agents, ", "))
	return nil
}

// uninstallSkills 调 skills CLI 移除 review 技能；失败只提示（避免卸载被阻断）。
func uninstallSkills(options *Options) {
	if options.SkillsSource == "" || options.SkillsSource == "none" {
		return
	}
	agents := skillsAgents(options.Tools)
	if len(agents) == 0 {
		return
	}
	if options.DryRun {
		options.Log("dry-run：bunx skills remove review -a %s -y", strings.Join(agents, " -a "))
		return
	}
	if err := options.RunSkills(SkillsRequest{Dir: options.Dir, Agents: agents, Remove: true}); err != nil {
		options.Log("移除 review 技能失败（可手动 bunx skills remove review）: %v", err)
	}
}

// removeMarkerFile 删除带 marker 的旧版直接写入文件（迁移到 skills CLI 用）。
func removeMarkerFile(options *Options, path string) {
	existing, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(existing), Marker) {
		return
	}
	options.remove(path)
}

// runSkillsCLI 执行 bunx skills（缺省 RunSkills）：add 用 --copy 生成真实文件，
// 便于提交与在容器/CI 中复现。
func runSkillsCLI(request SkillsRequest) error {
	args := []string{"skills"}
	if request.Remove {
		args = append(args, "remove", "review")
	} else {
		args = append(args, "add", request.Source, "--skill", "review", "--copy")
	}
	for _, agent := range request.Agents {
		args = append(args, "-a", agent)
	}
	args = append(args, "-y")
	command := exec.Command("bunx", args...)
	command.Dir = request.Dir
	output, err := command.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return fmt.Errorf("执行 bunx: %w", err)
		}
		return fmt.Errorf("执行 bunx: %w: %s", err, trimmed)
	}
	return nil
}

// skillsFindings 检查各 CLI 的技能安装状态（内容对照仓库内 skills/review）。
func skillsFindings(options *Options) []Finding {
	var findings []Finding
	seen := map[string]bool{}
	for _, tool := range options.Tools {
		relative, ok := skillPathForTool(tool)
		if !ok || seen[relative] {
			continue
		}
		seen[relative] = true
		findings = append(findings, checkSkillFile(options, relative))
	}
	return findings
}

func checkSkillFile(options *Options, relative string) Finding {
	path := filepath.Join(options.Dir, relative)
	existing, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return Finding{Path: relative, Status: StatusMissing, Detail: "未安装（assistant install）"}
	case err != nil:
		return Finding{Path: relative, Status: StatusMissing, Detail: err.Error()}
	case strings.TrimSpace(string(existing)) == strings.TrimSpace(skills.Review):
		return Finding{Path: relative, Status: StatusOK}
	default:
		return Finding{Path: relative, Status: StatusOutdated, Detail: "技能内容与当前版本不一致（assistant install 可更新）"}
	}
}
