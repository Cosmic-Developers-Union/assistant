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
	"sort"
	"strings"
)

// GitRemote 是从 remote URL 推导出的站点与仓库。
type GitRemote struct {
	// Name 是 remote 名（如 origin / upstream）。
	Name string
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

// ListRemotes 解析 repoDir 的全部 remote（跳过无法解析的），origin 排在最前。
func ListRemotes(repoDir string) []GitRemote {
	output, err := runGit(repoDir, "remote")
	if err != nil {
		return nil
	}
	var remotes []GitRemote
	for _, name := range strings.Fields(output) {
		url, err := runGit(repoDir, "config", "--get", "remote."+name+".url")
		if err != nil {
			continue
		}
		parsed, ok := ParseGitRemoteURL(strings.TrimSpace(url))
		if !ok {
			continue
		}
		parsed.Name = name
		remotes = append(remotes, parsed)
	}
	sort.SliceStable(remotes, func(i, j int) bool {
		return remotes[i].Name == "origin" && remotes[j].Name != "origin"
	})
	return remotes
}

// SelectGiteaRemote 在多个 remote 中选出 Gitea 站点：origin 优先，其次按顺序
// 探测（GitHub/GitLab 等同名 version 端点不匹配会被跳过）。探测都没命中时仍
// 返回 origin 或第一个可解析 remote（ok=false），调用方可在显式 host 场景复用
// 其仓库路径。
func SelectGiteaRemote(repoDir string, probe func(host string) bool) (GitRemote, bool) {
	remotes := ListRemotes(repoDir)
	if len(remotes) == 0 {
		return GitRemote{}, false
	}
	if probe != nil {
		for _, remote := range remotes {
			if probe(remote.Host) {
				return remote, true
			}
		}
	}
	return remotes[0], false
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
