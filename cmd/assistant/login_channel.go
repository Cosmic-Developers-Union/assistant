package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/qq"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/telegram"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/weixin"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// 这个文件是 `assistant login --type <平台>` 的对话通道分支：登记通道密钥
// （weixin 扫码 / qq 开放平台 / telegram BotFather）与查看通道状态。
//
// 三者的形状一致——凭据进 credentials.json（按通道键），config.json 里的通道
// 条目只留白名单与端点——所以放在一起共用 loginChannelOptions 与
// mergeChannelEntry；差异只在「凭据从哪来」与「怎么验」。

// loginChannelOptions 是登记/查看通道时与 gitea 无关的输入。
type loginChannelOptions struct {
	// Name 是实例名（多开同一平台时区分用）；缺省写匿名实例，通道键 = type
	Name string
	// BaseURL 是 weixin 的 ilink API 根地址（缺省官方地址）
	BaseURL string
	// APIBaseURL 是 qq / telegram 的 Bot API 根地址（可换自建反代）
	APIBaseURL string
}

// channelKeyOf 拼出通道键：命名实例 = type/name，匿名 = type。与
// instances.Channel.Key 同一口径，通道键就是凭据库里的 host。
func channelKeyOf(channelType, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return channelType
	}
	return channelType + "/" + name
}

// mergeChannelEntry 把这次登录确定的字段并进通道条目，保留其余既有配置。
//
// 密钥字段一律清空：它们只进凭据库，写进 config.json 等于把刚收好的密钥又抄回
// 用户文件。qq 的 app_id 也要清——解析时内联 app_id 优先，留着旧的会让新登记的
// 机器人连不上（操作者以为登的是新机器人，实际用的是旧 app_id）。
func mergeChannelEntry(existing, next instances.Channel) instances.Channel {
	merged := existing
	merged.Type = next.Type
	merged.Name = strings.TrimSpace(next.Name)
	merged.BotToken, merged.AppID, merged.AppSecret = "", "", ""
	overwrite(&merged.BaseURL, next.BaseURL)
	overwrite(&merged.APIBaseURL, next.APIBaseURL)
	overwrite(&merged.BotID, next.BotID)
	overwrite(&merged.LoginUserID, next.LoginUserID)
	if len(next.AdminUsers) > 0 {
		merged.AdminUsers = next.AdminUsers
	}
	return merged
}

func overwrite(target *string, value string) {
	if value = strings.TrimSpace(value); value != "" {
		*target = value
	}
}

// saveChannelEntry 更新或追加一条通道实例并落盘。匹配键是 (type, name)：同名就是
// 同一条通道，重新登录改的是它的凭据，不该每次多出一条。
func saveChannelEntry(file *instances.File, path string, entry instances.Channel) error {
	entry.Name = strings.TrimSpace(entry.Name)
	existing, found := findChannelEntry(file, entry.Type, entry.Name)
	if found {
		file.Channels[existing] = mergeChannelEntry(file.Channels[existing], entry)
	} else {
		file.Channels = append(file.Channels, mergeChannelEntry(instances.Channel{}, entry))
	}
	file.Normalize()
	if err := file.Validate(); err != nil {
		return err
	}
	return instances.Save(path, file)
}

// findChannelEntry 按 (type, name) 找通道条目的下标。
func findChannelEntry(file *instances.File, channelType, name string) (int, bool) {
	name = strings.TrimSpace(name)
	for index := range file.Channels {
		channel := file.Channels[index]
		if channel.Type == channelType && strings.TrimSpace(channel.Name) == name {
			return index, true
		}
	}
	return 0, false
}

// existingAPIBaseURL 返回同 (type, name) 的既有通道条目里配置的自建端点。
//
// 反代用户在 config.json 里写 api_base_url，然后跑 login add 登记凭据——自检必须
// 打同一个端点，否则永远验的是官方地址，一条永远「自检失败」的正道消息。
// 旗标优先于配置：显式给的参数不该被既有配置盖住。
func existingAPIBaseURL(file *instances.File, channelType, name, flagValue string) string {
	if value := strings.TrimSpace(flagValue); value != "" {
		return value
	}
	if index, ok := findChannelEntry(file, channelType, name); ok {
		return file.Channels[index].APIBaseURL
	}
	return ""
}

// saveChannelCredential 把通道密钥写进凭据库（按通道键整条替换）。
// 返回凭据库路径，供成功信息报出落点。
func saveChannelCredential(channelKey, channelType, identity, token string) (string, error) {
	path, err := credentials.Path()
	if err != nil {
		return "", err
	}
	store, err := credentials.Load(path)
	if err != nil {
		return "", err
	}
	store.SetChannelCredential(credentials.Credential{
		Host:    channelKey,
		User:    identity,
		Purpose: channelType,
		Token:   token,
		Source:  credentials.SourceLogin,
	})
	if err := store.Validate(); err != nil {
		return "", err
	}
	return path, credentials.Save(path, store)
}

// reportChannelLogin 打印一次登记的结果：两个落点都要说出来。
//
// 「凭据写哪了」是配置显式化的底线——只说「登录成功」，操作者既不知道密钥在
// credentials.json 还是 config.json，也不知道这条通道在 config.json 里叫什么。
func reportChannelLogin(stdout interface{ Write([]byte) (int, error) }, entry instances.Channel, credentialPath, configPath, identity string) {
	key := entry.Key()
	fmt.Fprintf(stdout, "登录成功：%s 凭据已写入 %s（按通道键 %s，0600）\n", key, credentialPath, key)
	fmt.Fprintf(stdout, "通道条目已写入 %s（%s；白名单 %d 人）\n", configPath, key, len(entry.AdminUsers))
	if identity != "" {
		fmt.Fprintf(stdout, "身份：%s\n", identity)
	}
	fmt.Fprintln(stdout, "启动对话：assistant run（channels 列表里有条目且 runtime 引用它即启用）")
}

// runLoginAddWeixin 扫码登录微信 Bot：拿到凭据后写凭据库，config.json 只留通道
// 条目（base_url / ilink_bot_id / login_user_id 这些诊断字段仍属于配置）。
func runLoginAddWeixin(command *cobra.Command, configPath string, options *loginChannelOptions) error {
	stdout := command.OutOrStdout()
	writePath, file, err := loadInstanceFileForSetup(configPath)
	if err != nil {
		return err
	}
	if file == nil {
		file = &instances.File{}
	}
	baseURL := instances.DefaultWeixinBaseURL
	if index, ok := findChannelEntry(file, instances.ChannelWeixin, options.Name); ok &&
		strings.TrimSpace(file.Channels[index].BaseURL) != "" {
		baseURL = file.Channels[index].BaseURL
	}
	baseURL = firstNonEmpty(strings.TrimSpace(options.BaseURL), baseURL)
	onQR := func(code weixin.QRCode) {
		fmt.Fprintln(stdout, "请用手机微信扫描下方二维码完成登录（扫码后手机上会提示确认）：")
		if err := weixin.RenderQR(stdout, code.Content); err != nil {
			fmt.Fprintf(stdout, "（终端二维码渲染失败：%v，请使用下方链接）\n", err)
		}
		fmt.Fprintf(stdout, "若二维码无法显示或无法扫描，可访问：%s\n", code.Content)
	}
	promptVerify := func(attempt int) (string, error) {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return "", fmt.Errorf("需要验证码但当前环境不可交互（请在终端运行）")
		}
		fmt.Fprintf(command.ErrOrStderr(), "请输入手机显示的验证码（第 %d 次）：", attempt)
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		fmt.Fprintln(command.ErrOrStderr())
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	credentialsGot, err := weixin.Login(command.Context(), baseURL, onQR, promptVerify, nil)
	if err != nil {
		return err
	}
	// 先写凭据库再写 config.json：反过来一旦 config 校验失败，密钥已经收好了，
	// 重跑一次即可；反过来则可能留下一条指向空密钥的通道。
	credentialPath, err := saveChannelCredential(
		channelKeyOf(instances.ChannelWeixin, options.Name), instances.ChannelWeixin,
		credentialsGot.UserID, credentialsGot.BotToken)
	if err != nil {
		return err
	}
	entry := instances.Channel{
		Type:        instances.ChannelWeixin,
		Name:        strings.TrimSpace(options.Name),
		BaseURL:     credentialsGot.BaseURL,
		BotID:       credentialsGot.BotID,
		LoginUserID: credentialsGot.UserID,
	}
	if err := saveChannelEntry(file, writePath, entry); err != nil {
		return err
	}
	reportChannelLogin(stdout, entry, credentialPath, writePath, orDash(credentialsGot.UserID))
	return nil
}

// runLoginAddTelegram 登记 Telegram 通道凭据：交互读入 BotFather 的 token →
// getMe 实测 → 写凭据库 + config.json。
//
// 实测放在写盘之前：token 写进 config.json 之后到 daemon 启动才第一次真正使用，
// 一个填错的 token 会安静地躺在那里直到重启才炸。
func runLoginAddTelegram(command *cobra.Command, configPath string, options *loginChannelOptions) error {
	writePath, file, err := loadInstanceFileForSetup(configPath)
	if err != nil {
		return err
	}
	if file == nil {
		file = &instances.File{}
	}
	apiBaseURL := existingAPIBaseURL(file, instances.ChannelTelegram, options.Name, options.APIBaseURL)
	prompts := newPromptSession(command)
	token, err := prompts.secret("Telegram bot token（@BotFather /newbot 发放）：",
		"通道凭据只能交互式录入，请在终端里运行 assistant login add --type telegram")
	if err != nil {
		return err
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("缺少 bot token")
	}
	adminUsers, err := prompts.adminUsers("telegram")
	if err != nil {
		return err
	}
	me, err := verifyTelegramToken(command.Context(), token, apiBaseURL)
	if err != nil {
		return fmt.Errorf("凭据自检失败（getMe）: %w", err)
	}
	identity := "@" + me.Username
	if me.Username == "" {
		// BotFather 允许不设 username 的机器人；用户名是空的，诊断身份退回
		// 数字 id（全局唯一，够用）
		identity = strconv.FormatInt(me.ID, 10)
	}
	credentialPath, err := saveChannelCredential(
		channelKeyOf(instances.ChannelTelegram, options.Name), instances.ChannelTelegram, identity, token)
	if err != nil {
		return err
	}
	entry := instances.Channel{
		Type:       instances.ChannelTelegram,
		Name:       strings.TrimSpace(options.Name),
		APIBaseURL: apiBaseURL,
		AdminUsers: adminUsers,
	}
	if err := saveChannelEntry(file, writePath, entry); err != nil {
		return err
	}
	reportChannelLogin(command.OutOrStdout(), entry, credentialPath, writePath, identity)
	return nil
}

// runLoginAddQQ 登记 QQ 开放平台凭据：交互读入 AppID/AppSecret → 换取 access
// token 实测 → 写凭据库 + config.json。AppID 不是密钥，但它是凭据库里的身份
// 维度，所以同样只进凭据库；config.json 里的通道条目不写它。
func runLoginAddQQ(command *cobra.Command, configPath string, options *loginChannelOptions) error {
	writePath, file, err := loadInstanceFileForSetup(configPath)
	if err != nil {
		return err
	}
	if file == nil {
		file = &instances.File{}
	}
	apiBaseURL := existingAPIBaseURL(file, instances.ChannelQQ, options.Name, options.APIBaseURL)
	prompts := newPromptSession(command)
	appID, err := prompts.line("QQ 开放平台 AppID：")
	if err != nil {
		return err
	}
	appSecret, err := prompts.secret("QQ 开放平台 AppSecret：",
		"通道凭据只能交互式录入，请在终端里运行 assistant login add --type qq")
	if err != nil {
		return err
	}
	if strings.TrimSpace(appID) == "" || strings.TrimSpace(appSecret) == "" {
		return fmt.Errorf("缺少 app_id 或 app_secret")
	}
	adminUsers, err := prompts.adminUsers("qq")
	if err != nil {
		return err
	}
	if err := verifyQQCredential(command.Context(), appID, appSecret, apiBaseURL); err != nil {
		return fmt.Errorf("凭据自检失败（换取 access token）: %w", err)
	}
	credentialPath, err := saveChannelCredential(
		channelKeyOf(instances.ChannelQQ, options.Name), instances.ChannelQQ, appID, appSecret)
	if err != nil {
		return err
	}
	entry := instances.Channel{
		Type:       instances.ChannelQQ,
		Name:       strings.TrimSpace(options.Name),
		APIBaseURL: apiBaseURL,
		AdminUsers: adminUsers,
	}
	if err := saveChannelEntry(file, writePath, entry); err != nil {
		return err
	}
	reportChannelLogin(command.OutOrStdout(), entry, credentialPath, writePath, appID)
	return nil
}

// qqTokenURL 覆盖 QQ 换取 access token 的端点（生产是包内固定常量，不经配置）。
// 留成变量是为了测试：真端点只认真实 AppID，「凭据正确 → 自检通过 → 写盘」这条
// 主路径没法在离线测试里跑通，于是只剩下失败分支有覆盖。
var qqTokenURL string

// verifyQQCredential 实测 QQ 凭据（换取 access token）。自检放在写盘之前。
func verifyQQCredential(ctx context.Context, appID, appSecret, apiBaseURL string) error {
	verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	config := qq.Config{AppID: appID, AppSecret: appSecret, APIBaseURL: apiBaseURL}
	if qqTokenURL != "" {
		config.TokenURL = qqTokenURL
	}
	return qq.NewClient(config).VerifyCredential(verifyCtx)
}

// verifyTelegramToken 实测 Telegram token（getMe）并回传机器人身份。
func verifyTelegramToken(ctx context.Context, token, apiBaseURL string) (telegram.Me, error) {
	verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return telegram.NewClient(telegram.Config{BotToken: token, APIBaseURL: apiBaseURL}).GetMe(verifyCtx)
}

// —— 通道状态（login list --type <平台>）——

// channelRow 是一条通道实例的展示行：通道键 + 生效字段 + 凭据来源。
type channelRow struct {
	Entry    instances.Channel
	Resolved resolvedChannel
}

// channelRows 按平台类型列出通道实例并解析凭据。未配置 / 配置里没有该类型的
// 通道时返回 nil，由调用方打印引导。
func channelRows(file *instances.File, channelType string) ([]channelRow, error) {
	if file == nil {
		return nil, nil
	}
	store, err := loadCredentialStoreQuietly()
	if err != nil {
		return nil, err
	}
	var rows []channelRow
	for _, entry := range file.Channels {
		if entry.Type != channelType {
			continue
		}
		resolved, err := resolveChannelSecret(entry, store)
		if err != nil {
			return nil, err
		}
		rows = append(rows, channelRow{Entry: entry, Resolved: resolved})
	}
	return rows, nil
}

// credentialSummary 描述一条通道的凭据状态：拿到了没有、从哪个文件拿的。绝不
// 回显密钥本身（status 常被整段贴进 issue 求助）。
func credentialSummary(resolved resolvedChannel) string {
	if strings.TrimSpace(resolved.Value) == "" {
		return "none"
	}
	if resolved.Source == secretSourceCredentials {
		return "set（" + resolved.Identity + " @ " + secretSourceCredentials + "）"
	}
	return "set（" + secretSourceConfig + "）"
}

// runLoginListChannel 打印某个平台下的通道实例状态。qq / telegram 顺带在线实测
// 凭据——它们不像 weixin 那样能从状态本身看出凭据是否还有效。
func runLoginListChannel(command *cobra.Command, configPath, channelType string) error {
	_, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return err
	}
	rows, err := channelRows(file, channelType)
	if err != nil {
		return err
	}
	stdout := command.OutOrStdout()
	if len(rows) == 0 {
		// 「还没配」和「配置文件读不出来」必须分开说：前者去 channels 里加条目
		// 或跑一次登录，后者要先去修配置语法，指错了会让人反复加白名单。
		fmt.Fprintf(stdout, "未配置 %s 通道：运行 assistant login add --type %s 登记凭据"+
			"（或在 config.json 的 channels 列表加入 {\"type\":%q, ...} 条目）\n",
			channelType, channelType, channelType)
		return nil
	}
	failed := false
	for _, row := range rows {
		entry := row.Entry
		switch channelType {
		case instances.ChannelWeixin:
			fmt.Fprintf(stdout, "key=%s enabled=%v base_url=%s login=%s bot_id=%s token=%s admins=%d\n",
				entry.Key(), entry.IsEnabled(), entry.BaseURL,
				orDash(entry.LoginUserID), orDash(entry.BotID),
				credentialSummary(row.Resolved), len(entry.AdminUsers))
			// weixin 没有只读的凭据校验端点（token 只能靠重新扫码获得）。
			// 如实说「未实测」比留一行空着好——操作者会以为这一列验过了。
			fmt.Fprintln(stdout, "  凭据自检：跳过（weixin 只能重新扫码登录验证）")
			continue
		case instances.ChannelQQ:
			appID := row.Resolved.AppID
			if appID == "" {
				appID = "-"
			}
			fmt.Fprintf(stdout, "key=%s enabled=%v api_base_url=%s app_id=%s secret=%s admins=%d split_limit=%d\n",
				entry.Key(), entry.IsEnabled(), entry.APIBaseURL, appID,
				credentialSummary(row.Resolved), len(entry.AdminUsers), entry.SplitLimit)
		case instances.ChannelTelegram:
			fmt.Fprintf(stdout, "key=%s enabled=%v token=%s admins=%d api=%s\n",
				entry.Key(), entry.IsEnabled(), credentialSummary(row.Resolved),
				len(entry.AdminUsers), entry.APIBaseURL)
		}
		// 凭据不全时「跳过」而不是「失败」：还没填与填了但不被接受，需要操作者
		// 做的事完全不同，混成一句「自检失败」会让人去改错的地方。
		if !row.Resolved.complete() {
			fmt.Fprintf(stdout, "  凭据自检：跳过（%s 与 %s 都没有可用凭据）\n", secretSourceConfig, secretSourceCredentials)
			continue
		}
		if err := verifyChannelRow(channelType, row); err != nil {
			failed = true
			fmt.Fprintf(stdout, "  凭据自检失败（%s）：%v\n", entry.Key(), err)
			continue
		}
		if channelType == instances.ChannelTelegram {
			fmt.Fprintln(stdout, "  凭据自检：通过（getMe 成功）")
		} else {
			fmt.Fprintln(stdout, "  凭据自检：通过（access token 获取成功）")
		}
	}
	if failed {
		return fmt.Errorf("存在凭据自检失败的 %s 通道", channelType)
	}
	return nil
}

// verifyChannelRow 在线实测一条通道的凭据。只对 qq / telegram 有意义——weixin
// 分支在这里就返回了，调用方已经打印「跳过」。
func verifyChannelRow(channelType string, row channelRow) error {
	resolved := row.Resolved
	switch channelType {
	case instances.ChannelQQ:
		return verifyQQCredential(context.Background(), resolved.AppID, resolved.Value, row.Entry.APIBaseURL)
	case instances.ChannelTelegram:
		_, err := verifyTelegramToken(context.Background(), resolved.Value, row.Entry.APIBaseURL)
		return err
	}
	return nil
}
