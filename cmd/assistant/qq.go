package main

import (
	"context"
	"fmt"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/envref"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/qq"

	"github.com/spf13/cobra"
)

// newQQCommand 管理 QQ 对话桥：凭据是开放平台控制台的 AppID/AppSecret（手工
// 写入 config.json 的 channels 列表，没有扫码登录流程），所以这里只提供状态
// 与自检。
func newQQCommand(configFlag *string) *cobra.Command {
	command := &cobra.Command{
		Use:   "qq",
		Short: "QQ 对话桥（开放平台 Bot API v2）：配置状态与凭据自检",
		Long: "QQ 通道凭据来自 q.qq.com 开放平台：创建机器人后把 AppID/AppSecret 写入\n" +
			"config.json 的 channels 列表（{\"type\":\"qq\",\"app_id\":...,\"app_secret\":...}），\n" +
			"之后 assistant run 即可通过 QQ 群/@ 与私聊和 daemon 对话。本命令只读配置\n" +
			"并实测凭据，不做任何写入。",
		Args: cobra.NoArgs,
	}
	command.AddCommand(newQQStatusCommand(configFlag))
	return command
}

// newQQStatusCommand 显示 channels 里全部 QQ 实例的状态（不显示密钥）并逐个
// 实测换取 access token（app_secret 的 $VAR 引用先展开）。
func newQQStatusCommand(configFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "显示 QQ 通道配置状态（不显示密钥），并实测 AppID/AppSecret",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			_, file, err := resolveInstanceFile(commandOptions{ConfigPath: *configFlag})
			if err != nil {
				return err
			}
			stdout := command.OutOrStdout()
			if file == nil {
				fmt.Fprintln(stdout, "未配置 QQ 通道（在 config.json 的 channels 列表加入 {\"type\":\"qq\", ...} 条目）")
				return nil
			}
			configs := qqChannelViews(file)
			if len(configs) == 0 {
				fmt.Fprintln(stdout, "未配置 QQ 通道（在 config.json 的 channels 列表加入 {\"type\":\"qq\", ...} 条目）")
				return nil
			}
			failed := false
			for _, view := range configs {
				fmt.Fprintf(stdout, "key=%s enabled=%v api_base_url=%s app_id=%s secret=%s admins=%d split_limit=%d\n",
					view.Key, view.Enabled, view.APIBaseURL, orDash(view.AppID), tokenState(view.AppSecret),
					len(view.AdminUsers), view.SplitLimit)
				if err := verifyQQ(view, stdout); err != nil {
					failed = true
					fmt.Fprintf(stdout, "凭据自检失败（%s）：%v\n", view.Key, err)
				}
			}
			if failed {
				return fmt.Errorf("存在凭据自检失败的 QQ 通道")
			}
			return nil
		},
	}
}

// qqChannelView 是 qq status 的展示条目（通道键 + 展开后的生效字段）。
type qqChannelView struct {
	Key        string
	Enabled    bool
	APIBaseURL string
	AppID      string
	AppSecret  string
	AdminUsers []string
	SplitLimit int
}

// qqChannelViews 列出 channels 里的 qq 实例（app_secret 经环境变量引用展开）。
func qqChannelViews(file *instances.File) []qqChannelView {
	var views []qqChannelView
	for _, channel := range file.Channels {
		if channel.Type != instances.ChannelQQ {
			continue
		}
		appID := channel.AppID
		if expanded, err := expandQQAppID(channel); err == nil {
			appID = expanded
		}
		secret := channel.AppSecret
		if field, value := channel.SecretField(); field == "app_secret" {
			if expanded, err := envref.Expand(value, envref.Options{
				Field: fmt.Sprintf("channels[%s].app_secret", channel.Key()),
			}); err == nil {
				secret = expanded
			}
			// 展开失败保留原值：自检会失败并给出原因
		}
		views = append(views, qqChannelView{
			Key:        channel.Key(),
			Enabled:    channel.IsEnabled(),
			APIBaseURL: channel.APIBaseURL,
			AppID:      appID,
			AppSecret:  secret,
			AdminUsers: channel.AdminUsers,
			SplitLimit: channel.SplitLimit,
		})
	}
	return views
}

func expandQQAppID(channel instances.Channel) (string, error) {
	return envref.Expand(channel.AppID, envref.Options{
		Field: fmt.Sprintf("channels[%s].app_id", channel.Key()),
	})
}

// verifyQQ 实测单个 QQ 通道的凭据（AppSecret 已展开）。
func verifyQQ(view qqChannelView, stdout interface{ Write([]byte) (int, error) }) error {
	if view.AppID == "" || view.AppSecret == "" {
		stdout.Write([]byte("凭据不完整：跳过自检（app_id 与 app_secret 都需要）\n"))
		return nil
	}
	client := qq.NewClient(qq.Config{
		AppID:      view.AppID,
		AppSecret:  view.AppSecret,
		APIBaseURL: view.APIBaseURL,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.VerifyCredential(ctx); err != nil {
		return err
	}
	stdout.Write([]byte("凭据自检：通过（access token 获取成功）\n"))
	return nil
}
