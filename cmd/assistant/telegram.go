package main

import (
	"context"
	"fmt"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/telegram"

	"github.com/spf13/cobra"
)

// newTelegramCommand 管理 Telegram 对话桥：凭据是 @BotFather 发放的 bot token
// （手工写入 config.json 的 channels 列表，type=telegram），这里提供状态与自检。
func newTelegramCommand(configFlag *string) *cobra.Command {
	command := &cobra.Command{
		Use:   "telegram",
		Short: "Telegram 对话桥（Bot API 长轮询）：配置状态与凭据自检",
		Long: "Telegram 通道凭据来自 @BotFather：/newbot 创建机器人拿到 token 后，\n" +
			"在 config.json 的 channels 列表加一条 {\"type\": \"telegram\",\n" +
			"\"bot_token\": \"...\", \"admin_users\": [...]},assistant run 即启动。\n" +
			"本命令只读配置并实测凭据（getMe），不做任何写入。",
		Args: cobra.NoArgs,
	}
	command.AddCommand(newTelegramStatusCommand(configFlag))
	return command
}

// newTelegramStatusCommand 显示每个 telegram 通道实例的状态并实测 getMe。
func newTelegramStatusCommand(configFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "显示 Telegram 通道实例状态（不显示密钥），并实测 bot token",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			_, file, err := resolveInstanceFile(commandOptions{ConfigPath: *configFlag})
			if err != nil {
				return err
			}
			stdout := command.OutOrStdout()
			var telegramChannels []instances.Channel
			if file != nil {
				for _, entry := range file.Channels {
					if entry.Type == instances.ChannelTelegram {
						telegramChannels = append(telegramChannels, entry)
					}
				}
			}
			if len(telegramChannels) == 0 {
				fmt.Fprintln(stdout, "未配置 Telegram 通道（在 config.json 的 channels 列表加 type=telegram 条目）")
				return nil
			}
			for _, entry := range telegramChannels {
				fmt.Fprintf(stdout, "%s token=%s admins=%d agent=%s api=%s\n",
					entry.Key(), tokenState(entry.BotToken), len(entry.AdminUsers), orDash(entry.Agent), entry.APIBaseURL)
				if err := verifyTelegramToken(command, entry); err != nil {
					return fmt.Errorf("%s 凭据自检失败：%w", entry.Key(), err)
				}
				fmt.Fprintf(stdout, "  凭据自检：通过（getMe 成功）\n")
			}
			return nil
		},
	}
}

func verifyTelegramToken(command *cobra.Command, entry instances.Channel) error {
	client := telegram.NewClient(telegram.Config{
		BotToken:   entry.BotToken,
		APIBaseURL: entry.APIBaseURL,
	})
	ctx, cancel := context.WithTimeout(command.Context(), 15*time.Second)
	defer cancel()
	_, err := client.GetMe(ctx)
	return err
}
