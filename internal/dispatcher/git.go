// 评审会话的 git 环境准备：宿主检出的基线镜像同步，与按 PR head 建独立
// worktree（detach）。评审标准（.claude/）一律取自基线镜像——worktree 里
// PR 自带的版本会被覆盖，PR 无从改弱自己被审的规则。会话结束后移除
// worktree；会话的 cwd 指向 worktree，评审实验（go run 等）不污染主检出。
package dispatcher

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func runGit(repoDir string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
	}
	return stdout.String(), nil
}

// SyncMirror 把宿主检出强制对齐 origin 基线分支：fetch --prune → checkout -f -B
// → clean -fd。检出是评审标准（.claude/）与 Issue 分诊会话的数据源，必须精确
// 镜像基线——本地任何分叉（提交、切分支、未跟踪杂物）一律丢弃；clean 不带 -x，
// .gitignore 豁免的本地产物（node_modules/logs/lock）不受影响。checkout 用 -f
// 是为了让脏树也能完成切换，不用先人工收拾。返回对齐后的基线短 sha。
func SyncMirror(repoDir, baseBranch string) (string, error) {
	if _, err := runGit(repoDir, "fetch", "--prune", "origin"); err != nil {
		return "", err
	}
	if _, err := runGit(repoDir, "checkout", "-f", "-B", baseBranch, "origin/"+baseBranch); err != nil {
		return "", err
	}
	if _, err := runGit(repoDir, "clean", "-fd"); err != nil {
		return "", err
	}
	stdout, err := runGit(repoDir, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// PinStandard 用宿主检出的 .claude/ 整体替换 worktree 的同名目录。worktree 按
// PR head 检出，随带的 .claude 是 PR 自己的版本——照单全收等于允许 PR 改弱
// 自己被审的规则（甚至自带放行权限的 settings）。宿主检出缺 .claude/ 时抛错
// （fail-closed：宁可不起会话，不可无标准评审）。
func PinStandard(repoDir, worktreeDir string) error {
	source := filepath.Join(repoDir, ".claude")
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("宿主检出缺少 .claude/（评审标准来源，应锚定基线分支）：%s", source)
	}
	if err := os.RemoveAll(filepath.Join(worktreeDir, ".claude")); err != nil {
		return err
	}
	return copyDir(source, filepath.Join(worktreeDir, ".claude"))
}

// PrepareWorktree 拉取 PR head 并在 worktreeDir 建 detach 检出。
// Gitea 为每个 PR 暴露 refs/pull/<n>/head（跨 fork 一律存在），fetch 后以
// FETCH_HEAD 建检出；目录已存在（上次崩溃残留）时先强制移除再重建。
// 检出后立即以宿主检出的 .claude/ 覆盖（评审标准锚定基线，见 PinStandard）。
func PrepareWorktree(repoDir string, pullNumber int64, worktreeDir string) (string, error) {
	ref := fmt.Sprintf("refs/pull/%d/head", pullNumber)
	if _, err := runGit(repoDir, "fetch", "--quiet", "origin", ref); err != nil {
		return "", err
	}
	stdout, err := runGit(repoDir, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", err
	}
	headSHA := strings.TrimSpace(stdout)
	if err := RemoveWorktree(repoDir, worktreeDir); err != nil {
		return "", err
	}
	if _, err := runGit(repoDir, "worktree", "add", "--quiet", "--detach", worktreeDir, headSHA); err != nil {
		return "", err
	}
	if err := PinStandard(repoDir, worktreeDir); err != nil {
		return "", err
	}
	return headSHA, nil
}

// RemoveWorktree 移除 worktree；目录不存在或已是残留脏态时静默收敛。
func RemoveWorktree(repoDir, worktreeDir string) error {
	if _, err := runGit(repoDir, "worktree", "remove", "--force", worktreeDir); err != nil {
		// 目录不存在（首次运行）或已被清理——统一 prune 收敛注册表
		_, _ = runGit(repoDir, "worktree", "prune")
		return nil
	}
	return nil
}

// copyDir 递归复制目录（保留符号链接本身，不解引用）。
func copyDir(source, destination string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.Name())
		destinationPath := filepath.Join(destination, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(sourcePath)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, destinationPath); err != nil {
				return err
			}
		case entry.IsDir():
			if err := copyDir(sourcePath, destinationPath); err != nil {
				return err
			}
		default:
			if err := copyFile(sourcePath, destinationPath, info.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}
