// gitea 集成自己的 git 读取助手：解析 remote 需要读 .git/config，而通用的
// 调度引擎（internal/dispatcher）不对外暴露它的 git 封装——gitea 包不反向依赖
// 调度引擎，这点与平台包不依赖 daemon 同理。
package gitea

import (
	"fmt"
	"os/exec"
	"strings"
)

// runGit 在 repoDir 里跑一条 git 命令，返回 stdout。
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
