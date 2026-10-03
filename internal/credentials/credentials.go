// Package credentials 管理用户级的平台实例；运行服务不读取这里的开发者登录态。
package credentials

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// File 按平台分组，每个平台的字段集独立且严格校验。
type File struct {
	Instances Instances `json:"instances"`
}

// Instances 是已支持平台的类型注册面，未知平台或字段均拒绝。
type Instances struct {
	Gitea    []Gitea    `json:"gitea,omitempty"`
	QQ       []QQ       `json:"qq,omitempty"`
	Weixin   []Weixin   `json:"weixin,omitempty"`
	Telegram []Telegram `json:"telegram,omitempty"`
}

// Gitea 保存一个站点上的个人令牌与加密登录密码；MCP 只使用令牌。
type Gitea struct {
	Name              string `json:"name"`
	URL               string `json:"url"`
	Username          string `json:"username"`
	Token             string `json:"token"`
	EncryptedPassword string `json:"password,omitempty"`
}

// QQ 保存开放平台的应用身份。
type QQ struct {
	Name      string `json:"name"`
	AppID     string `json:"app-id"`
	AppSecret string `json:"app-secret"`
}

// Weixin 保存扫码换得的身份与平台端点。
type Weixin struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	UserID string `json:"user-id"`
	BotID  string `json:"bot-id"`
	Token  string `json:"token"`
}

// Telegram 保存 BotFather 颁发并实测过的机器人令牌。
type Telegram struct {
	Name     string `json:"name"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

// Path 使用平台标准用户目录，从任何项目启动都指向同一份凭据。
func Path() (string, error) {
	if path := os.Getenv("ASSISTANT_CREDENTIALS"); path != "" {
		return path, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("定位用户凭据目录: %w", err)
	}
	return filepath.Join(dir, "Cosmic-Developers-Union", "assistant", "credentials.json"), nil
}

// Load 缺失文件返回空实例库，损坏或旧格式不会被静默覆盖。
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return new(File), nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取凭据: %w", err)
	}
	file := new(File)
	if err := json.Unmarshal(data, file, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("解析凭据（旧版凭据需重新登记实例）: %w", err)
	}
	if err := file.Validate(); err != nil {
		return nil, err
	}
	return file, nil
}

// Validate 拒绝重复名字、无效端点与缺失密钥，错误不回显令牌。
func (f *File) Validate() error {
	names := map[string]bool{}
	nameOK := func(name string) error {
		if strings.TrimSpace(name) == "" || names[name] {
			return fmt.Errorf("实例名为空或重复：%q", name)
		}
		names[name] = true
		return nil
	}
	for _, item := range f.Instances.Gitea {
		if err := nameOK(item.Name); err != nil {
			return err
		}
		if err := ValidateHost(item.URL); err != nil {
			return err
		}
		if item.Username == "" || item.Token == "" {
			return fmt.Errorf("gitea 实例缺少 username/token")
		}
		if item.EncryptedPassword != "" {
			if _, err := item.PasswordValue(); err != nil {
				return fmt.Errorf("gitea 实例 %s: %w", item.Name, err)
			}
		}
	}
	for _, item := range f.Instances.QQ {
		if err := nameOK(item.Name); err != nil {
			return err
		}
		if item.AppID == "" || item.AppSecret == "" {
			return fmt.Errorf("qq 实例缺少 app-id/app-secret")
		}
	}
	for _, item := range f.Instances.Weixin {
		if err := nameOK(item.Name); err != nil {
			return err
		}
		if err := ValidateHost(item.URL); err != nil {
			return err
		}
		if item.Token == "" || item.UserID == "" || item.BotID == "" {
			return fmt.Errorf("weixin 实例缺少身份或 token")
		}
	}
	for _, item := range f.Instances.Telegram {
		if err := nameOK(item.Name); err != nil {
			return err
		}
		if item.Token == "" || item.Username == "" {
			return fmt.Errorf("telegram 实例缺少 username/token")
		}
	}
	return nil
}

// Save 用原子替换和 0600 防止半份凭据或其他用户读取。
func Save(path string, file *File) error {
	if err := file.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建凭据目录: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// NormalizeHost 让地址末尾斜杠不影响实例匹配。
func NormalizeHost(host string) string { return strings.TrimRight(strings.TrimSpace(host), "/") }

// Hosts 返回凭据库已登记的唯一站点，不把多账号误算成多站点。
func (f *File) Hosts() []string {
	var hosts []string
	seen := map[string]bool{}
	for _, entry := range f.Instances.Gitea {
		host := NormalizeHost(entry.URL)
		if !seen[host] {
			hosts = append(hosts, host)
			seen[host] = true
		}
	}
	return hosts
}

// GiteaToken 要求一个站点唯一账号，多账号时明确拒绝猜测。
func (f *File) GiteaToken(host string) (string, error) {
	token := ""
	for _, entry := range f.Instances.Gitea {
		if NormalizeHost(entry.URL) == NormalizeHost(host) {
			if token != "" {
				return "", fmt.Errorf("该站点登记了多个账号，请显式指定令牌")
			}
			token = entry.Token
		}
	}
	return token, nil
}
