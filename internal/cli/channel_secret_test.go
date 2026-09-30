package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
)

// storeWithChannel 造一份只含一条通道凭据的凭据库。
func storeWithChannel(key, purpose, user, token string) *credentials.File {
	store := &credentials.File{}
	store.SetChannelCredential(credentials.Credential{Host: key, Purpose: purpose, User: user, Token: token})
	return store
}

// 通道密钥的解析顺序是本次改动的核心契约：config.json 内联优先（含 $VAR 展开），
// 缺省按通道键回退凭据库，两处都没有才算缺。顺序错了会有两种难查的后果——
// 改了凭据库却不生效（内联赢），或者改了 config.json 反而被旧凭据盖住（回退赢）。
func TestResolveChannelSecretPrefersInlineOverCredentialStore(t *testing.T) {
	t.Setenv("INLINE_BOT_TOKEN", "from-env")
	// 通道键随实例名走（telegram/work），下面每条通道都带 Name: "work"
	store := storeWithChannel("telegram/work", credentials.PurposeTelegram, "@cosmic_bot", "from-store")

	t.Run("内联明文优先", func(t *testing.T) {
		resolved, err := resolveChannelSecret(
			instances.Channel{Type: instances.ChannelTelegram, Name: "work", BotToken: "from-config"}, store)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Value != "from-config" || resolved.Source != secretSourceConfig {
			t.Errorf("= %+v, want from-config / config.json", resolved)
		}
		if !resolved.complete() {
			t.Error("complete() = false")
		}
	})

	t.Run("内联 $VAR 展开后仍优先", func(t *testing.T) {
		resolved, err := resolveChannelSecret(
			instances.Channel{Type: instances.ChannelTelegram, Name: "work", BotToken: "$INLINE_BOT_TOKEN"}, store)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Value != "from-env" || resolved.Source != secretSourceConfig {
			t.Errorf("= %+v, want from-env / config.json", resolved)
		}
	})

	t.Run("内联引用未定义即报错，不被凭据库掩盖", func(t *testing.T) {
		// 「字面量 $VAR 还是空串」发到平台都难以排查；更不能因为凭据库里恰好有
		// 一条就把配置写错这件事吞掉
		_, err := resolveChannelSecret(
			instances.Channel{Type: instances.ChannelTelegram, Name: "work", BotToken: "$UNDEFINED_TOKEN"}, store)
		if err == nil || !strings.Contains(err.Error(), "channels[telegram/work].bot_token") {
			t.Fatalf("err = %v, want 点名字段", err)
		}
	})

	t.Run("内联 $VAR 解析成空串不回退凭据库", func(t *testing.T) {
		// 用户写了 bot_token=$VAR 就说明「密钥由环境注入」；变量空了就该报缺，
		// 静默改用凭据库会让它连上另一个机器人
		t.Setenv("EMPTY_BOT_TOKEN", "")
		resolved, err := resolveChannelSecret(
			instances.Channel{Type: instances.ChannelTelegram, Name: "work", BotToken: "${EMPTY_BOT_TOKEN}"}, store)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Value != "" || resolved.Source != secretSourceConfig {
			t.Errorf("= %+v, want 空值且来源仍是 config.json", resolved)
		}
		if resolved.complete() {
			t.Error("空串不该被判为凭据齐备")
		}
	})

	t.Run("内联缺省时按通道键回退凭据库", func(t *testing.T) {
		resolved, err := resolveChannelSecret(
			instances.Channel{Type: instances.ChannelTelegram, Name: "work"}, store)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Value != "from-store" || resolved.Source != secretSourceCredentials {
			t.Errorf("= %+v, want from-store / credentials.json", resolved)
		}
		if resolved.Identity != "@cosmic_bot" {
			t.Errorf("Identity = %q, want @cosmic_bot（诊断用）", resolved.Identity)
		}
	})

	t.Run("命名实例按 weixin/work 取而非 weixin", func(t *testing.T) {
		named := storeWithChannel("weixin/work", credentials.PurposeWeixin, "wxid_7", "wx-tok")
		resolved, err := resolveChannelSecret(instances.Channel{Type: instances.ChannelWeixin, Name: "work"}, named)
		if err != nil || resolved.Value != "wx-tok" {
			t.Errorf("= %+v, %v, want wx-tok", resolved, err)
		}
		// 匿名实例不该命中命名实例那条记录
		resolved, err = resolveChannelSecret(instances.Channel{Type: instances.ChannelWeixin}, named)
		if err != nil || resolved.complete() {
			t.Errorf("= %+v, %v, want 解析不到", resolved, err)
		}
	})
}

// 两处都没有凭据时报的是「缺什么、去哪补」，而不是笼统的启动失败——这条消息会
// 出现在启动日志的一行里，操作者往往没有别的上下文。
func TestMissingChannelSecretNamesTheChannelAndTheCommand(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		entry instances.Channel
		want  []string
	}{
		{"匿名 weixin", instances.Channel{Type: instances.ChannelWeixin}, []string{"通道 weixin 缺凭据", "assistant login add --type weixin"}},
		{"命名 telegram", instances.Channel{Type: instances.ChannelTelegram, Name: "work"}, []string{"通道 telegram/work 缺凭据", "--type telegram --name work"}},
		{"qq 点名两个字段", instances.Channel{Type: instances.ChannelQQ, Name: "q1"}, []string{"通道 qq/q1 缺 app_id 或 app_secret", "--type qq --name q1"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := missingChannelSecret(testCase.entry)
			if err == nil {
				t.Fatal("应报错")
			}
			for _, want := range testCase.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want 含 %q", err, want)
				}
			}
		})
	}
}

// qq 的 app_id 不是密钥：可以留在 config.json，也可以只在凭据库里（那里它是
// app_secret 记录的 user 维度）。两条路都得认，否则「只写了 app_secret」会一路
// 飘到适配器里才炸成一句看不懂的 401。
func TestResolveChannelSecretHandlesQQAppIDFromEitherPlace(t *testing.T) {
	t.Run("app_id 留在 config.json", func(t *testing.T) {
		resolved, err := resolveChannelSecret(
			instances.Channel{Type: instances.ChannelQQ, Name: "q1", AppID: "102000", AppSecret: "s"},
			&credentials.File{})
		if err != nil {
			t.Fatal(err)
		}
		if resolved.AppID != "102000" || resolved.AppIDSource != secretSourceConfig {
			t.Errorf("= %+v, want 102000 / config.json", resolved)
		}
		if !resolved.complete() {
			t.Error("complete() = false")
		}
	})

	t.Run("app_id 只在凭据库里", func(t *testing.T) {
		resolved, err := resolveChannelSecret(
			instances.Channel{Type: instances.ChannelQQ, Name: "q1"},
			storeWithChannel("qq/q1", credentials.PurposeQQ, "102000", "s"))
		if err != nil {
			t.Fatal(err)
		}
		if resolved.AppID != "102000" || resolved.AppIDSource != secretSourceCredentials {
			t.Errorf("= %+v, want 102000 / credentials.json", resolved)
		}
	})

	t.Run("有 app_secret 没 app_id 仍算缺", func(t *testing.T) {
		resolved, err := resolveChannelSecret(
			instances.Channel{Type: instances.ChannelQQ, Name: "q1", AppSecret: "s"}, &credentials.File{})
		if err != nil {
			t.Fatal(err)
		}
		if resolved.complete() {
			t.Error("缺 app_id 时不该判为凭据齐备")
		}
	})
}

// gitea 通道不走这条解析：它的令牌由调度引擎按用途解析，还牵连身份与分支保护
// 令牌。误把它接进来会让 gitea 通道拿到一条对话通道的凭据。
func TestResolveChannelSecretLeavesGiteaAlone(t *testing.T) {
	resolved, err := resolveChannelSecret(
		instances.Channel{Type: instances.ChannelGitea, Host: "https://git.example.com", Token: "t"},
		storeWithChannel("https://git.example.com", credentials.PurposeMCP, "alice", "m"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Value != "" || resolved.Source != "" || resolved.Field != "token" {
		t.Errorf("= %+v, want 不解析（值与来源都空）", resolved)
	}
}

// 同键多身份是「整条替换」写入约定被绕开时的产物：解析点给不出账号可选项，
// 报错必须说清怎么办。
func TestResolveChannelSecretSurfacesDuplicateCredentialError(t *testing.T) {
	store := &credentials.File{Credentials: []credentials.Credential{
		{Host: "telegram", User: "@a", Purpose: credentials.PurposeTelegram, Token: "1"},
		{Host: "telegram", User: "@b", Purpose: credentials.PurposeTelegram, Token: "2"},
	}}
	_, err := resolveChannelSecret(instances.Channel{Type: instances.ChannelTelegram}, store)
	if err == nil || !strings.Contains(err.Error(), "assistant login add --type telegram") {
		t.Fatalf("err = %v, want 含重新登录的出路", err)
	}
}

// applyResolvedChannel 把解析结果写回通道实例：下游适配器拿到的是展开后的值，
// 写错字段的话 telegram 会带着 weixin 的 token 去连。
func TestApplyResolvedChannelWritesBackToTheRightField(t *testing.T) {
	telegram := applyResolvedChannel(instances.Channel{Type: instances.ChannelTelegram},
		resolvedChannel{Field: "bot_token", Value: "tg", Source: secretSourceCredentials})
	if telegram.BotToken != "tg" {
		t.Errorf("BotToken = %q, want tg", telegram.BotToken)
	}
	qq := applyResolvedChannel(instances.Channel{Type: instances.ChannelQQ},
		resolvedChannel{Field: "app_secret", Value: "s", AppID: "102000"})
	if qq.AppSecret != "s" || qq.AppID != "102000" {
		t.Errorf("= %+v, want AppSecret=s AppID=102000", qq)
	}
}

// loadCredentialStoreQuietly 读不到凭据库时给空库而不是错误：daemon 逐条启动
// 通道时，一条通道缺凭据不该让整个 runtime 起不来（内联的凭据仍然可用）。
func TestLoadCredentialStoreQuietlyFallsBackToEmptyStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	store, err := loadCredentialStoreQuietly()
	if err != nil {
		t.Fatalf("文件不存在时应给空库而非报错：%v", err)
	}
	if !store.Empty() {
		t.Errorf("store = %+v, want 空", store)
	}
	// 内容正常时读出来
	normal := storeWithChannel("telegram", credentials.PurposeTelegram, "@bot", "tok")
	if err := credentials.Save(path, normal); err != nil {
		t.Fatal(err)
	}
	store, err = loadCredentialStoreQuietly()
	if err != nil {
		t.Fatal(err)
	}
	credential, ok, err := store.ChannelCredentialFor("telegram", credentials.PurposeTelegram)
	if err != nil || !ok || credential.Token != "tok" {
		t.Errorf("= %+v, %v, %v, want tok", credential, ok, err)
	}
}

// 坏掉的凭据库同样给空库 + 错误，让调用方决定降级还是中止。
func TestLoadCredentialStoreQuietlyReportsBrokenStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := loadCredentialStoreQuietly()
	if err == nil {
		t.Fatal("解析失败应报错")
	}
	if store == nil || !store.Empty() {
		t.Errorf("store = %+v, want 空库", store)
	}
}
