package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fmt"

	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/repoinstall"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
	"github.com/Cosmic-Developers-Union/assistant/skills"
	"github.com/spf13/cobra"
)

// probeStub 是一个「什么都探不出来」的会话运行时探测器。
//
// 它钉住的正是「宿主装没装 claude、装的哪一版」都不该影响体检结论：探测失败时
// 会话运行时仍报 OK，只多一句「读不出版本」的提醒。用它替换真实的 Probe 后，
// 用例不再与宿主 PATH 上那个 claude 赛跑。
type probeStub struct{}

func (probeStub) Output(string, ...string) ([]byte, error) {
	return nil, os.ErrNotExist
}

// doctorGapCommand 造一个 doctor 命令骨架：只挂 auditServer 真的会去读的两个旗标
// （--config、--repo）。
//
// 缺了它们，auditServer 的第一件事就变成「读旗标并失败」——那是一条真实的错误
// 路径（cobra 的旗标表里没有这个名字），必须有单独用例去钉，不能被「旗标总是
// 存在」的假设掩盖。因此这里刻意不挂其它旗标，需要它们时由用例自己加。
func doctorGapCommand(t *testing.T, out *bytes.Buffer, configPath, repo string) *cobra.Command {
	t.Helper()
	command := &cobra.Command{Use: "doctor"}
	command.SetOut(out)
	command.SetErr(out)
	command.SetContext(t.Context())
	command.Flags().String("config", configPath, "")
	command.Flags().String("repo", repo, "")
	return command
}

// doctorGapConfig 把一个 instances 配置写进临时目录并返回路径。
//
// 体检的服务端分支全部由 config.json 驱动（通道 host、repos[]、default_provider），
// 用临时文件而不是环境变量意味着用例之间不会互相污染，也不会碰到真实配置目录。
func doctorGapConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// doctorGapGiteaConfig 造一份「一个 gitea 通道」的最小合法配置。
//
// 通道的 host 用被检站点本身，repos 用给定的仓库名，其余字段（reviewer/merger）
// 保持系统固定的两个评审身份——服务端体检把它们原样带进 AuditOptions。
func doctorGapGiteaConfig(t *testing.T, host, repoName string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"providers": map[string]any{"plain": map[string]any{}},
		"channels": []map[string]any{{
			"type":     "gitea",
			"host":     host,
			"reviewer": "ai",
			"merger":   "merge",
			"repos":    []string{repoName},
		}},
		"runtimes": map[string]any{"main": map[string]any{"main_agent": "main"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return doctorGapConfig(t, string(body))
}

// TestAuditServerReportsMissingFlags 断言命令上缺了 --config / --repo 旗标时
// auditServer 立刻把错误冒出来，而不是当成「取到了空值」继续往下走。
//
// 两条旗标读取在真实命令里恒成立，但它们是审计链路唯一的输入来源：一旦有人改了
// 命令装配（漏挂旗标、改了名字），读旗标会失败。此时若把错误当成空串吞掉，体检
// 会以「默认配置 + 未指定仓库」继续，报出来的是「未定位到 Gitea 实例」——
// 真正的原因（旗标没注册）被彻底掩盖。
func TestAuditServerReportsMissingFlags(t *testing.T) {
	isolateCredentials(t)
	out := &bytes.Buffer{}

	for _, testCase := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"缺 config 旗标", []string{"repo"}, "config"},
		{"缺 repo 旗标", []string{"config"}, "repo"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			command := &cobra.Command{Use: "doctor"}
			command.SetOut(out)
			command.SetErr(out)
			command.SetContext(t.Context())
			for _, flag := range testCase.flags {
				command.Flags().String(flag, "", "")
			}
			_, _, err := auditServer(command, "", &repoToolOptions{Dir: t.TempDir()}, 2, false)
			if err == nil {
				t.Fatalf("%s 时应报错", testCase.name)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("错误应点明缺失的旗标 %q：%v", testCase.want, err)
			}
		})
	}
}

// TestAuditServerReportsUnparsableConfig 断言 config.json 读不出来时审计失败，
// 而不是退回「没有配置」的跳过态。
//
// 「配置坏了」和「还没配」在跳过文案里长得一模一样，但处置完全不同：前者要修
// 文件，后者要 login add。若把解析错误吞成跳过，操作者会照着「未定位到 Gitea
// 实例」的方向去排查，而坏文件一直是坏的。
func TestAuditServerReportsUnparsableConfig(t *testing.T) {
	isolateCredentials(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte("{ 不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	command := doctorGapCommand(t, out, configPath, "acme/tool")
	_, _, err := auditServer(command, configPath, &repoToolOptions{Dir: t.TempDir()}, 2, false)
	if err == nil {
		t.Fatalf("坏配置应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "解析配置") {
		t.Errorf("错误应点明配置解析失败：%v", err)
	}
}

// TestAuditServerReportsMalformedRepoFlag 断言 --repo 不是 owner/name 形态时
// 审计直接报错，不拿半截目标去连站点。
//
// 目标定位已经把 --repo 原样当成仓库全名（配置里只有一个通道时，它不再需要命中
// repos[]）。于是一个写成 "noslash" 的输入会一路走到这里：必须在本地说清楚格式
// 不对，而不是让站点以 404 回应、把问题伪装成「仓库不存在或没权限」。
func TestAuditServerReportsMalformedRepoFlag(t *testing.T) {
	isolateCredentials(t)
	configPath := doctorGapGiteaConfig(t, "https://gitea.example.com", "")

	out := &bytes.Buffer{}
	command := doctorGapCommand(t, out, configPath, "noslash")
	_, _, err := auditServer(command, configPath, &repoToolOptions{Dir: t.TempDir()}, 2, false)
	if err == nil {
		t.Fatalf("非法 --repo 应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "owner/name") {
		t.Errorf("错误应给出正确格式：%v", err)
	}
}

// TestAuditServerReportsBrokenCredentialStoreWhenRepoFlagGiven 断言凭据库损坏且
// 仓库名来自旗标时审计报错，而不是当成「没有管理员令牌」回一个跳过。
//
// 这条路径与 credential_failure_gap_test.go 里那条互补：那条的配置里只有通道、
// 目标靠 route (d) 推出，这条把实例写在 config.json 里、仓库名由 --repo 给，
// 于是「查哪一份凭据」的决定因素是旗标解析结果而非平台推断。若把读取失败当成
// 查不到，报出来的是「缺少可用令牌」，操作者会去重新登录一遍——而登录本身也要
// 读同一个坏文件，于是陷入「登不上也说不清为什么」的循环。
func TestAuditServerReportsBrokenCredentialStoreWhenRepoFlagGiven(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(storePath, []byte("{ 不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CREDENTIALS", storePath)

	configPath := doctorGapGiteaConfig(t, "https://gitea.example.com", "acme/tool")
	out := &bytes.Buffer{}
	command := doctorGapCommand(t, out, configPath, "acme/tool")
	_, _, err := auditServer(command, configPath, &repoToolOptions{Dir: t.TempDir()}, 2, false)
	if err == nil {
		t.Fatalf("凭据库损坏应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), storePath) {
		t.Errorf("错误应点明坏掉的凭据库：%v", err)
	}
}

// doctorGapAdminCredential 给被检站点写一份 admin 用途凭据（体检读的就是它）。
//
// 通道自带的 token 字段不参与服务端体检——体检走的是凭据库的 admin 用途令牌
// （login/setup 写入的那份，表达平台身份）。用例必须沿这条真实链路造数据。
func doctorGapAdminCredential(t *testing.T, host string) {
	t.Helper()
	isolateCredentials(t)
	writePurposeCredentials(t, []credentials.Credential{{
		Host:    host,
		Purpose: credentials.PurposeAdmin,
		User:    "admin",
		Token:   "admin-token",
	}})
}

// doctorGapFakeSite 起一个「体检接口都回同一个状态」的假 Gitea，返回站点地址。
//
// 每一项检查都变成「读取被拒」（跳过而非失败），500 让第一项检查直接出错。
//
// 例外是 /api/v1/version：探测端点（status.ProbeGitea）拿它判定「这个 remote 是不是
// Gitea」，只有 200 才算数。环境变量单实例模式下仓库名只能靠这条探测得到，所以它
// 固定回 200；体检接口是否整段 403 都不影响这一步。
func doctorGapFakeSite(t *testing.T, statusCode int, message string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		t.Fatal(err)
	}
	version, err := json.Marshal(map[string]string{"version": "1.22.0"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/api/v1/version") {
			_, _ = writer.Write(version)
			return
		}
		writer.WriteHeader(statusCode)
		_, _ = writer.Write(body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// TestAuditServerSkipsWhenSiteDeniesRead 断言站点对所有读接口回 403 时审计返回
// 一组「跳过」的体检项与检查目标，而不是报错。
//
// 权限不足是体检要如实报告的结论（需要仓库管理员才能看分支保护），不是命令失败：
// 操作者要看到的是「哪些项没查成、为什么」，而 doctor 的整体结论由这些项一起决定。
// 同时这里钉住检查目标会被回填成 host/owner/repo——没有它，操作者不知道这份结论
// 是对谁下的。
func TestAuditServerSkipsWhenSiteDeniesRead(t *testing.T) {
	site := doctorGapFakeSite(t, http.StatusForbidden, "forbidden")
	doctorGapAdminCredential(t, site)

	configPath := doctorGapGiteaConfig(t, site, "acme/tool")
	out := &bytes.Buffer{}
	command := doctorGapCommand(t, out, configPath, "acme/tool")
	findings, target, err := auditServer(command, configPath, &repoToolOptions{Dir: t.TempDir()}, 2, false)
	if err != nil {
		t.Fatalf("403 不该报错：%v\n%s", err, out.String())
	}
	if target != site+"/acme/tool" {
		t.Errorf("检查目标 = %q，want %q", target, site+"/acme/tool")
	}
	if len(findings) == 0 {
		t.Fatalf("应返回体检项，got 空")
	}
	sawSecretCheck := false
	for _, finding := range findings {
		if finding.Status != status.AuditStatusSkipped {
			t.Errorf("%s 状态 = %q，want %q（权限不足应跳过而非失败）",
				finding.Path, finding.Status, status.AuditStatusSkipped)
		}
		if !strings.Contains(finding.Detail, "读取被拒") {
			t.Errorf("%s 的说明应点明读取被拒：%q", finding.Path, finding.Detail)
		}
		if strings.HasPrefix(finding.Path, "actions secret ") {
			sawSecretCheck = true
		}
	}
	if !sawSecretCheck {
		t.Errorf("体检应包含 merge 令牌的存在性检查：%+v", findings)
	}
}

// TestAuditServerReportsSiteFailure 断言站点读接口回 500 时审计把底层错误原样
// 冒出来（带上是哪一步失败的），而不是回一份「跳过」的体检结论。
//
// 服务端故障与权限不足必须分得开：跳过意味着「查不了但配置大概率没问题」，
// 而 5xx 意味着体检本身没做完。若把 5xx 也说成跳过，doctor 会以成功退出，
// 运维会把一个根本没查过的仓库当成体检通过。
func TestAuditServerReportsSiteFailure(t *testing.T) {
	site := doctorGapFakeSite(t, http.StatusInternalServerError, "boom")
	doctorGapAdminCredential(t, site)

	configPath := doctorGapGiteaConfig(t, site, "acme/tool")
	out := &bytes.Buffer{}
	command := doctorGapCommand(t, out, configPath, "acme/tool")
	findings, target, err := auditServer(command, configPath, &repoToolOptions{Dir: t.TempDir()}, 2, false)
	if err == nil {
		t.Fatalf("5xx 应报错（findings=%+v target=%q）：\n%s", findings, target, out.String())
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("错误应透出站点返回的正文：%v", err)
	}
	if !strings.Contains(err.Error(), "branch protections") {
		t.Errorf("错误应点明失败的检查步骤：%v", err)
	}
}

// TestAuditServerSkipsWithoutToken 断言站点上没有任何 admin 用途凭据时审计以
// 「缺少可用令牌」跳过，并且仍然回填检查目标。
//
// 这是 doctor 最常见的首次运行形态（config.json 有通道、凭据库还是空的）。跳过
// 而不是报错是刻意的：本地体检与凭据体检各有各的结论，doctor 不该因为没登录就
// 整条失败——但必须说清「已定位到谁、缺什么」，否则操作者不知道下一步该做什么。
func TestAuditServerSkipsWithoutToken(t *testing.T) {
	isolateCredentials(t)
	configPath := doctorGapGiteaConfig(t, "https://gitea.example.com", "acme/tool")
	out := &bytes.Buffer{}
	command := doctorGapCommand(t, out, configPath, "acme/tool")
	findings, target, err := auditServer(command, configPath, &repoToolOptions{Dir: t.TempDir()}, 2, false)
	if err != nil {
		t.Fatalf("缺令牌不该报错：%v\n%s", err, out.String())
	}
	if target != "https://gitea.example.com/acme/tool" {
		t.Errorf("检查目标 = %q，want host/owner/repo", target)
	}
	if len(findings) != 1 {
		t.Fatalf("应只有一条跳过项，got %+v", findings)
	}
	if findings[0].Status != status.AuditStatusSkipped {
		t.Errorf("状态 = %q，want %q", findings[0].Status, status.AuditStatusSkipped)
	}
	if !strings.Contains(findings[0].Detail, "缺少可用令牌") {
		t.Errorf("说明应点明缺令牌：%q", findings[0].Detail)
	}
}

// TestAuditServerSkipsWhenNoGiteaInstanceLocated 断言既没有可用配置、也没有能
// 证明自己是 Gitea 的 remote 时，审计以跳过收场并给出检查者能执行的两条出路。
//
// 这条是「在别人的仓库里跑 doctor」的常态：目录里没有 Gitea remote，配置文件也
// 不指向任何实例。此时不允许报错（本地结论仍然有效），但取消跳过必须能自己走
// 出来——所以文案里要带上 --repo 与 config.json 两个选项。
func TestAuditServerSkipsWhenNoGiteaInstanceLocated(t *testing.T) {
	isolateCredentials(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/acme/tool.git")

	out := &bytes.Buffer{}
	// 环境变量单实例模式：--config 留空才会回落到「没有配置文件」。显式指
	// 一个不存在的路径是另一回事（读不了就报错），那是配置错误而不是缺配置。
	command := doctorGapCommand(t, out, "", "")
	findings, target, err := auditServer(command, "",
		&repoToolOptions{Dir: dir}, 2, false)
	if err != nil {
		t.Fatalf("未定位到实例不该报错：%v\n%s", err, out.String())
	}
	if target != "" {
		t.Errorf("未定位到时不该给检查目标，got %q", target)
	}
	if len(findings) != 1 {
		t.Fatalf("应只有一条跳过项，got %+v", findings)
	}
	if findings[0].Status != status.AuditStatusSkipped || !strings.Contains(findings[0].Detail, "未定位到 Gitea 实例") {
		t.Errorf("跳过项说明不对：%+v", findings[0])
	}
	if !strings.Contains(findings[0].Detail, "--repo") || !strings.Contains(findings[0].Detail, "config.json") {
		t.Errorf("跳过项应给出可执行的出路：%q", findings[0].Detail)
	}
}

// TestPrintSessionFindingsReportsMissingClaude 断言找不到 claude 时以 MISSING
// 计入问题数，并且给出「怎么装 / 怎么指路径」的出路。
//
// 会话跑不起来的头号原因是运行时不在 PATH 上。报出来还不够：操作者需要知道
// assistant 允许用 --claude-bin / DISPATCH_CLAUDE_BIN 指一个绝对路径（容器与
// systemd 下 PATH 常常没有 ~/.local/bin）。这条同时钉住问题计数——doctor 的
// 退出码就靠它。
func TestPrintSessionFindingsReportsMissingClaude(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("PATH", t.TempDir())

	out := &bytes.Buffer{}
	configPath := doctorGapGiteaConfig(t, "https://gitea.example.com", "acme/tool")
	problems := printSessionFindings(out, configPath)
	if problems == 0 {
		t.Fatalf("找不到 claude 应计入问题：\n%s", out.String())
	}
	text := out.String()
	if !strings.Contains(text, "MISSING") || !strings.Contains(text, "找不到") {
		t.Errorf("应报出 claude 缺失：\n%s", text)
	}
	if !strings.Contains(text, "--claude-bin") || !strings.Contains(text, "DISPATCH_CLAUDE_BIN") {
		t.Errorf("应给出指定路径的两条出路：\n%s", text)
	}
}

// TestPrintSessionFindingsReportsUnversionedClaude 断言 PATH 上的 claude 跑不出
// 版本时仍然报 OK，但明确标注「读不出版本，可能不是 Claude Code CLI」。
//
// 这是真机上只可能在该路径暴露的形态：存在一个同名的可执行文件（旧版本、包装
// 脚本、别人的 claude），findings 不能因此把会话判成不可用（版本探测失败不等于
// 跑不起来），但也不能装作一切正常——操作者需要看到这个提醒。
//
// 探测缓存按**传入的名字**键控（doctor 传的是字面量 "claude"，不是 LookPath 的
// 结果），所以宿主真实 claude 一旦被别的用例探测过，本用例就会命中那条缓存、
// 测不出「读不出版本」的分支。注入探测缝之外还要清缓存，两者缺一不可。
func TestPrintSessionFindingsReportsUnversionedClaude(t *testing.T) {
	isolateCredentials(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// PATH 只留这个目录：缺口在那里，全部元素都非空，LookPath 不会回落到
	// $HOME/.local/bin 之类的兜底目录把宿主真实的 claude 找回来。
	t.Setenv("PATH", dir)
	previous := claudecfg.Probe
	claudecfg.Probe = probeStub{}
	claudecfg.ResetProbeCaches()
	t.Cleanup(func() {
		claudecfg.Probe = previous
		claudecfg.ResetProbeCaches()
	})

	out := &bytes.Buffer{}
	configPath := doctorGapGiteaConfig(t, "https://gitea.example.com", "acme/tool")
	_ = printSessionFindings(out, configPath)
	text := out.String()
	if !strings.Contains(text, "读不出版本") {
		t.Errorf("应标注读不出版本：\n%s", text)
	}
	if !strings.Contains(text, bin) {
		t.Errorf("应报出探测的那个二进制：\n%s", text)
	}
	if !strings.Contains(text, "OK") {
		t.Errorf("读不出版本不该判成 MISSING：\n%s", text)
	}
}

// TestPrintSessionFindingsReportsUnwritableConfigRoot 断言会话配置根建不出来时
// 以 MISSING 计入问题，并点明那个不可写的路径。
//
// 配置根是会话记录与 claude 全局配置的落点（CLAUDE_CONFIG_DIR 可覆盖）。它被一个
// 普通文件占住时，会话会在跑起来之后才失败，而那时报出来的是 claude 自己的写盘
// 错误——离真正的原因很远。体检必须提前在本地把这一步做掉。
func TestPrintSessionFindingsReportsUnwritableConfigRoot(t *testing.T) {
	isolateCredentials(t)
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("占位文件"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", blocked)

	out := &bytes.Buffer{}
	configPath := doctorGapGiteaConfig(t, "https://gitea.example.com", "acme/tool")
	problems := printSessionFindings(out, configPath)
	if problems == 0 {
		t.Fatalf("配置根不可写应计入问题：\n%s", out.String())
	}
	text := out.String()
	if !strings.Contains(text, blocked) || !strings.Contains(text, "不可写") {
		t.Errorf("应点明不可写的路径与原因：\n%s", text)
	}
}

// TestPrintSessionFindingsAcceptsUnusedConfigRoot 断言配置根建得出来时报 OK，
// 且文案点明它是 assistant 托管的会话落点、不是 ~/.claude。
//
// 「不是 ~/.claude」这句是刻意的：会话的凭据与配置不再跟随 claude 的登录态，
// 操作者若照旧去 ~/.claude 排查或往里塞凭据，会得到一份完全不生效的配置。
func TestPrintSessionFindingsAcceptsUnusedConfigRoot(t *testing.T) {
	isolateCredentials(t)
	target := filepath.Join(t.TempDir(), "claude-root")
	t.Setenv("CLAUDE_CONFIG_DIR", target)

	out := &bytes.Buffer{}
	configPath := doctorGapGiteaConfig(t, "https://gitea.example.com", "acme/tool")
	_ = printSessionFindings(out, configPath)
	text := out.String()
	if !strings.Contains(text, target) || !strings.Contains(text, "OK") {
		t.Errorf("可写的配置根应报 OK 并带路径：\n%s", text)
	}
	if !strings.Contains(text, "不是 ~/.claude") {
		t.Errorf("应点明不是 ~/.claude：\n%s", text)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("体检应顺手把配置根建出来：%v", err)
	}
}

// TestPrintSessionFindingsSkipsWithoutConfig 断言没有 config.json 时凭据一项以
// SKIPPED 跳过并且不计入问题（本地两项仍然照常体检）。
//
// 还没配过任何东西的机器上跑 doctor 是常见形态。此时「没有 AI 凭据」不是配置
// 缺陷而是待办，把它计成问题会让 doctor 在完全健康的新机器上以失败退出——
// 但文案必须告诉操作者下一步是 assistant login add。
func TestPrintSessionFindingsSkipsWithoutConfig(t *testing.T) {
	isolateCredentials(t)
	out := &bytes.Buffer{}
	// 留空表示「没有配置文件」：显式给一个不存在的路径会被当成读配置失败。
	problems := printSessionFindings(out, "")
	text := out.String()
	if !strings.Contains(text, "SKIPPED") || !strings.Contains(text, "login add") {
		t.Errorf("应跳过并给出下一步：\n%s", text)
	}
	// 本地两项（claude / 配置根）的问题数不受这一步影响：跳过不加问题
	if problems != 0 {
		t.Errorf("跳过不该计入问题，got %d：\n%s", problems, text)
	}
}

// TestPrintSessionFindingsReportsMissingCredential 断言 provider 解析成功但既没
// 带 api_key、进程环境也没有 Anthropic 凭据变量时，以 MISSING 计入问题并给出
// 两条可执行的出路（配 api_key 或注入环境变量）。
//
// 会话不再读 ~/.claude 登录态，所以「有 provider 但没凭据」是一个必须显式报出的
// 静默失败源：会话会起来，然后在第一次调用模型时认证失败。测试环境本身带着
// ANTHROPIC_* 变量，故这里显式清空，让这条路径只依赖被测代码。
func TestPrintSessionFindingsReportsMissingCredential(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	out := &bytes.Buffer{}
	configPath := doctorGapConfig(t, `{"default_provider":"plain","providers":{"plain":{}},"runtimes":{"main":{"main_agent":"main"}}}`)
	problems := printSessionFindings(out, configPath)
	if problems == 0 {
		t.Fatalf("缺 AI 凭据应计入问题：\n%s", out.String())
	}
	text := out.String()
	if !strings.Contains(text, "MISSING") || !strings.Contains(text, "没有 api_key") {
		t.Errorf("应报出缺 api_key：\n%s", text)
	}
	if !strings.Contains(text, "providers") || !strings.Contains(text, "ANTHROPIC_API_KEY") {
		t.Errorf("应给出两条可执行的出路：\n%s", text)
	}
}

// TestPrintSessionFindingsReportsProviderCredential 断言所选 provider 的凭据
// 就绪时报 OK，并点明凭据来自哪一层（provider env / settings / 进程环境）。
//
// 「凭据来源」是这套体检最有价值的一栏：同一个 ANTHROPIC_AUTH_TOKEN 来自 provider
// 配置还是进程环境，决定了换个运行时（容器 / systemd）还能不能跑。测试显式注入
// 进程环境变量，钉住「进程环境」这一条识别路径而不是碰运气命中宿主环境。
func TestPrintSessionFindingsReportsProviderCredential(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "gap-process-token")

	out := &bytes.Buffer{}
	configPath := doctorGapConfig(t, `{"default_provider":"plain","providers":{"plain":{}},"runtimes":{"main":{"main_agent":"main"}}}`)
	problems := printSessionFindings(out, configPath)
	if problems != 0 {
		t.Fatalf("凭据就绪不该计入问题，got %d：\n%s", problems, out.String())
	}
	text := out.String()
	if !strings.Contains(text, "OK") || !strings.Contains(text, "进程环境") {
		t.Errorf("应报 OK 并点明来源是进程环境：\n%s", text)
	}
}

// TestPrintServerFindingsFormatsAndCounts 断言服务端体检项按状态格式化输出，并且
// 只把「不是 ok/skipped」的项算作问题。
//
// 这是 doctor 最后一步的唯一实现：文案前缀（server:）、状态大写、以及 has detail
// 时补的「— 说明」。问题计数直接决定 doctor 的退出码，所以「跳过不算问题、其它
// 都算」这条规则必须有独立用例钉住——它同时覆盖了有说明与无说明两种排版。
func TestPrintServerFindingsFormatsAndCounts(t *testing.T) {
	out := &bytes.Buffer{}
	problems := printServerFindings(out, []status.AuditFinding{
		{Path: "branch protection main", Status: status.AuditStatusOK},
		{Path: "labels", Status: status.AuditStatusSkipped, Detail: "读取被拒（需要仓库管理员）"},
		{Path: "collaborator ai", Status: status.AuditStatusOutdated, Detail: "协作者权限不足"},
		{Path: "actions secret MERGE_TOKEN", Status: status.AuditStatusMissing},
	})

	if problems != 2 {
		t.Errorf("问题数 = %d，want 2（ok 与 skipped 不算问题）：\n%s", problems, out.String())
	}
	text := out.String()
	for _, want := range []string{
		"OK        server: branch protection main\n",
		"SKIPPED   server: labels — 读取被拒（需要仓库管理员）\n",
		"OUTDATED  server: collaborator ai — 协作者权限不足\n",
		"MISSING   server: actions secret MERGE_TOKEN\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("输出缺少 %q：\n%s", want, text)
		}
	}
}

// TestPrintServerFindingsEmpty 断言没有体检项时输出为空、问题数为零。
//
// 服务端检查被整体跳过（未定位到实例）时传进来的就是空切片。这里不能凭空造出
// 一行输出，也不能把「没检查」算成问题——doctor 的结论由明确的问题项组成。
func TestPrintServerFindingsEmpty(t *testing.T) {
	out := &bytes.Buffer{}
	if problems := printServerFindings(out, nil); problems != 0 {
		t.Errorf("问题数 = %d，want 0", problems)
	}
	if out.Len() != 0 {
		t.Errorf("不该有输出：%q", out.String())
	}
}

// TestRunDoctorReportsLocalInspectionFailure 断言本地体检起不来时 runDoctor 立刻
// 返回该错误，不继续去查会话与服务端。
//
// 本地体检要定位用户目录（~/.codex 等 Codex 配置位）。用户目录取不出来时
// （容器里 HOME 未设置）继续跑会得到一份「本地一切正常」的假结论——因为找不到
// 配置文件会被当成没装过脚手架，而真正的原因是根本看不了那个目录。
func TestRunDoctorReportsLocalInspectionFailure(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("HOME", "")

	out := &bytes.Buffer{}
	command := doctorGapCommand(t, out, filepath.Join(t.TempDir(), "absent.json"), "")
	err := runDoctor(command, filepath.Join(t.TempDir(), "absent.json"), &repoToolOptions{Dir: t.TempDir()}, 2, false)
	if err == nil {
		t.Fatalf("本地体检失败应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "定位用户目录") {
		t.Errorf("错误应点明用户目录定位失败：%v", err)
	}
}

// TestRunDoctorPropagatesServerInspectionFailure 断言服务端体检出错时 runDoctor
// 把错误原样冒出来，而不是继续打印一份不完整的体检报告。
//
// 本地与会话两段已经打印过了，服务端这段是决定 doctor 结论的最后一段。若把它的
// 错误吞掉，命令会以「发现 0 处配置问题」退出，而实际上一项服务端检查都没做完
// ——运维会据此认为仓库配置是健康的。
func TestRunDoctorPropagatesServerInspectionFailure(t *testing.T) {
	site := doctorGapFakeSite(t, http.StatusInternalServerError, "boom")
	doctorGapAdminCredential(t, site)

	configPath := doctorGapGiteaConfig(t, site, "acme/tool")
	out := &bytes.Buffer{}
	command := doctorGapCommand(t, out, configPath, "acme/tool")
	err := runDoctor(command, configPath, &repoToolOptions{Dir: t.TempDir()}, 2, false)
	if err == nil {
		t.Fatalf("服务端体检失败应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("错误应透出站点故障：%v", err)
	}
	if !strings.Contains(out.String(), "会话运行时") {
		t.Errorf("本地与会话两段应已打印：\n%s", out.String())
	}
}

// TestRunDoctorReportsProblemsWhenSiteDeniesRead 断言服务端检查只剩「跳过」时
// doctor 仍然以问题退出，并把检查目标打印出来。
//
// 跳过项不计入问题，但会话那块通常还缺着东西；这条钉的是「检查目标」那一行确实
// 会打出来、以及问题汇总是跨三段相加的：本地/会话/服务端任何一处不为零，命令就
// 必须以失败收场（doctor 是给 CI 与人工排查用的门禁，不能含糊通过）。
func TestRunDoctorReportsProblemsWhenSiteDeniesRead(t *testing.T) {
	site := doctorGapFakeSite(t, http.StatusForbidden, "forbidden")
	doctorGapAdminCredential(t, site)
	t.Setenv("PATH", t.TempDir())

	configPath := doctorGapGiteaConfig(t, site, "acme/tool")
	out := &bytes.Buffer{}
	command := doctorGapCommand(t, out, configPath, "acme/tool")
	err := runDoctor(command, configPath, &repoToolOptions{Dir: t.TempDir()}, 2, false)
	if err == nil {
		t.Fatalf("有配置问题应以失败退出：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "处配置问题") {
		t.Errorf("错误应汇总问题数：%v", err)
	}
	text := out.String()
	if !strings.Contains(text, "# 服务端检查目标："+site+"/acme/tool") {
		t.Errorf("应打印检查目标：\n%s", text)
	}
	if !strings.Contains(text, "server: ") {
		t.Errorf("应打印服务端体检项：\n%s", text)
	}
}

// embeddedSkillReview 是仓库内嵌的 review 技能内容，体检就是拿它逐字对照产物。
var embeddedSkillReview = skills.Review

// TestRunDoctorReportsSuccessWithoutToken 断言体检真的零缺陷时 runDoctor 返回 nil：
// 仓库已按当前模板装好、会话解析出 provider 凭据、服务端未定位到实例（跳过不算问题）。
//
// 这是 doctor 唯一的「成功」出口，门禁的正确性全压在它身上：任何一段把「跳过」
// 或「无缺陷」也计入问题，命令就会永远以失败退出，操作者只能开始忽略它。
// 仓库侧必须真的装过脚手架——裸检出本就该报 10 处 MISSING，那不是缺陷而是 doctor
// 的正常结论，只有装好之后才谈得上「零问题」。
func TestRunDoctorReportsSuccessWithoutToken(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "gap-process-token")
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/acme/tool.git")
	// 先按当前模板装一遍，再体检：doctor 比对的期望内容正是 install 写下的那一份
	install := &repoToolOptions{
		Dir:          dir,
		Tools:        []string{"claude", "opencode", "codex"},
		CodexPath:    filepath.Join(t.TempDir(), "config.toml"),
		SkillsSource: "none", // 技能产物由 skills CLI 落到别处，install 用 "none" 跳过
	}
	if err := repoinstall.Install(t.Context(), install.repoOptions(doctorGapCommand(t, &bytes.Buffer{}, "", ""))); err != nil {
		t.Fatalf("准备阶段 install 失败：%v", err)
	}
	// 技能产物由 skills CLI 落盘（install 只是转调它），所以上面用 "none" 跳过了；
	// 体检对照的却正是仓库内嵌的那份内容，这里照它补上，仓库才算真的装齐。
	for _, relative := range []string{".claude/skills/review/SKILL.md", ".agents/skills/review/SKILL.md"} {
		path := filepath.Join(dir, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(embeddedSkillReview), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 配置必须真实存在：resolveInstanceFile 读不存在的路径会直接报错，而本条的
	// 目的是让命令走到「一切通过」这个出口，不是去测配置缺失。
	configPath := doctorGapGiteaConfig(t, "https://gitea.example.com", "acme/tool")

	out := &bytes.Buffer{}
	command := doctorGapCommand(t, out, configPath, "")
	err := runDoctor(command, configPath, install, 2, false)
	if err != nil {
		t.Fatalf("全通过时不该报错：%v\n%s", err, out.String())
	}
	for _, status := range []string{"MISSING", "OUTDATED", "UNMANAGED"} {
		if strings.Contains(out.String(), status) {
			t.Errorf("不该有 %s 项：\n%s", status, out.String())
		}
	}
}

// TestRunDoctorCountsLocalFindingsInProblemTotal 断言裸检出（未装脚手架）时 runDoctor
// 以本地缺陷报错，且问题总数与逐条列出的非 OK 项严格一致。
//
// 首次接入 assistant 的仓库就是这个形态：问题清单必须是自己数得清的——操作者照着
// 报出的条数逐项 install，条数和文案对不上就等于给了他一份不知道是否完整的清单。
func TestRunDoctorCountsLocalFindingsInProblemTotal(t *testing.T) {
	isolateCredentials(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "gap-process-token")
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/acme/tool.git")
	configPath := doctorGapGiteaConfig(t, "https://gitea.example.com", "acme/tool")

	out := &bytes.Buffer{}
	command := doctorGapCommand(t, out, configPath, "")
	err := runDoctor(command, configPath,
		&repoToolOptions{Dir: dir, CodexPath: filepath.Join(t.TempDir(), "config.toml")}, 2, false)
	if err == nil {
		t.Fatalf("未装脚手架的仓库应报出问题：\n%s", out.String())
	}
	listed := 0
	for _, line := range strings.Split(out.String(), "\n") {
		for _, status := range []string{"MISSING", "OUTDATED", "UNMANAGED", "LEGACY"} {
			if strings.HasPrefix(line, status) {
				listed++
				break
			}
		}
	}
	if listed == 0 {
		t.Fatalf("应逐条列出非 OK 项：\n%s", out.String())
	}
	want := fmt.Sprintf("发现 %d 处配置问题", listed)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("问题总数应等于列出的条数：err=%v，列了 %d 条\n%s", err, listed, out.String())
	}
}

// TestAuditServerUsesEnvCredentials 断言没有 config.json 时审计走单实例环境变量
// 模式：GITEA_HOST/GITEA_ACCESS_TOKEN 提供站点与令牌，remote 提供仓库名。
//
// 这是 CI 里跑 doctor 的形态（配置不入库，只有环境变量）。它与 config.json 模式的
// 差别不只是凭据来源：reviewer/merger 在环境模式下拿不到，服务端体检必须照样
// 能跑完并给出结论，否则 CI 上的体检会整段跳过。
func TestAuditServerUsesEnvCredentials(t *testing.T) {
	isolateCredentials(t)
	site := doctorGapFakeSite(t, http.StatusForbidden, "forbidden")
	t.Setenv("GITEA_HOST", site)
	t.Setenv("GITEA_ACCESS_TOKEN", "env-token")

	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "remote", "add", "origin", site+"/acme/tool.git")

	out := &bytes.Buffer{}
	// 环境变量单实例模式：--config 留空才会回落到「没有配置文件」。显式指
	// 一个不存在的路径是另一回事（读不了就报错），那是配置错误而不是缺配置。
	command := doctorGapCommand(t, out, "", "")
	findings, target, err := auditServer(command, "",
		&repoToolOptions{Dir: dir}, 2, false)
	if err != nil {
		t.Fatalf("环境变量模式不该报错：%v\n%s", err, out.String())
	}
	if target != site+"/acme/tool" {
		t.Errorf("检查目标 = %q，want %q", target, site+"/acme/tool")
	}
	if len(findings) == 0 {
		t.Fatal("应返回体检项")
	}
	for _, finding := range findings {
		if finding.Status != status.AuditStatusSkipped {
			t.Errorf("%s 状态 = %q，want %q", finding.Path, finding.Status, status.AuditStatusSkipped)
		}
	}
}

// TestAuditServerSkipsEnvModeWithoutCredentials 断言 remote 是 Gitea 但环境变量
// 没给全时，审计以「缺少凭据」跳过并回填检查目标；同时钉住 Gitea 地址取自环境
// 变量而不是 remote。
//
// 与 config.json 模式的区别在于：排除到这里说明 remote 已经确认是 Gitea（探测
// 通过），所以「缺凭据」是一条明确的待办，文案里必须点出可用哪两个变量或改用
// config.json——否则 CI 上的人只会看到一句「跳过」。
func TestAuditServerSkipsEnvModeWithoutCredentials(t *testing.T) {
	isolateCredentials(t)
	site := doctorGapFakeSite(t, http.StatusForbidden, "forbidden")
	t.Setenv("GITEA_HOST", "")
	t.Setenv("GITEA_ACCESS_TOKEN", "")

	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "remote", "add", "origin", site+"/acme/tool.git")

	out := &bytes.Buffer{}
	// 环境变量单实例模式：--config 留空才会回落到「没有配置文件」。显式指
	// 一个不存在的路径是另一回事（读不了就报错），那是配置错误而不是缺配置。
	command := doctorGapCommand(t, out, "", "")
	findings, target, err := auditServer(command, "",
		&repoToolOptions{Dir: dir}, 2, false)
	if err != nil {
		t.Fatalf("缺凭据不该报错：%v\n%s", err, out.String())
	}
	if target != site+"/acme/tool" {
		t.Errorf("检查目标 = %q，want remote 的 host/repo", target)
	}
	if len(findings) != 1 {
		t.Fatalf("应只有一条跳过项，got %+v", findings)
	}
	detail := findings[0].Detail
	if !strings.Contains(detail, "GITEA_HOST") || !strings.Contains(detail, "config.json") {
		t.Errorf("跳过项应给出两条出路：%q", detail)
	}
}

// TestAuditServerReportsMissingAdminToken 断言目标实例认得出来、凭据库里却没有
// admin 令牌时，审计退回一条明确的「缺少可用令牌」跳过项，而不是继续往下走。
//
// 体检的凭据来自凭据库（login add / setup 写入），不是 config.json 里的通道字段：
// 仓库已经在配置里，用户会理所当然地认为「都配好了」。若此处不拦住，空令牌会被
// 送进客户端，回来的是站点上的 401——排查方向指向权限，真正的原因是这台机器上
// 从没登录过该站点。这条把「实例在配置里」与「凭据在凭据库里」分开构造，钉住
// 体检认的是后者。
func TestAuditServerReportsMissingAdminToken(t *testing.T) {
	isolateCredentials(t)
	site := doctorGapFakeSite(t, http.StatusForbidden, "forbidden")

	// 凭据库里这个站点一条 admin 令牌都没有（例如从没 login add 过）。体检必须
	// 明确报「缺令牌」而不是把空令牌交给客户端——空令牌换来的是一串 401，排查方向
	// 会指向站点权限，而真正的原因是这台机器上根本没登录过。

	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "remote", "add", "origin", site+"/acme/tool.git")
	configPath := doctorGapGiteaConfig(t, site, "acme/tool")

	out := &bytes.Buffer{}
	command := doctorGapCommand(t, out, configPath, "")
	findings, target, err := auditServer(command, configPath, &repoToolOptions{Dir: dir}, 2, false)
	if err != nil {
		t.Fatalf("缺令牌不该报错：%v\n%s", err, out.String())
	}
	if target != site+"/acme/tool" {
		t.Errorf("检查目标 = %q，want %q", target, site+"/acme/tool")
	}
	if len(findings) != 1 {
		t.Fatalf("应只有一条跳过项，got %+v", findings)
	}
	if detail := findings[0].Detail; !strings.Contains(detail, "缺少可用令牌") {
		t.Errorf("跳过项应说明缺令牌：%q", detail)
	}
}

// TestPrintSessionFindingsStopsAtProviderResolutionError 断言 provider 解析失败时
// 体检以 UNMANAGED 停住、把失败计入问题数，绝不继续往下报「没有 api_key」。
//
// 解析失败意味着这份 provider 配置根本不能生效。若这时仍去报「没有 api_key」，操作
// 者会去补一把用不上的钥匙，而真正要修的是 provider 定义本身——报错停在哪一步，
// 决定了排查方向对不对。这里用 openai 预设（没有 Anthropic 兼容端点、必须给
// ANTHROPIC_BASE_URL）造出解析失败：缺了它 config.json 就过不了校验。
//
// 注意这条走的是 resolveInstanceFile 的「配置坏」分支（Load 出错、Parse 出来的
// 不是空骨架，于是原样上报错误），而不是 printSessionFindings 里那行 UNMANAGED——
// 后者要求 instances.Load 成功、EffectiveOverrides 却失败，生产上走不到（见
// docs/coverage-gaps.md）。本用例钉的是操作者真正会看到的那条路径。
func TestPrintSessionFindingsStopsAtProviderResolutionError(t *testing.T) {
	isolateCredentials(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	// 必须带上 channels：只有 providers/default_provider 的配置会被判成空骨架，
	// 那样走的是 SKIPPED，校验根本轮不到。
	config := `{"default_provider":"openai","providers":{"openai":{"api_key":"sk-test"}},` +
		`"channels":[{"type":"telegram","bot_token":"t"}]}`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	// 宿主若有 ANTHROPIC_BASE_URL，openai 就能解析成功，这条路径就走不到了
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	out := &bytes.Buffer{}
	problems := printSessionFindings(out, configPath)
	text := out.String()
	if !strings.Contains(text, "SKIPPED") {
		t.Errorf("配置坏掉时凭据检查应停在这条分支：\n%s", text)
	}
	if strings.Contains(text, "没有 api_key") {
		t.Errorf("配置还没解析成功，不该报「没有 api_key」：\n%s", text)
	}
	if !strings.Contains(text, "会话 AI 凭据") {
		t.Errorf("凭据检查应停在这条分支：\n%s", text)
	}
	if problems != 0 {
		t.Errorf("这条分支不计问题数（配置问题由别处报）：problems=%d\n%s", problems, text)
	}
}

// TestPrintSessionFindingsReportsMissingProviderKey 断言 provider 认得出来、但既
// 没有 api_key 也没有覆盖时以 MISSING 计入问题，并给出取钥匙的指引。
//
// 这是「配置写对了、凭据还没配」的常见初始状态：提示必须指向
// claudecfg.MissingCredentialHint 那条出路，否则操作者会去翻 provider 定义找不着头绪。
func TestPrintSessionFindingsReportsMissingProviderKey(t *testing.T) {
	isolateCredentials(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	configPath := filepath.Join(dir, "config.json")
	// provider 名认得出来（minimax 是内置预设），但既没有 api_key 也没有 env 覆盖
	config := `{"default_provider":"minimax","providers":{"minimax":{}},` +
		`"channels":[{"type":"telegram","bot_token":"t","admin_users":["1"]}]}`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	// 宿主环境里的令牌会让 CredentialSource 认定「有凭据」，这条分支就走不到了
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	out := &bytes.Buffer{}
	problems := printSessionFindings(out, configPath)
	text := out.String()
	if !strings.Contains(text, "MISSING") || !strings.Contains(text, "没有 api_key") {
		t.Errorf("没有凭据应判 MISSING：\n%s", text)
	}
	if !strings.Contains(text, claudecfg.MissingCredentialHint) {
		t.Errorf("应给出取钥匙的指引：\n%s", text)
	}
	if problems == 0 {
		t.Errorf("MISSING 应计入问题数：\n%s", text)
	}
}
