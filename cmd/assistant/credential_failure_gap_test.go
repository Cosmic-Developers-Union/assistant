package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"

	"github.com/spf13/cobra"
)

// corruptCredentialStore 在临时目录里放一份语法坏掉的凭据库，并把
// ASSISTANT_CREDENTIALS 指向它。
//
// 这条路径（凭据库存在但解析不了）此前没有任何用例走过：所有 seed* 夹具都写合法
// JSON。结果是 store.go / capability.go / validate.go 里「读凭据失败」的那几个
// return 分支全都没被覆盖，而它们恰恰是「机器被人手工改坏过凭据库」时唯一的反应。
//
// 用 ASSISTANT_CREDENTIALS 而不是 XDG_CONFIG_HOME：override 优先级最高，其它
// 用例留下的 XDG 状态不会干扰。
func corruptCredentialStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte("{ not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	return path
}

// corruptCredentialStoreBytes 在临时目录里放一份指定内容的凭据库，并把
// ASSISTANT_CREDENTIALS 指向它。
//
// corruptCredentialStore 只能造出「整份文件读不出来」这一种坏法；而凭据库的校验
// 是分层的——JSON 语法过了之后还有逐条记录的字段校验。要打到第二层，必须能把
// 内容换成「语法合法、语义非法」的形态，所以这里把内容开放出来。
func corruptCredentialStoreBytes(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CREDENTIALS", path)
	return path
}

// TestCredentialForPropagatesCorruptStore 断言凭据库损坏时 credentialFor 把解析
// 错误原样上报（ok=false），而不是当成「该用途没登录过」。
//
// 两者对调用方是截然不同的世界：ok=false 会让命令提示「去 login add」，而真相是
// 本机已有凭据、只是坏了——照着提示重登只会再写一份同样读不出来的文件。错误必须
// 一路冒到最外层，tokenForPurpose 也要把它包着带出去（而不是替换成缺令牌文案）。
func TestCredentialForPropagatesCorruptStore(t *testing.T) {
	storePath := corruptCredentialStore(t)
	host := "https://broken.example.com"

	_, ok, err := credentialFor("", host, credentials.PurposeAdmin)
	if err == nil {
		t.Fatalf("凭据库损坏应报错，ok=%v", ok)
	}
	if ok {
		t.Error("损坏时不该声称找到了凭据")
	}
	if !strings.Contains(err.Error(), storePath) {
		t.Errorf("错误应点明坏掉的文件 %s：%v", storePath, err)
	}

	// tokenForPurpose 必须让底层错误穿透：若它退化成「缺少 admin 用途令牌」的
	// 文案，操作者会被引到 login add 而不是去修那份坏文件。
	_, err = tokenForPurpose("", host, credentials.PurposeAdmin)
	if err == nil {
		t.Fatal("凭据库损坏时 tokenForPurpose 应报错")
	}
	if strings.Contains(err.Error(), "缺少") {
		t.Errorf("不该把它说成缺少令牌：%v", err)
	}
	if !strings.Contains(err.Error(), storePath) {
		t.Errorf("应保留底层错误里的文件路径：%v", err)
	}
}

// TestRequireAdminIdentityFailsOpenOnBrokenStore 断言凭据库读不出来时管理门禁
// 「放行」而不是拒绝。
//
// 这是 capability.go 注释里写明的两条有意设计之一：本地身份只是加速失败的缓存，
// 真正的授权事实来自服务端（setup.NewAdmin 会在线核对 is_admin）。凭据库坏掉时
// 若在此处拒绝，操作者会被一个本地文件挡在管理命令之外，连修它的机会都没有。
func TestRequireAdminIdentityFailsOpenOnBrokenStore(t *testing.T) {
	corruptCredentialStore(t)

	// 站点的登录身份是普通账号：如果错误被当成「读到了非管理员身份」，这里会
	// 报错。断言不报错才能证明坏库真的走了放行分支。
	storePath := filepath.Join(t.TempDir(), "plain.json")
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: "https://plain.example.com", User: "dev"})
	if err := credentials.Save(storePath, store); err != nil {
		t.Fatal(err)
	}
	// 先确认合法库 + 非管理员身份确实会被拒（否则下面的断言毫无意义）
	t.Setenv("ASSISTANT_CREDENTIALS", storePath)
	if err := requireAdminIdentity("", "https://plain.example.com"); err == nil {
		t.Fatal("非管理员身份应被拒绝")
	}

	corruptCredentialStore(t)
	if err := requireAdminIdentity("", "https://plain.example.com"); err != nil {
		t.Errorf("凭据库损坏时应放行（权威判定在服务端），got %v", err)
	}
}

// TestValidateReportsBrokenCredentialStore 断言 validate 在凭据库损坏时输出一行
// ERROR（退出码 1），而不是静默当作「没有令牌」。
//
// validate 是「改完配置、重启 daemon 之前先跑一遍」的只读体检；把不可解析的凭据
// 库报成一句 WARN「缺 review/merge 令牌」会让操作者去 setup 重派生，而本地文件
// 仍然读不出来——诊断结论完全指错方向。
func TestValidateReportsBrokenCredentialStore(t *testing.T) {
	storePath := corruptCredentialStore(t)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := instances.Save(configPath, &instances.File{Channels: []instances.Channel{{
		Type:     instances.ChannelGitea,
		Host:     "https://broken.example.com",
		Reviewer: "ai",
		Merger:   "merge",
		Repos:    []instances.Repo{{Name: "acme/video"}},
	}}}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runValidate(&out, configPath); err == nil {
		t.Fatal("凭据库损坏应让 validate 以非零退出")
	}
	text := out.String()
	if !strings.Contains(text, "ERROR") || !strings.Contains(text, "credentials.json") {
		t.Errorf("应有一行 credentials.json 的 ERROR：\n%s", text)
	}
	if !strings.Contains(text, storePath) && !strings.Contains(text, "解析失败") {
		t.Errorf("应说明是解析失败：\n%s", text)
	}
}

// TestAuditServerReportsBrokenCredentialStore 断言 doctor 的服务端体检在凭据库
// 损坏时报错退出，而不是当成「该实例没有 admin 令牌」回一条 SKIPPED。
//
// SKIPPED 的措辞是「缺少凭据」——操作者据此会去补 GITEA_HOST/GITEA_ACCESS_TOKEN，
// 但问题在本地 JSON 坏了。凭据库的读错误必须原样上报。
//
// 配置里只有 repositories、没有任何平台条目：此时目标由 --repo 决定（doctor 只
// 检查一个仓库，不需要平台身份），凭据查询必然落到凭据库上——这正是本条要打的
// 分支。凭据库损坏时该查询必须报错，而不是让 auditServer 把空令牌当成「没登录」。
func TestAuditServerReportsBrokenCredentialStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	storePath := corruptCredentialStore(t)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	// 一个 gitea 通道（host 不写进凭据）：--repo 命中 route (d)「单平台 + 显式
	// 仓库名」，fromConfig 为真 → 查询该 host 的 admin 用途令牌。
	if err := instances.Save(configPath, &instances.File{
		Channels: []instances.Channel{{
			Type: instances.ChannelGitea, Host: "https://broken.example.com",
			Reviewer: "ai", Merger: "merge",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// auditServer 从 --config 旗标读配置路径（参数位的 configPath 会被覆盖），
	// 因此这里必须把路径写进旗标默认值，否则它按空路径解析、看不到任何实例。
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetContext(t.Context())
	command.Flags().String("config", configPath, "")
	command.Flags().String("repo", "acme/video", "")

	_, _, err := auditServer(command, configPath, &repoToolOptions{Dir: dir}, 2, false)
	if err == nil {
		t.Fatal("凭据库损坏时服务端体检应报错")
	}
	if !strings.Contains(err.Error(), storePath) {
		t.Errorf("错误应点明坏掉的凭据库：%v", err)
	}
}

// TestCredentialForFailsWhenConfigDirUnknown 断言连凭据文件落点都定不下来时
// credentialFor 立刻报错，而不是拿一个拼出来的路径继续往下读。
//
// 落点解析失败意味着这台机器既没有 XDG_CONFIG_HOME 也没有 HOME（容器里以空
// 环境跑、或 HOME 被清掉）：此时任何一个「拼个相对路径去读」的兜底都会让命令
// 在错误的目录里找到一份陌生凭据，或者悄悄把自己当成未登录。错误必须当场冒出来，
// 并且点明是「定位凭据目录」这一步失败的，否则操作者会去查文件权限。
func TestCredentialForFailsWhenConfigDirUnknown(t *testing.T) {
	// 两个环境变量都清空：Path() 内部走 os.UserConfigDir()，它在两者皆空时报错。
	// ASSISTANT_CREDENTIALS 必须一并清掉，否则 override 会让落点照常解析成功。
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	if _, err := credentials.Path(); err == nil {
		t.Skip("本机在 HOME/XDG 皆空时仍能定位配置目录，构造不出该分支")
	}

	_, ok, err := credentialFor("", "https://nowhere.example.com", credentials.PurposeAdmin)
	if err == nil {
		t.Fatalf("定不下凭据落点时应报错，ok=%v", ok)
	}
	if ok {
		t.Error("落点未知时不该声称找到了凭据")
	}
	if !strings.Contains(err.Error(), "定位凭据目录") {
		t.Errorf("错误应点明是定位配置目录失败：%v", err)
	}
	if strings.Contains(err.Error(), "缺少") {
		t.Errorf("不该把它说成缺少令牌：%v", err)
	}
}
