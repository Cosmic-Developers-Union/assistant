package cli

import (
	json "encoding/json/v2"
	"fmt"
	"strings"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/project"
	"github.com/spf13/cobra"
)

func newProtectionCommand() *cobra.Command {
	var instance, repo, branch, merger string
	var checks []string
	var asJSON bool
	root := &cobra.Command{Use: "protection", Short: "查看、更新或移除当前项目的分支保护", Args: cobra.NoArgs, Example: "  assistant project protection show\n  assistant project protection update --dry-run\n  assistant project protection update --required-checks build,test\n  assistant project protection remove --branch main"}
	root.PersistentFlags().StringVar(&instance, "instance", "", "可选覆盖 Git remote 匹配的登录实例；写操作需要仓库管理员权限")
	root.PersistentFlags().StringVar(&repo, "repo", "", "owner/name（缺省从所选实例匹配 Git remote）")
	root.PersistentFlags().StringVar(&branch, "branch", "", "具体分支名（缺省平台默认分支），不接受通配符")
	for _, kind := range []string{"show", "update", "remove"} {
		cmd := &cobra.Command{Use: kind, Args: cobra.NoArgs, Short: map[string]string{"show": "显示当前实际保护规则，不修改状态", "update": "创建或更新双批准保护，不改标签和协作者", "remove": "移除选定分支的同名保护规则"}[kind], RunE: func(cmd *cobra.Command, _ []string) (resultErr error) {
			dir, _ := cmd.Flags().GetString("dir")
			entry, err := projectGiteaInstance(dir, instance)
			if err != nil {
				return err
			}
			defer func() { resultErr = credentials.RedactError(resultErr, entry.Token) }()
			owner, name, err := projectRepo(dir, repo, entry.URL)
			if err != nil {
				return err
			}
			client, err := project.NewClient(entry)
			if err != nil {
				return err
			}
			dry, _ := cmd.Flags().GetBool("dry-run")
			opt := project.ProtectionOptions{Owner: owner, Name: name, Branch: branch, Merger: merger, Checks: checks, DryRun: dry}
			var state *project.Protection
			switch kind {
			case "show":
				state, err = client.GetProtection(cmd.Context(), opt)
			case "update":
				state, err = client.UpdateProtection(cmd.Context(), opt)
			case "remove":
				state, err = client.RemoveProtection(cmd.Context(), opt)
			}
			if err != nil {
				return err
			}
			if kind == "show" {
				return printProtection(cmd, state, asJSON)
			}
			prefix := "已"
			if dry {
				prefix = "演练：将"
			}
			if kind == "remove" {
				cmd.Printf("%s移除 %s 分支 %s 的同名保护规则\n", prefix, state.Repository, state.Branch)
			} else {
				cmd.Printf("%s更新 %s 分支 %s 的双批准保护，合并白名单 @%s\n", prefix, state.Repository, state.Branch, merger)
			}
			return nil
		}}
		switch kind {
		case "show":
			cmd.Flags().BoolVar(&asJSON, "json", false, "输出完整平台保护规则 JSON；缺失规则为 null")
		case "update":
			cmd.Aliases = []string{"install"}
			cmd.Flags().StringVar(&merger, "merger", "merge", "合并白名单账号")
			cmd.Flags().StringSliceVar(&checks, "required-checks", nil, "必要检查 context；缺省保留，--required-checks= 清空")
		case "remove":
			cmd.Aliases = []string{"uninstall"}
		}
		root.AddCommand(cmd)
	}
	return root
}

func printProtection(cmd *cobra.Command, state *project.Protection, asJSON bool) error {
	if asJSON {
		data, err := json.Marshal(state)
		if err != nil {
			return fmt.Errorf("编码分支保护: %w", err)
		}
		cmd.Println(string(data))
		return nil
	}
	cmd.Printf("仓库：%s\n分支：%s\n", state.Repository, state.Branch)
	if state.Rule == nil {
		cmd.Println("保护：无同名规则")
		return nil
	}
	r := state.Rule
	cmd.Printf("规则：%s\n批准数：%d\n允许直接推送：%t\n合并白名单启用：%t\n合并账号：%s\n合并团队：%s\n管理员禁止绕过：%t\n提交后批准过期：%t\n阻止驳回/未回应/落后：%t/%t/%t\n必要检查启用：%t\n必要检查：%s\n", r.RuleName, r.RequiredApprovals, r.EnablePush, r.EnableMergeWhitelist, strings.Join(r.MergeWhitelistUsernames, ","), strings.Join(r.MergeWhitelistTeams, ","), r.BlockAdminMergeOverride, r.DismissStaleApprovals, r.BlockOnRejectedReviews, r.BlockOnOfficialReviewRequests, r.BlockOnOutdatedBranch, r.EnableStatusCheck, strings.Join(r.StatusCheckContexts, ","))
	return nil
}
