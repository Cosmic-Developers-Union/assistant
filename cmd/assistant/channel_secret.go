package main

import (
	"fmt"
	"strings"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/envref"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
)

// 通道密钥的来源标记。凭据落在哪个文件必须显示出来：静默回退到凭据库虽然好用，
// 但操作者无从判断 assistant 到底读的是哪一份，出了「连上了但不是我的机器人」
// 这种事只能靠猜。
const (
	secretSourceConfig      = "config.json"
	secretSourceCredentials = "credentials.json"
)

// resolvedChannel 是一条对话通道实例的凭据解析结果。
//
// 解析顺序：config.json 内联值优先（可能带 $VAR 引用，展开失败即报错），缺省按
// 通道键回退凭据库（credentials.json）。与 gitea 通道 token 字段「缺省回退凭据库
// purpose=review」是同一套口径——老配置里的内联值零改动继续可用。
type resolvedChannel struct {
	// Key 是通道键（weixin、weixin/work、qq/support）。
	Key string
	// Field 是密钥字段名（app_secret / bot_token），与 Channel.SecretField 同口径。
	Field string
	// Value 是展开后的密钥。空串表示两处都没有——调用方自己决定这是「报缺凭据」
	// 还是「只报状态」。
	Value string
	// Source 是 Value 的出处（config.json / credentials.json）；Value 为空时也为空。
	Source string
	// AppID / AppIDSource 是 qq 的 AppID（凭据库里 app_secret 记录的 user 维度，
	// 不是密钥，所以也可以留在 config.json 里）。其余平台为空。
	AppID       string
	AppIDSource string
	// Identity 是凭据库里记录的身份（bot/app 名），仅用于诊断显示。
	Identity string
}

// complete 报告解析结果够不够启动这条通道：密钥必须在，qq 还要 app_id。
// app_id 不是密钥，可以留在 config.json，也可以只在凭据库里——两条路都得通，
// 否则「只写了 app_secret」会一路飘到适配器里才炸。
func (r resolvedChannel) complete() bool {
	if strings.TrimSpace(r.Value) == "" {
		return false
	}
	return r.Field != "app_secret" || strings.TrimSpace(r.AppID) != ""
}

// resolveChannelSecret 解析一条对话通道实例的凭据。
//
// 找不到时不报错：list 只想把「没有凭据」显示出来，真正要启动这条通道的地方
// （daemon）自己判断并给出补凭据的指引——那里的措辞能带上具体是哪个通道键。
// 只有内联值里的 $VAR 引用未定义才当场报错：那本来就是配置错误，不该被
// 「凭据库里也许有」掩盖。
func resolveChannelSecret(entry instances.Channel, store *credentials.File) (resolvedChannel, error) {
	inlineField, inline := entry.SecretField()
	resolved := resolvedChannel{Key: entry.Key(), Field: inlineField}
	switch entry.Type {
	case instances.ChannelWeixin, instances.ChannelQQ, instances.ChannelTelegram:
	default:
		// gitea 通道的令牌由调度引擎按用途解析（还牵连身份与分支保护令牌），
		// 不走这条通道密钥的路
		return resolved, nil
	}

	// qq 的 AppID 不是密钥：留在 config.json 里照样能用，放凭据库里也行
	if entry.Type == instances.ChannelQQ {
		resolved.AppID, resolved.AppIDSource = entry.AppID, secretSourceConfig
		if strings.TrimSpace(resolved.AppID) == "" {
			resolved.AppID, resolved.AppIDSource = "", ""
		}
	}

	// 密钥的 $VAR/${VAR} 引用在消费点展开（File 里的原始定义不动）；
	// 未定义变量在这里拦下，不让字面量发往平台
	if inlineField != "" && strings.TrimSpace(inline) != "" {
		expanded, err := envref.Expand(inline, envref.Options{
			Field: fmt.Sprintf("channels[%s].%s", entry.Key(), inlineField),
		})
		if err != nil {
			return resolved, err
		}
		resolved.Value, resolved.Source = expanded, secretSourceConfig
		return resolved, nil
	}

	credential, ok, err := store.ChannelCredentialFor(entry.Key(), entry.Type)
	if err != nil {
		return resolved, err
	}
	if !ok {
		return resolved, nil
	}
	resolved.Value, resolved.Source = credential.Token, secretSourceCredentials
	resolved.Identity = credential.User
	if entry.Type == instances.ChannelQQ {
		resolved.AppID, resolved.AppIDSource = credential.User, secretSourceCredentials
	}
	return resolved, nil
}

// missingChannelSecret 描述「这条通道没有可用凭据」：报出通道键与补凭据的命令。
// 消息要能在没有上下文的地方独立成立——它经常出现在启动日志的一行里。
func missingChannelSecret(entry instances.Channel) error {
	login := "assistant login add --type " + entry.Type
	if name := strings.TrimSpace(entry.Name); name != "" {
		login += " --name " + name
	}
	if entry.Type == instances.ChannelQQ {
		return fmt.Errorf("通道 %s 缺 app_id 或 app_secret（config.json 内联与 %s 都没有）：运行 %s",
			entry.Key(), secretSourceCredentials, login)
	}
	return fmt.Errorf("通道 %s 缺凭据（config.json 内联与 %s 都没有）：运行 %s",
		entry.Key(), secretSourceCredentials, login)
}

// applyResolvedChannel 把解析结果写回通道实例，让下游适配器拿到的是展开后的值。
func applyResolvedChannel(entry instances.Channel, resolved resolvedChannel) instances.Channel {
	switch resolved.Field {
	case "app_secret":
		entry.AppSecret = resolved.Value
	case "bot_token":
		entry.BotToken = resolved.Value
	}
	if resolved.AppID != "" {
		entry.AppID = resolved.AppID
	}
	return entry
}
