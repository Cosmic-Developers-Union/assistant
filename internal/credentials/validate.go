package credentials

import (
	"fmt"
	"net/url"
	"strings"
)

// ValidateHost 只允许无内嵌凭据的 HTTP(S) 平台地址。
func ValidateHost(host string) error {
	parsed, err := url.Parse(host)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("平台地址必须是没有凭据或查询参数的 HTTP(S) URL")
	}
	return nil
}

// ParseRepoName 校验 owner/name，避免把平台输入拼成越界路径。
func ParseRepoName(raw string) (string, string, error) {
	owner, name, ok := strings.Cut(raw, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(raw, "\\\x00") || owner == "." || owner == ".." || name == "." || name == ".." || strings.ContainsAny(raw, " \t\r\n") {
		return "", "", fmt.Errorf("仓库必须使用 owner/name 格式")
	}
	return owner, name, nil
}
