package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunManagerActionEnvModeHonoursOptionalTokens 断言 env 模式（无配置文件）下，
// 独立的分支保护令牌与状态评审令牌确实被接线到状态客户端上，而且 --repo / 环境变量
// 里的仓库名当得起语法检查。
//
// 这条路径覆盖的是「裸跑 CI」——只有 GITEA_HOST / GITEA_ACCESS_TOKEN 时：
//   - GITEA_BRANCH_PROTECTION_TOKEN 必须让客户端改用独立令牌（Actions 内置令牌
//     拿不到分支保护端点），配了却没接线等于门禁读了假数据；
//   - GITEA_STATE_TOKEN 决定驳回是否算 official review，同样必须接线；
//   - GITEA_REPOSITORY 写错（少一段）时要在本地就报错，而不是发一个必然 404 的请求
//     出去，让操作者去 Gitea 日志里找原因。
func TestRunManagerActionEnvModeHonoursOptionalTokens(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fake := newFakeGitea(t)
	fake.login["base-token"] = "ai"
	fake.login["protection-token"] = "ai"
	fake.login["state-token"] = "ai"
	fake.repositories = append(fake.repositories, "acme/rocket")

	t.Setenv("GITEA_HOST", fake.server.URL)
	t.Setenv("GITEA_ACCESS_TOKEN", "base-token")
	t.Setenv("GITEA_BRANCH_PROTECTION_TOKEN", "protection-token")
	t.Setenv("GITEA_STATE_TOKEN", "state-token")
	t.Setenv("GITEA_REPOSITORY", "acme/rocket")

	var logs strings.Builder
	if err := runLabelSync(t.Context(), io.Discard, &logs, commandOptions{Repository: "acme/rocket", Verbose: true}); err != nil {
		t.Fatalf("env 模式 + 独立令牌下 label-sync 应能跑通：%v", err)
	}
	// 独立令牌必须真的接线：两个都用上了就在 verbose 日志里各留一行，否则
	// 「配了却没接线」的回归只能靠站点行为侧写才能发现。
	for _, want := range []string{
		"GITEA_BRANCH_PROTECTION_TOKEN",
		"GITEA_STATE_TOKEN",
		"认证通过",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("verbose 日志应出现 %q（独立令牌是否接线）：\n%s", want, logs.String())
		}
	}

	// 仓库名语法坏掉：必须在本地挡住，站点的请求日志里不该出现这条路径。
	logs.Reset()
	err := runLabelSync(t.Context(), io.Discard, &logs, commandOptions{Repository: "acme"})
	if err == nil {
		t.Fatalf("仓库名少了 owner/repo 的一段时应报错：\n%s", logs.String())
	}
	if !strings.Contains(err.Error(), "owner/name") {
		t.Errorf("错误应点明是仓库名格式的问题：%v", err)
	}
}

// TestRunManagerActionEnvModeLoadsDotEnvFile 断言工作目录里有 .env 时它被读入，
// 没有时退回进程环境变量，两种情况都不算失败。
//
// .env 是「本机凭证不进 shell 历史」的日常用法：静默忽略它会让操作者以为配置已生效，
// 实际所有请求都带着空令牌发出去。这里同时钉住「verbose 日志说明用了哪一份」——
// 配置来源必须可观测，否则排查鉴权失败时分不清读的是哪个文件。
func TestRunManagerActionEnvModeLoadsDotEnvFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fake := newFakeGitea(t)
	fake.login["base-token"] = "ai"
	fake.repositories = append(fake.repositories, "acme/rocket")

	dir := t.TempDir()
	dotEnv := filepath.Join(dir, ".env")
	writeFile(t, dotEnv, "GITEA_HOST="+fake.server.URL+"\nGITEA_ACCESS_TOKEN=base-token\nGITEA_REPOSITORY=acme/rocket\n")
	t.Chdir(dir)

	// 进程环境里故意不放这些值：能否跑通完全取决于 .env 有没有被载入。
	var logs strings.Builder
	if err := runLabelSync(t.Context(), io.Discard, &logs, commandOptions{Verbose: true}); err != nil {
		t.Fatalf("应从 .env 读到站点配置：%v", err)
	}
	if !strings.Contains(logs.String(), dotEnv) {
		t.Errorf("verbose 日志应说明载入了 %s：\n%s", dotEnv, logs.String())
	}
}

// writeFile 在给定路径写入内容，失败即终止用例。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
