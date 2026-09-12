// Gitea 站点与仓库的自动检测：解析宿主检出的 origin remote URL。
//
// http(s) remote（如 http://gitea.example.com:3000/owner/repo.git）
// 可完整推出 API 根地址与 owner/repo。ssh/scp remote 只可靠推出仓库路径——
// ssh 端口与 web 端口没有对应关系，host 仅以 http://<主机名[:端口]> 尽力猜测，
// 此时需要 --host / GITEA_HOST 显式覆盖。
package dispatcher

import (
	"os/exec"
	"regexp"
	"strings"
)

// GitRemote 是从 remote URL 推导出的站点与仓库。
type GitRemote struct {
	// Host 是 Gitea API 根地址（scheme://host[:port]，无尾斜杠）
	Host string
	// Repository 是 owner/repo
	Repository string
}

// 带协议：proto://[user@]host[:port]/owner/repo[.git]。https 保留 https，
// 其余（git/ssh/git+ssh）API 一律按 http 站点处理。
var schemefulRemote = regexp.MustCompile(`(?i)^(https?|git|ssh|git\+ssh)://(?:[^/@]+@)?([^/:]+)(?::(\d+))?/(.+?)(?:\.git)?/?$`)

// scp 形态：[user@]host[:port]:owner/repo[.git]（数字端口变体尽力支持）
var scpRemote = regexp.MustCompile(`^(?:[^@/]+@)?([^/:]+)(?::(\d+))?:([^/].+?)(?:\.git)?/?$`)

// ParseGitRemoteURL 把 remote URL 折成 host + 仓库；无法识别时返回 false。
func ParseGitRemoteURL(url string) (GitRemote, bool) {
	trimmed := strings.TrimSpace(url)
	if match := schemefulRemote.FindStringSubmatch(trimmed); match != nil {
		proto, hostname, port, path := match[1], match[2], match[3], match[4]
		scheme := "http"
		if strings.EqualFold(proto, "https") {
			scheme = "https"
		}
		host := scheme + "://" + hostname
		if port != "" {
			host += ":" + port
		}
		return GitRemote{Host: host, Repository: strings.TrimSuffix(path, ".git")}, true
	}
	if match := scpRemote.FindStringSubmatch(trimmed); match != nil {
		hostname, port, path := match[1], match[2], match[3]
		host := "http://" + hostname
		if port != "" {
			host += ":" + port
		}
		return GitRemote{Host: host, Repository: strings.TrimSuffix(path, ".git")}, true
	}
	return GitRemote{}, false
}

// OriginRemote 读取 repoDir 的 origin remote URL；不在 git 检出内或无 origin
// 时返回 false。
func OriginRemote(repoDir string) (string, bool) {
	output, err := exec.Command("git", "-C", repoDir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return "", false
	}
	url := strings.TrimSpace(string(output))
	return url, url != ""
}

// RepoRoot 解析 dir 所在 git 检出的仓库根（dir 可为检出内任意子目录）；非检出
// 时返回 false。
func RepoRoot(dir string) (string, bool) {
	output, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}
	root := strings.TrimSpace(string(output))
	return root, root != ""
}
