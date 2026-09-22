package main

import (
	"context"
	"fmt"
	"time"

	"assistant/internal/qq"

	"github.com/spf13/cobra"
)

// newQQCommand 管理 QQ 对话桥：凭据是开放平台控制台的 AppID/AppSecret（手工
// 写入 config.json 的 qq 节，没有扫码登录流程），所以这里只提供状态与自检。
func newQQCommand(configFlag *string) *cobra.Command {
	command := &cobra.Command{
		Use:   "qq",
		Short: "QQ 对话桥（开放平台 Bot API v2）：配置状态与凭据自检",
		Long: "QQ 通道凭据来自 q.qq.com 开放平台：创建机器人后把 AppID/AppSecret 写入\n" +
			"config.json 的 qq 节（enabled=true），之后 assistant run 即可通过 QQ 群/@\n" +
			"与私聊和 daemon 对话。本命令只读配置并实测凭据，不做任何写入。",
		Args: cobra.NoArgs,
	}
	command.AddCommand(newQQStatusCommand(configFlag))
	return command
}

// newQQStatusCommand 显示 QQ 通道配置状态（不显示密钥）并实测换取 access token。
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
			if file == nil || file.QQ == nil {
				fmt.Fprintln(stdout, "未配置 QQ 通道（在 config.json 的 qq 节填入 q.qq.com 开放平台的 app_id/app_secret）")
				return nil
			}
			config := file.QQ
			fmt.Fprintf(stdout, "enabled=%v api_base_url=%s app_id=%s secret=%s admins=%d agent=%s split_limit=%d\n",
				config.Enabled, config.APIBaseURL, orDash(config.AppID), tokenState(config.AppSecret),
				len(config.AdminUsers), orDash(config.Agent), config.SplitLimit)
			if config.AppID == "" || config.AppSecret == "" {
				fmt.Fprintln(stdout, "凭据不完整：跳过自检（app_id 与 app_secret 都需要）")
				return nil
			}
			client := qq.NewClient(qq.Config{
				AppID:      config.AppID,
				AppSecret:  config.AppSecret,
				APIBaseURL: config.APIBaseURL,
			})
			ctx, cancel := context.WithTimeout(command.Context(), 15*time.Second)
			defer cancel()
			if err := client.VerifyCredential(ctx); err != nil {
				return fmt.Errorf("凭据自检失败：%w", err)
			}
			fmt.Fprintln(stdout, "凭据自检：通过（access token 获取成功）")
			return nil
		},
	}
}
