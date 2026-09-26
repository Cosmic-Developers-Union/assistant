package main

import (
	"strings"
	"testing"
)

// `assistant mcp gitea` 的命令面契约：flag 名、默认值与帮助文本是**对外接口**
// （各 AI CLI 的 MCP 配置直接调用它），写错 flag 名等于该入口静默失效。
func TestMCPGiteaCommandFlagsAndHelp(t *testing.T) {
	command := newMCPGiteaCommand()

	if command.Use != "gitea" {
		t.Errorf("Use = %q, want gitea", command.Use)
	}
	if command.Args == nil {
		t.Error("应拒绝多余位置参数（Args 不能为 nil）")
	}

	for flag, want := range map[string]string{
		"host":  "",
		"token": "",
		"dir":   ".",
	} {
		defined := command.Flags().Lookup(flag)
		if defined == nil {
			t.Errorf("缺少 --%s（MCP 配置按名调用，漏掉就是静默失效）", flag)
			continue
		}
		if defined.DefValue != want {
			t.Errorf("--%s 默认值 = %q, want %q", flag, defined.DefValue, want)
		}
	}

	// 帮助正文必须交代检测顺序：操作者排「工具用不了」时照着它逐项排查
	for _, want := range []string{"--host", "GITEA_ACCESS_TOKEN", "config.json", "凭据库"} {
		if !strings.Contains(command.Long, want) {
			t.Errorf("帮助缺少 %q：\n%s", want, command.Long)
		}
	}
}

// 挂到 mcp 命令树下：`assistant mcp gitea` 可达（子命令没挂上等于该能力消失）。
func TestMCPCommandIncludesGitea(t *testing.T) {
	configFlag := new(string)
	command := newMCPCommand(configFlag)

	names := map[string]bool{}
	for _, child := range command.Commands() {
		names[child.Use] = true
	}
	for _, want := range []string{"gitea", "daemon", "sessions"} {
		if !names[want] {
			t.Errorf("mcp 下缺少子命令 %s：%v", want, names)
		}
	}
}

// RunE 把三个 flag 原样交给 mcps.RunGitea：传错/漏传不会有编译错误，只会让会话
// 拿到错的目录或站点。用 --host 一个不可能解析的站点值驱动，断言它确实被采纳
// （错误信息里带上该值即说明参数到达了实现）。
func TestMCPGiteaRunECarriesFlags(t *testing.T) {
	command := newMCPGiteaCommand()
	command.SetArgs([]string{"--host", "https://mcp-gitea-contract.invalid", "--dir", t.TempDir()})
	command.SetOut(&strings.Builder{})
	errBuffer := &strings.Builder{}
	command.SetErr(errBuffer)

	err := command.Execute()
	// 该站点不可达，必然报错；关键是错误里能看出 --host 与 --dir 已送达
	if err == nil {
		t.Fatal("不可达站点应报错")
	}
	combined := err.Error() + errBuffer.String()
	if !strings.Contains(combined, "mcp-gitea-contract.invalid") {
		t.Errorf("--host 未送达实现：%s", combined)
	}
}

// host 缺省且无任何候选（无 flag、无环境变量、不在 git 检出、凭据库为空）时必须
// **显式报错并指向修法**，不许静默猜一个站点——猜错会把会话接到别人的实例上。
func TestMCPGiteaRunEWithoutHostExplainsHowToFix(t *testing.T) {
	for _, key := range []string{"GITEA_HOST", "GITEA_ACCESS_TOKEN", "GITEA_ACCESS_TOKEN_FILE", "GITEA_MCP_BIN"} {
		t.Setenv(key, "")
	}
	t.Setenv("ASSISTANT_CREDENTIALS", t.TempDir()+"/credentials.json")

	command := newMCPGiteaCommand()
	command.SetArgs([]string{"--dir", t.TempDir()})
	command.SetOut(&strings.Builder{})
	errBuffer := &strings.Builder{}
	command.SetErr(errBuffer)

	err := command.Execute()
	if err == nil {
		t.Fatal("无候选 host 时应报错，不能静默继续")
	}
	message := err.Error()
	for _, want := range []string{"--host", "GITEA_HOST", "assistant login add"} {
		if !strings.Contains(message, want) {
			t.Errorf("错误应指出修法 %q：%s", want, message)
		}
	}
}
