// 评审会话的 git 环境准备：宿主检出的基线镜像同步，与按 PR head 建独立
// worktree（detach）。评审标准（.claude/）一律取自基线镜像——worktree 里
// PR 自带的版本会被覆盖，PR 无从改弱自己被审的规则。会话结束后移除
// worktree；会话的 cwd 指向 worktree，评审实验（go run 等）不污染主检出。
package dispatcher

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func runGit(repoDir string, args ...string) (string, error) {
	return runGitEnv(repoDir, nil, args...)
}

// runGitEnv 是带额外环境变量的 git 调用：受管克隆用 gitTokenEnv 注入站点令牌
// （http.extraHeader），凭据不写入 .git/config。
func runGitEnv(repoDir string, env []string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
	command.Env = append(os.Environ(), env...)
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

// gitTokenEnv 让 git 子进程以站点令牌认证（Authorization header），只作用于
// 本次命令：令牌不落 .git/config，也不出现在 URL/进程参数里。
//
// scheme 用 Basic 而不是 token：Gitea 的 git smart HTTP 两种都认，但 LFS 端点
// （objects/batch）只认 Basic——token scheme 会让 git-lfs 的对象下载 401，
// 克隆时 smudge 失败（克隆成功、检出失败）。Basic 的用户名占位 oauth2：
// Gitea 校验的是密码位令牌，用户名任意。
func gitTokenEnv(token string) []string {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	basic := base64.StdEncoding.EncodeToString([]byte("oauth2:" + token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
		"GIT_TERMINAL_PROMPT=0",
	}
}

// EnsureRepo 确保受管克隆存在：目录里没有 .git 时按 host/owner/name 克隆
// （origin 保持无凭据 URL，fetch 时按需注入令牌），返回是否新建。已存在时不
// 做任何同步（由 run 循环的 SyncMirror 负责，保持与 origin/<base> 一致）。
func EnsureRepo(dir, host, fullName, token string) (bool, error) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return false, nil
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return false, fmt.Errorf("受管克隆落点已被占用（非 git 检出）：%s", dir)
	}
	url := strings.TrimRight(strings.TrimSpace(host), "/") + "/" + fullName + ".git"
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return false, fmt.Errorf("创建克隆目录: %w", err)
	}
	command := exec.Command("git", "clone", "--quiet", url, dir)
	command.Env = append(os.Environ(), gitTokenEnv(token)...)
	var stderr strings.Builder
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return false, fmt.Errorf("克隆 %s: %s", url, message)
	}
	return true, nil
}

// SyncMirror 把宿主检出强制对齐 origin 基线分支：fetch --prune → checkout -f -B
// → clean -fd。检出是评审标准（.claude/）与 Issue 分诊会话的数据源，必须精确
// 镜像基线——本地任何分叉（提交、切分支、未跟踪杂物）一律丢弃；clean 不带 -x，
// .gitignore 豁免的本地产物（node_modules/logs/lock）不受影响。checkout 用 -f
// 是为了让脏树也能完成切换，不用先人工收拾。返回对齐后的基线短 sha。
func SyncMirror(repoDir, baseBranch, token string) (string, error) {
	env := gitTokenEnv(token)
	if _, err := runGitEnv(repoDir, env, "fetch", "--prune", "origin"); err != nil {
		return "", err
	}
	if _, err := runGitEnv(repoDir, env, "checkout", "-f", "-B", baseBranch, "origin/"+baseBranch); err != nil {
		return "", err
	}
	if _, err := runGitEnv(repoDir, env, "clean", "-fd"); err != nil {
		return "", err
	}
	stdout, err := runGit(repoDir, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

// SingleFlightMirror 把 SyncMirror 包成「同仓库互斥 + 窗口内单飞」：一轮检测
// 的多个待办并发起会话时（ProcessItem 各自在会话前同步基线），第一个真正
// 执行 fetch/checkout，其余等锁后复用同一结果——避免并发 git 命令互撞
// .git/index.lock（受管克隆是同仓库唯一检出）。窗口取检测间隔：一轮至多
// 同步一次；同步失败同样复用到窗口结束（fail-closed：基线不新鲜时整轮跳过，
// 下一轮重新同步）。
func SingleFlightMirror(window time.Duration, mirror func() (string, error)) func() (string, error) {
	var mutex sync.Mutex
	var sha string
	var failure error
	var syncedAt time.Time
	return func() (string, error) {
		mutex.Lock()
		defer mutex.Unlock()
		if !syncedAt.IsZero() && time.Since(syncedAt) < window {
			return sha, failure
		}
		sha, failure = mirror()
		syncedAt = time.Now()
		return sha, failure
	}
}
// PinStandard 用宿主检出的 .claude/ 整体替换 worktree 的同名目录。worktree 按
// PR head 检出，随带的 .claude 是 PR 自己的版本——照单全收等于允许 PR 改弱
// 自己被审的规则（甚至自带放行权限的 settings），所以一律先删掉：基线有
// .claude/ 就覆盖上去，没有就保持「worktree 里没有 .claude/」——会话的
// settings、MCP 与评审协议都由 assistant 注入，仓库没装脚手架不构成起会话的
// 门槛（缺的不是标准，只是仓库自己的项目级调优）。
func PinStandard(repoDir, worktreeDir string) error {
	source := filepath.Join(repoDir, ".claude")
	target := filepath.Join(worktreeDir, ".claude")
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	if _, err := os.Stat(source); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return copyDir(source, target)
}

// PrepareWorktree 拉取 PR head 并在 worktreeDir 建 detach 检出。
// Gitea 为每个 PR 暴露 refs/pull/<n>/head（跨 fork 一律存在），fetch 后以
// FETCH_HEAD 建检出；目录已存在（上次崩溃残留）时先强制移除再重建。
// 检出后立即以宿主检出的 .claude/ 覆盖（评审标准锚定基线，见 PinStandard）。
func PrepareWorktree(repoDir string, pullNumber int64, worktreeDir, token string) (string, error) {
	ref := fmt.Sprintf("refs/pull/%d/head", pullNumber)
	if _, err := runGitEnv(repoDir, gitTokenEnv(token), "fetch", "--quiet", "origin", ref); err != nil {
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

// PrepareBaselineWorktree 以基线分支的远端 head 建 detach worktree（Issue
// 分诊会话的隔离工作区，与 PR 会话同口径：workspace 在 /tmp，基线检出
// 只被 fetch/worktree 命令触碰）。评审标准同样锚定宿主基线（PinStandard）。
func PrepareBaselineWorktree(repoDir, baseBranch, worktreeDir, token string) (string, error) {
	if _, err := runGitEnv(repoDir, gitTokenEnv(token), "fetch", "--quiet", "origin", baseBranch); err != nil {
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
