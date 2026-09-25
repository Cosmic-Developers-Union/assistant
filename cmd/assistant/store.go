package main

import (
	"fmt"

	"assistant/internal/credentials"
)

// 凭据库是唯一的令牌来源：所有命令都通过这里按 (host, purpose) 取令牌。
// config.json 只描述实例与仓库。

// credentialFor 解析 (host, purpose) 的令牌；该用途没有登录过时 ok=false。
func credentialFor(configPath, host, purpose string) (credentials.Credential, bool, error) {
	path, err := credentials.Path()
	if err != nil {
		return credentials.Credential{}, false, err
	}
	store, err := credentials.Load(path)
	if err != nil {
		return credentials.Credential{}, false, err
	}
	return store.CredentialFor(host, purpose)
}

// tokenForPurpose 同 credentialFor，但缺失时返回带行动指引的错误。
func tokenForPurpose(configPath, host, purpose string) (credentials.Credential, error) {
	credential, ok, err := credentialFor(configPath, host, purpose)
	if err != nil {
		return credentials.Credential{}, err
	}
	if !ok {
		return credentials.Credential{}, fmt.Errorf("站点 %s 缺少 %s 用途令牌：%s", host, purpose, missingTokenHint(host, purpose))
	}
	return credential, nil
}

// missingTokenHint 给出补凭据的具体命令。
func missingTokenHint(host, purpose string) string {
	switch purpose {
	case credentials.PurposeAdmin:
		return "用管理员账号运行 assistant login add " + host + " --user <管理员账号>"
	case credentials.PurposeReview, credentials.PurposeMerge:
		return "运行 assistant setup --host " + host + "（机器人账号与令牌由 setup 创建）"
	default:
		return "运行 assistant login add " + host + " --user <账号>"
	}
}
