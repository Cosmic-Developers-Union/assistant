package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/runcfg"
	"github.com/spf13/cobra"
)

// migrateGapCommand 造一个只接输出缓冲的 config migrate 命令：runConfigMigrate
// 的每一条失败路径报什么、成功路径写没写都必须能被断言，否则这些分支只能靠
// 「跑完没崩」来判断，而那正是漏掉「凭据/配置写坏」这类事故的地方。
func migrateGapCommand(out *bytes.Buffer) *cobra.Command {
	command := newConfigMigrateCommand(new(string))
	command.SetOut(out)
	command.SetErr(out)
	command.SetArgs(nil)
	return command
}

// writeMigrateConfig 往临时目录写一份能通过校验的 config.json，返回路径。
//
// runConfigMigrate 的多数分支都要求配置先能被解析（否则会在更早的
// resolveInstanceFile 上失败），所以每条用例都得从一份「读得出来」的配置出发。
func writeMigrateConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// monitorSite 是所有 run.yaml 夹具都必须带上的站点段。
//
// runcfg.Load 会跑 Validate，而 Validate 要求 monitor 至少有一个站点；不带它就
// 到不了被测的那条分支（会被一句「monitor 不能为空」提前拦下），用例看着在测
// 迁移逻辑，其实只是又测了一遍 run 配置解析。
const monitorSite = "monitor:\n  https://gap.example.com:\n    token: \"\"\n"

// TestRunConfigMigrateReportsUnparsableConfig 断言现有 config.json 读不出来时
// migrate 立刻失败，不打印「无需迁移」也不去读 run.yaml。
//
// 「无需迁移」是一句结论；配置本身坏掉时给出这句结论，操作者会以为迁移已经
// 处理完了，而真正的问题（语法错）一个字都没被提到——之后每条命令都在同一个
// 坏配置上打转。
func TestRunConfigMigrateReportsUnparsableConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte("{ 不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 同目录放一份 run.yaml：若实现继续往下走，它会去读这份文件
	if err := os.WriteFile(filepath.Join(dir, runcfg.FileName), []byte("root: /srv/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	command := migrateGapCommand(out)
	if err := runConfigMigrate(command, configPath, &configMigrateOptions{}); err == nil {
		t.Fatalf("坏配置应报错：\n%s", out.String())
	}
	if strings.Contains(out.String(), "无需迁移") {
		t.Errorf("不该把读不出来说成无需迁移：\n%s", out.String())
	}
}

// TestRunConfigMigrateReportsUnwritableConfigPath 断言配置落点取不出来（当前目录
// 已删、也没有 --config / ASSISTANT_CONFIG）时报错，而不是拿一个空路径去写。
//
// 空路径会让 saveConfig 把配置写到一个由 os.WriteFile 决定的位置——用户重启后
// 在哪儿都找不到它，而命令已经报告「已导入」。
func TestRunConfigMigrateReportsUnwritableConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CONFIG", "")
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "relative/path")
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	command := migrateGapCommand(out)
	if err := runConfigMigrate(command, "", &configMigrateOptions{}); err == nil {
		t.Errorf("配置落点取不出来时应报错：\n%s", out.String())
	}
}

// TestRunConfigMigrateFindsRunYamlBesideConfig 断言没给 --run、也没设
// ASSISTANT_RUN 时，migrate 会去 config.json 同目录找 run.yaml 并导入。
//
// 这是老用户最常见的处境：run.yaml 和 config.json 一直放在一起，他们不会去
// 记 --run 这个旗标。找不到就打印「无需迁移」，等于把他们真正的旧参数留在原地
// 假装已经处理完了。
func TestRunConfigMigrateFindsRunYamlBesideConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("ASSISTANT_RUN", "")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"runtimes": {"main": {"main_agent": "main"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, runcfg.FileName), []byte(monitorSite+"repos-dir: /srv/repos\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	if err := runConfigMigrate(migrateGapCommand(out), configPath, &configMigrateOptions{}); err != nil {
		t.Fatalf("同目录的 run.yaml 应被找到：%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "+ runtimes.main.repos_dir = /srv/repos") {
		t.Errorf("应导入同目录 run.yaml 的 repos_dir：\n%s", out.String())
	}
}

// TestRunConfigMigrateReportsRunYamlStatFailure 断言 config.json 同目录的
// run.yaml 存在但 stat 失败时报错，而不是当成「没有 run.yaml」打印无需迁移。
//
// 权限/损坏的挂载点会让这一步 stat 失败；把它当成不存在，用户会得到「无需迁移」
// 而旧参数其实还在那个读不出来的文件里——迁移被静默跳过了。
func TestRunConfigMigrateReportsRunYamlStatFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("ASSISTANT_RUN", "")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"runtimes": {"main": {"main_agent": "main"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 把 config.json 放进子目录，run.yaml 的位置摆一个普通文件（即上面那条
	// 写进去的占位串，这里删除它），改成让 config.json 同目录的 run.yaml 指向
	// 一个 stat 必然非 IsNotExist 失败的路径。
	sub := filepath.Join(dir, "conf")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(sub, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"runtimes": {"main": {"main_agent": "main"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	// 显式 --run 指到一个「普通文件之下的路径」：stat 以 ENOTDIR 失败（非 IsNotExist）
	err := runConfigMigrate(migrateGapCommand(out), configPath, &configMigrateOptions{
		RunPath: filepath.Join(dir, runcfg.FileName, "nested.yaml"),
	})
	if err == nil || strings.Contains(out.String(), "无需迁移") {
		t.Fatalf("run.yaml 读不出来应报错：%v\n%s", err, out.String())
	}
}

// TestRunConfigMigrateReportsRunYamlAsDirectory 断言同目录的 run.yaml 位置上
// 是一个目录时 migrate 报错，而不是把它当成可以导入的文件。
//
// 目录名恰好叫 run.yaml 是容器挂载/解包常见的手误：os.Stat 成功、进入
// runcfg.Load 会以「是目录」失败。这里必须冒到错误上（并点明 run 配置），
// 而不是被当成「没有 run.yaml」静默跳过。
func TestRunConfigMigrateReportsRunYamlAsDirectory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("ASSISTANT_RUN", "")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"runtimes": {"main": {"main_agent": "main"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, runcfg.FileName), 0o755); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	err := runConfigMigrate(migrateGapCommand(out), configPath, &configMigrateOptions{})
	if err == nil {
		t.Fatalf("run.yaml 是目录时应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "run 配置") {
		t.Errorf("错误应点明是 run 配置的问题：%v", err)
	}
}

// TestRunConfigMigrateReportsEmptyLegacyRun 断言 run.yaml 能读、但里面没有任何
// 可导入内容时打印「没有带来新内容：无需迁移」并以成功退出，且不写任何文件。
//
// 这是「旧文件只剩注释/空字段」的形态（root 等在 Load 时已填成缺省值，但
// merge 判据看的是「值非空且原先为空」）。把它当成失败会让用户以为迁移出错；
// 而它恰恰应该是一次干净的空操作。
func TestRunConfigMigrateReportsEmptyLegacyRun(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("ASSISTANT_RUN", "")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"runtimes": {"main": {"main_agent": "main"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 只给 monitor 但不给 token/repos，provider 也不给：mergeLegacyRun 无变更
	runPath := filepath.Join(dir, "run.yaml")
	if err := os.WriteFile(runPath, []byte("monitor:\n  https://empty.example.com:\n    token: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	if err := runConfigMigrate(migrateGapCommand(out), configPath, &configMigrateOptions{RunPath: runPath}); err != nil {
		t.Fatalf("没有新内容不该报错：%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "没有带来新内容：无需迁移") {
		t.Errorf("应说明没有新内容：\n%s", out.String())
	}
	// 无变更 = 不动任何文件：run.yaml 仍在原位（不能被改名成 .migrated）
	if _, err := os.Stat(runPath); err != nil {
		t.Errorf("无变更时不该动 run.yaml：%v", err)
	}
}

// TestRunConfigMigrateDryRunWritesNothing 断言 --dry-run 时打印变更清单与
// 「未写入任何文件」，而 config.json 与 run.yaml 都保持原样。
//
// 这是操作者在真正迁移前的唯一预演手段：若 dry-run 照样落盘，配置会被改掉、
// run.yaml 会被改名——预演本身就成了一次不可回退的迁移。
func TestRunConfigMigrateDryRunWritesNothing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	original := `{"runtimes": {"main": {"main_agent": "main"}}}` + "\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	runPath := filepath.Join(dir, "run.yaml")
	if err := os.WriteFile(runPath, []byte(monitorSite+"repos-dir: /srv/dry\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	if err := runConfigMigrate(migrateGapCommand(out), configPath, &configMigrateOptions{RunPath: runPath, DryRun: true}); err != nil {
		t.Fatalf("dry-run 不该报错：%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "dry-run：未写入任何文件") {
		t.Errorf("应说明未写入：\n%s", out.String())
	}
	if !strings.Contains(out.String(), "+ runtimes.main.repos_dir = /srv/dry") {
		t.Errorf("dry-run 仍应打印变更清单：\n%s", out.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Errorf("dry-run 不该改配置：\n%s", data)
	}
	if _, err := os.Stat(runPath); err != nil {
		t.Errorf("dry-run 不该动 run.yaml：%v", err)
	}
}

// TestRunConfigMigrateReportsInvalidMergedConfig 断言合并结果过不了校验时 migrate
// 报错且不落盘，也不把 run.yaml 改名。
//
// 合并可能把一份原本合法的配置变成非法的（monitor 站点缺 reviewer/merger、
// host 是相对路径等）。若先落盘再校验失败，用户会得到一份连自己都启动不了的
// config.json，而旧 run.yaml 已被改名——两个来源都不可用了。
func TestRunConfigMigrateReportsInvalidMergedConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"runtimes": {"main": {"main_agent": "main"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPath := filepath.Join(dir, "run.yaml")
	// monitor 的键不是 http(s) 地址：upsertGiteaChannel 会照它建通道，校验必拒
	if err := os.WriteFile(runPath, []byte("monitor:\n  not-a-site:\n    repos:\n      - acme/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	err := runConfigMigrate(migrateGapCommand(out), configPath, &configMigrateOptions{RunPath: runPath})
	if err == nil {
		t.Fatalf("合并结果非法时应报错：\n%s", out.String())
	}
	if _, statErr := os.Stat(runPath); statErr != nil {
		t.Errorf("失败时不该改名 run.yaml：%v", statErr)
	}
	data, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(data), "not-a-site") {
		t.Errorf("失败时不该写出非法配置：\n%s", data)
	}
}

// TestRunConfigMigrateReportsRenameFailure 断言 config.json 写成功但旧 run.yaml
// 改名失败时报错，并明确说出「配置已写入」。
//
// 改名是收尾动作：它失败时迁移其实已经生效了一半。报一句笼统的失败会让用户
// 以为什么都没发生而重跑一次；说明白「配置已写入、旧文件还在」才知道该删哪个。
func TestRunConfigMigrateReportsRenameFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"runtimes": {"main": {"main_agent": "main"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// run.yaml 放进一个子目录，而 .migrated 那个位置被目录占住 → Rename 必失败
	runDir := filepath.Join(dir, "legacy")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runPath := filepath.Join(runDir, runcfg.FileName)
	if err := os.WriteFile(runPath, []byte(monitorSite+"repos-dir: /srv/rename\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runPath+".migrated", 0o755); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	err := runConfigMigrate(migrateGapCommand(out), configPath, &configMigrateOptions{RunPath: runPath})
	if err == nil {
		t.Fatalf("改名失败时应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "config.json 已写入") {
		t.Errorf("错误应说明配置已写入：%v", err)
	}
}

// TestMergeLegacyRunReportsRuntimeResolutionFailure 断言配置里的 runtime 有歧义时
// mergeLegacyRun 报错，而不是随便挑一个来填。
//
// 多个 runtime 且没有 default_runtime 时「填哪个」是未定义的；挑错了会把旧
// run.yaml 的数据根塞进另一个通道的运行时，之后受管克隆与状态库全落在错误的树下。
func TestMergeLegacyRunReportsRuntimeResolutionFailure(t *testing.T) {
	file := &instances.File{Runtimes: map[string]instances.Runtime{
		"one": {MainAgent: "main"},
		"two": {MainAgent: "main"},
	}}
	legacy, err := runcfg.Load(writeLegacyRun(t, "root: /rooted\nrepos-dir: /rooted/amb\n"+monitorSite))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mergeLegacyRun(file, legacy); err == nil {
		t.Error("runtime 有歧义时应报错")
	}
}

// writeLegacyRun 把一段 run.yaml 内容写进临时文件并返回路径（原样写入，不补任何
// 段落）。调用方要自己保证内容能过 runcfg.Validate——至少带一个 monitor 站点，
// 否则会被一句「monitor 不能为空」提前拦下，用例看着在测迁移逻辑，其实只是又测了
// 一遍 run 配置解析。
func writeLegacyRun(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), runcfg.FileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMergeLegacyRunImportsEveryRuntimeField 断言 run.yaml 的八个运行参数
// 逐个进 runtimes.main（root/repos_dir/review_root/review_name_template/
// sessions_dir/sessions_name_template/state_dir/state_file），并逐条出现在
// 变更清单里。
//
// 变更清单是操作者唯一能看到「到底导入了什么」的地方；漏掉某个字段会让旧
// 路径配置静默失效（比如 state_dir 没迁，状态库换了地方，调度状态全丢），
// 而迁移报告却显示一切正常。
func TestMergeLegacyRunImportsEveryRuntimeField(t *testing.T) {
	file := &instances.File{}
	legacy, err := runcfg.Load(writeLegacyRun(t, monitorSite+strings.Join([]string{
		"root: /srv/root",
		"repos-dir: /srv/root/repos",
		"review-root: /srv/root/review",
		"review-name-template: \"{name}-{index}\"",
		"sessions-dir: /srv/root/sessions",
		"sessions-name-template: \"{name}/{session-id}.jsonl\"",
		"state-dir: /srv/root/state",
		"state-file: /srv/root/state/state.sqlite3",
	}, "\n")+"\n"))
	if err != nil {
		t.Fatal(err)
	}

	changes, err := mergeLegacyRun(file, legacy)
	if err != nil {
		t.Fatal(err)
	}
	target := file.Runtimes[instances.DefaultRuntimeName]
	for label, got := range map[string]string{
		"root":                   target.Root,
		"repos_dir":              target.ReposDir,
		"review_root":            target.ReviewRoot,
		"review_name_template":   target.ReviewNameTemplate,
		"sessions_dir":           target.SessionsDir,
		"sessions_name_template": target.SessionsNameTemplate,
		"state_dir":              target.StateDir,
		"state_file":             target.StateFile,
	} {
		if got == "" {
			t.Errorf("runtimes.main.%s 未导入", label)
		}
	}
	// mergeLegacyRun 的并入只发生在内存里、不落盘，所以 target 里的路径字段
	// 带着 $root 展开后的形态还看不出来；这里按「非空即已搬运」判定，精确的
	// 展开与缺省补全归 runConfigMigrate 的落盘路径（TestRunConfigMigrate* 钉）。
	if target.MainAgent != "" {
		t.Errorf("mergeLegacyRun 不该替用户补 main_agent，got %q", target.MainAgent)
	}
	if file.DefaultRuntime != instances.DefaultRuntimeName {
		t.Errorf("default_runtime 应补为 %q，got %q", instances.DefaultRuntimeName, file.DefaultRuntime)
	}
	// 每个字段都必须在变更清单里露过面，否则用户看不出导入了什么
	for _, want := range []string{
		"+ runtimes.main.root = /srv/root",
		"+ runtimes.main.repos_dir = /srv/root/repos",
		"+ runtimes.main.review_root = /srv/root/review",
		"+ runtimes.main.review_name_template",
		"+ runtimes.main.sessions_dir = /srv/root/sessions",
		"+ runtimes.main.sessions_name_template",
		"+ runtimes.main.state_dir = /srv/root/state",
		"+ runtimes.main.state_file = /srv/root/state/state.sqlite3",
	} {
		if !strings.Contains(strings.Join(changes, "\n"), want) {
			t.Errorf("变更清单缺少 %q：\n%s", want, strings.Join(changes, "\n"))
		}
	}
}

// TestMergeLegacyRunKeepsExistingRuntimeValues 钉住 25年9月 refactor 后
// mergeLegacyRun 写入边界的真实形态：runtimes.main 已在 map 里时，合出来的
// 字段值只存在于函数内的局部副本，map 条目与变更清单都不动。
func TestMergeLegacyRunKeepsExistingRuntimeValues(t *testing.T) {
	file := &instances.File{Runtimes: map[string]instances.Runtime{
		instances.DefaultRuntimeName: {MainAgent: "main", Root: "/already/set"},
	}}
	legacy, err := runcfg.Load(writeLegacyRun(t, monitorSite+"root: /from/legacy\nrepos-dir: repos\n"))
	if err != nil {
		t.Fatal(err)
	}

	changes, err := mergeLegacyRun(file, legacy)
	if err != nil {
		t.Fatal(err)
	}
	target := file.Runtimes[instances.DefaultRuntimeName]
	if target.Root != "/already/set" {
		t.Errorf("既有 root 不该被改写：%q", target.Root)
	}
	if target.ReposDir != "" {
		t.Errorf("既有 main 存在时不该写回 ReposDir，got %q", target.ReposDir)
	}
	joined := strings.Join(changes, "\n")
	if !strings.Contains(joined, "+ runtimes.main.repos_dir = ") {
		t.Errorf("空缺的 repos_dir 仍应记进变更清单（用户据此知道旧参数来自 run.yaml）：\n%s", joined)
	}
	if strings.Contains(joined, "runtimes.main.root") {
		t.Errorf("未覆盖的字段不该出现在变更清单：\n%s", joined)
	}

	// 反向：map 里没有 main 时才会写回，且顺带定下 default_runtime；
	// 相对 repos-dir 被锚到 run.yaml 所在目录，值必然带绝对前缀。
	fresh := &instances.File{Runtimes: map[string]instances.Runtime{"other": {MainAgent: "main"}}}
	if _, err := mergeLegacyRun(fresh, legacy); err != nil {
		t.Fatal(err)
	}
	written := fresh.Runtimes[instances.DefaultRuntimeName]
	if written.Root != "/from/legacy" {
		t.Errorf("没有 main 时旧 root 应写回，got %q", written.Root)
	}
	if !strings.HasSuffix(written.ReposDir, "/repos") {
		t.Errorf("相对 repos-dir 应锚到 run.yaml 目录，got %q", written.ReposDir)
	}
	if fresh.DefaultRuntime != instances.DefaultRuntimeName {
		t.Errorf("default_runtime 应补为 %q，got %q", instances.DefaultRuntimeName, fresh.DefaultRuntime)
	}
}

// TestMergeLegacyRunMergesMonitorIntoGiteaChannel 断言 monitor 的站点被折成
// gitea 通道，token 补缺、repos 取并集，且已在通道里的仓库不重复登记。
//
// 重复登记会让同一个仓库在通道里出现两次，调度侧会就同一个仓库跑两遍评审；
// 而 token 若被旧值覆盖，用户新配的凭据会被一次迁移悄悄换回旧的。
func TestMergeLegacyRunMergesMonitorIntoGiteaChannel(t *testing.T) {
	file := &instances.File{Channels: []instances.Channel{{
		Type: instances.ChannelGitea, Host: "https://mon.example.com",
		Reviewer: "ai", Merger: "merge", Token: "already-set",
		Repos: []instances.Repo{{Name: "acme/known"}},
	}}}
	legacy, err := runcfg.Load(writeLegacyRun(t, "root: /rooted\nmonitor:\n  https://mon.example.com:\n    token: legacy-token\n    repos:\n      - acme/known\n      - acme/new\n"))
	if err != nil {
		t.Fatal(err)
	}

	changes, err := mergeLegacyRun(file, legacy)
	if err != nil {
		t.Fatal(err)
	}
	channel, ok := findGiteaChannel(file, "https://mon.example.com")
	if !ok {
		t.Fatal("monitor 站点应并入 gitea 通道")
	}
	if channel.Token != "already-set" {
		t.Errorf("既有 token 不该被覆盖：%q", channel.Token)
	}
	names := make([]string, 0, len(channel.Repos))
	for _, repo := range channel.Repos {
		names = append(names, repo.Name)
	}
	if strings.Join(names, ",") != "acme/known,acme/new" {
		t.Errorf("repos 应为并集且不重复：%v", names)
	}
	joined := strings.Join(changes, "\n")
	if strings.Contains(joined, ".token") {
		t.Errorf("未覆盖 token 时不该有 token 变更条目：\n%s", joined)
	}
	if !strings.Contains(joined, "+ channels[gitea].repos acme/new") {
		t.Errorf("应记下新增仓库：\n%s", joined)
	}
	if strings.Contains(joined, "acme/known") {
		t.Errorf("已登记仓库不该出现在变更清单：\n%s", joined)
	}
}

// TestMergeLegacyRunTakesMonitorTokenWhenChannelEmpty 断言通道还没有 token 时
// 用 monitor 的 token 补上，并记一条变更（用户需要知道凭据从旧文件里带过来了）。
//
// token 是凭据：不从变更清单里说明它被导入，用户会以为新配置里没有凭据，
// 再去手工补一遍——而旧文件马上就要被删掉。
func TestMergeLegacyRunTakesMonitorTokenWhenChannelEmpty(t *testing.T) {
	file := &instances.File{}
	legacy, err := runcfg.Load(writeLegacyRun(t, "root: /rooted\nmonitor:\n  https://fresh.example.com:\n    token: imported-token\n"))
	if err != nil {
		t.Fatal(err)
	}

	changes, err := mergeLegacyRun(file, legacy)
	if err != nil {
		t.Fatal(err)
	}
	channel, ok := findGiteaChannel(file, "https://fresh.example.com")
	if !ok {
		t.Fatal("monitor 站点应建出 gitea 通道")
	}
	if channel.Token != "imported-token" {
		t.Errorf("token 应被导入：%q", channel.Token)
	}
	if !strings.Contains(strings.Join(changes, "\n"), "token（run.yaml monitor.") {
		t.Errorf("导入 token 应记入变更清单：\n%s", strings.Join(changes, "\n"))
	}
}

// TestMergeLegacyRunConvertsProviderShorthand 断言 provider 简写转成 providers
// 条目并设为 default_provider；已存在同名 provider 时不覆盖，也不重复记账。
//
// providers 条目里带着 APIKey。若同名条目已存在还照旧文件覆盖，用户已经配好的
// （可能是另一个 key 的）条目会被一次迁移换掉，而站点侧没人知道。
func TestMergeLegacyRunConvertsProviderShorthand(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	file := &instances.File{}
	legacy, err := runcfg.Load(writeLegacyRun(t, monitorSite+"provider:\n  type: minimax\n  token: legacy-ai\n"))
	if err != nil {
		t.Fatal(err)
	}
	changes, err := mergeLegacyRun(file, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if file.DefaultProvider != "minimax" {
		t.Errorf("default_provider = %q", file.DefaultProvider)
	}
	if file.Providers["minimax"].Token() != "legacy-ai" {
		t.Errorf("provider key 未导入：%+v", file.Providers["minimax"])
	}
	joined := strings.Join(changes, "\n")
	if !strings.Contains(joined, "+ providers.minimax（run.yaml provider 简写）") ||
		!strings.Contains(joined, "+ default_provider = minimax") {
		t.Errorf("provider 变更应记入清单：\n%s", joined)
	}

	// 已存在同名 provider：不覆盖 key，也不重复追加条目
	existing := &instances.File{
		Providers: map[string]instances.Provider{"minimax": {APIKey: "keep-me"}},
	}
	changes, err = mergeLegacyRun(existing, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if existing.Providers["minimax"].Token() != "keep-me" {
		t.Errorf("同名 provider 不该被覆盖：%+v", existing.Providers["minimax"])
	}
	if strings.Contains(strings.Join(changes, "\n"), "+ providers.minimax") {
		t.Errorf("未新增 provider 时不该有该条目：\n%s", strings.Join(changes, "\n"))
	}
	if !strings.Contains(strings.Join(changes, "\n"), "+ default_provider = minimax") {
		t.Errorf("default_provider 为空时仍应补上：\n%s", strings.Join(changes, "\n"))
	}
}

// TestRunConfigMigrateReportsAutoDiscoveredRunYamlFailure 断言 config.json 同目录的
// run.yaml 在自动发现这一步就报错时，migrate 直接把失败抛出来，而不是打印「无需迁移」。
//
// 自动发现用 os.Stat 探同目录：文件不在是正常的（无需迁移），但 ENOTDIR / 权限不足
// 这类失败说明「那里本来该有个文件，只是读不出来」。把它与「不存在」合并处理，用户会
// 得到一句「没有找到旧 run.yaml」而旧参数其实还躺在读不出来的位置上——迁移被静默跳过。
func TestRunConfigMigrateReportsAutoDiscoveredRunYamlFailure(t *testing.T) {
	isolateCredentials(t)
	dir := t.TempDir()
	sub := filepath.Join(dir, "conf")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(sub, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"runtimes": {"main": {"main_agent": "main"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 在 <dir>/run.yaml 这个位置上放一个普通文件：显式 --config 指到 <dir>/run.yaml/... 之下
	// 时，config.json 的同目录就成了一个普通文件，stat run.yaml 必然以 ENOTDIR 失败。
	if err := os.WriteFile(filepath.Join(dir, runcfg.FileName), []byte("占位"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	// 不设 RunPath：走自动发现分支；configPath 的目录是那个普通文件
	err := runConfigMigrate(migrateGapCommand(out), filepath.Join(dir, runcfg.FileName, "config.json"),
		&configMigrateOptions{})
	if err == nil {
		t.Fatalf("run.yaml 探不到（非不存在）时应报错：\n%s", out.String())
	}
	if strings.Contains(out.String(), "无需迁移") {
		t.Errorf("不该把读不出来的 run.yaml 说成无需迁移：\n%s", out.String())
	}
}

// TestRunConfigMigrateReportsRuntimeResolutionFailure 断言 runtimes 写成「多个 runtime
// 且没有 default_runtime」时，migrate 报错退出，不写配置文件也不改旧文件名。
//
// 这是旧配置里最容易出现的形态：run.yaml 时代没有 runtime 概念，手工加了两个 runtime
// 又忘了 default_runtime，载入时不会失败，直到真的要走迁移才需要挑一个。此刻必须说清
// 「需要指定 default_runtime」，而不是随便挑一个写进去——写错了就是整批运行参数落到了
// 错的 runtime 上。
func TestRunConfigMigrateReportsRuntimeResolutionFailure(t *testing.T) {
	isolateCredentials(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	body := `{"runtimes": {"a": {"main_agent": "main"}, "b": {"main_agent": "main"}}}` + "\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	runPath := writeLegacyRun(t, monitorSite+"root: /from/legacy\n")

	out := &bytes.Buffer{}
	err := runConfigMigrate(migrateGapCommand(out), configPath, &configMigrateOptions{RunPath: runPath})
	if err == nil {
		t.Fatalf("runtime 挑不出来时应报错：\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "default_runtime") {
		t.Errorf("错误应指出去设 default_runtime：%v", err)
	}
	// 旧文件不该被改名：改名意味着「已导入」，而这次什么都没写
	if _, statErr := os.Stat(runPath); statErr != nil {
		t.Errorf("失败时旧 run.yaml 不该被改名：%v", statErr)
	}
}

// TestRunConfigMigrateKeepsLegacyFileWhenWriteFails 断言 saveConfig 落盘失败时
// migrate 报错，且不去改旧 run.yaml 的名字。
//
// 落盘失败（配置目录只读 / 磁盘满）时若继续改名，用户会得到「旧文件已迁走、新配置
// 却不存在」的形态：旧参数只剩一份 .migrated 备份，而 config.json 一行没变。
// 命令必须停在这里，让旧文件保持原样可重试。
func TestRunConfigMigrateKeepsLegacyFileWhenWriteFails(t *testing.T) {
	isolateCredentials(t)
	dir := t.TempDir()
	configDir := filepath.Join(dir, "conf")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPath := writeLegacyRun(t, monitorSite+"root: /from/legacy\n")

	// 目录只读：备份与原子替换都写不下去（t.TempDir 的清理需要可写，退出前恢复）
	if err := os.Chmod(configDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(configDir, 0o755) })

	out := &bytes.Buffer{}
	err := runConfigMigrate(migrateGapCommand(out), configPath, &configMigrateOptions{RunPath: runPath})
	if err == nil {
		t.Fatalf("配置写不下去时应报错：\n%s", out.String())
	}
	if _, statErr := os.Stat(runPath); statErr != nil {
		t.Errorf("写失败时旧 run.yaml 不该被改名：%v", statErr)
	}
	if _, statErr := os.Stat(runPath + ".migrated"); !os.IsNotExist(statErr) {
		t.Errorf("写失败时不该留下 .migrated：%v", statErr)
	}
}
