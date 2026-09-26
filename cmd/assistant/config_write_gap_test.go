package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/schema"
)

// TestConfigNewMkdirFailurePropagates 断言目标目录建不出来时 config new 报错，
// 而不是继续往一个不存在的路径写骨架。
//
// 目标路径的父目录被一个普通文件占住，是最常见的「路径手抖」形态（把
// config.json 写到了 /etc/passwd 这种文件底下）。此时若不报错而继续，用户
// 会看到「已写入空配置骨架」却找不到文件——以为成功，实际什么都没落盘。
func TestConfigNewMkdirFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// blocker 是文件，于是 blocker/config.json 的父目录创建必然失败
	_, err := execConfigNew(t, filepath.Join(blocker, "config.json"))
	if err == nil {
		t.Fatal("父目录建不出来时应报错，而不是假装写入成功")
	}
	if _, statErr := os.Stat(filepath.Join(blocker, "config.json")); statErr == nil {
		t.Error("失败时不该有任何文件被写出")
	}
}

// TestConfigNewStatFailurePropagates 断言「目标路径 stat 失败但不是不存在」时
// config new 报错退出。
//
// 与「文件不存在」必须区分开：不存在是首次生成的正常入口，而 EACCES / ELOOP
// 之类的失败意味着我们根本判断不出目标是否已被占用——此时若当成不存在继续
// 写，就会在一个读不到的位置上覆盖掉用户的配置。
func TestConfigNewStatFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	loop := filepath.Join(dir, "loop")
	// 自指向的符号链接：stat 会回 ELOOP，既不是 nil 也不是 IsNotExist
	if err := os.Symlink(loop, loop); err != nil {
		t.Skipf("本机不支持自指向符号链接：%v", err)
	}

	_, err := execConfigNew(t, loop)
	if err == nil {
		t.Fatal("stat 非「不存在」错误时应报错")
	}
	if !strings.Contains(err.Error(), "too many levels") && !strings.Contains(err.Error(), "loop") {
		t.Logf("（错误文案随平台而定，仅确认它确实报错）err = %v", err)
	}
}

// TestConfigInitRejectsUnparsableExistingConfig 断言现有配置语法坏掉时 config init
// 明确报「先修好或手动备份」，而不是当成空配置重新生成一份。
//
// 这是最危险的一种「补全」：用户手上那份配置可能有几十个平台条目，只因少了一个
// 逗号读不出来；若静默当作空文件重建，用户的全部登记会被一次命令抹掉，且旧文件
// 只有在真正写入后才备份——那时备份里已经没有内容了。
func TestConfigInitRejectsUnparsableExistingConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	path := filepath.Join(dir, "config.json")
	broken := `{"channels": [ {"type": "gitea",} ]}`
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := execConfigInit(t, path, "")
	if err == nil {
		t.Fatalf("坏配置应报错：\n%s", output)
	}
	if !strings.Contains(err.Error(), "现有配置无法解析") {
		t.Errorf("错误应点明现有配置无法解析：%v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != broken {
		t.Errorf("坏掉的配置不该被改写：\n%s", data)
	}
}

// TestConfigInitRejectsInvalidMergeResult 断言补全后的结果过不了校验时宁可不写。
//
// 配置里是一个 host 不是合法站点地址的 gitea 通道：prefill 不会动它（host 已在
// known 里），Normalize 也把它补成 reviewer/merger 齐全的样子，但 Validate 仍然
// 因为地址不合法而失败。此时写盘会得到一个连 daemon 都启动不了的配置——「宁可不
// 写，也不写坏」这条注释承诺的正是这个分支。
func TestConfigInitRejectsInvalidMergeResult(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	path := filepath.Join(dir, "config.json")
	// host 是相对路径而非站点根地址：Normalize 只去尾斜杠、补 reviewer/merger，
	// 补不出可运行配置，Validate 必然失败。
	existing := `{"channels": [ {"type": "gitea", "host": "not-a-site"} ]}` + "\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := execConfigInit(t, path, "")
	if err == nil {
		t.Fatalf("合并结果不合法时应报错：\n%s", output)
	}
	if !strings.Contains(err.Error(), "合并结果不合法，未写入") {
		t.Errorf("错误应说明未写入：%v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != existing {
		t.Errorf("不合法时不该改写原配置：\n%s", data)
	}

	// --dry-run 走的是另一条分支（校验后先打印再返回），不合法同样要拦住
	if _, dryErr := execConfigInit(t, path, "", "--dry-run"); dryErr == nil {
		t.Error("dry-run 也必须先过校验")
	}
}

// TestConfigInitDryRunWithoutChangeExplainsNoop 断言 dry-run 在没有任何改动时照样
// 打印「将写入」与空改动清单，而不是因为无处可改就静默退出。
//
// dry-run 是「重启 daemon 之前先看一眼会动什么」的唯一手段；无改动时也必须给出
// 一个明确结果，否则用户分不清「命令没生效」和「确实没东西可补」。
func TestConfigInitDryRunWithoutChangeExplainsNoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	path := filepath.Join(dir, "config.json")
	if _, err := execConfigNew(t, path); err != nil {
		t.Fatal(err)
	}
	// 骨架已带 $schema，凭据库为空 → prefill 无改动 → changes 为空但校验要过
	// （空通道列表本身合法：validate 只看「至少有一项非空」？这里骨架只有
	// channels:[]，故先补一个 runtime 使 Validate 通过）
	data := `{"$schema": "` + schema.Reference + `", "runtimes": {"main": {"main_agent": "main"}}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := execConfigInit(t, path, "", "--dry-run")
	if err != nil {
		t.Fatalf("无改动的 dry-run 不该报错：%v\n%s", err, output)
	}
	if !strings.Contains(output, "dry-run：将写入") {
		t.Errorf("dry-run 应打印目标路径：\n%s", output)
	}
	if !strings.Contains(output, "（无改动）") {
		t.Errorf("无改动时应显式说明：\n%s", output)
	}
}

// TestPrefillInstancesReportsBrokenCredentialStore 断言凭据库损坏时预填直接报错，
// 而不是当成「没登录过任何平台」继续生成一份空配置。
//
// 预填是 config init 的核心价值：把已登录的站点写进 instances。凭据库读不出来
// 时静默跳过，用户会得到一份看起来「正常完成」却缺了所有平台的配置，之后
// daemon 报「配置中没有平台」——而真正的问题在凭据文件上。
func TestPrefillInstancesReportsBrokenCredentialStore(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(storePath, []byte("{ 不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CREDENTIALS", storePath)

	_, err := prefillInstances(filepath.Join(dir, "config.json"), &instances.File{})
	if err == nil {
		t.Fatal("凭据库损坏时预填应报错")
	}
	if !strings.Contains(err.Error(), storePath) {
		t.Errorf("错误应点明坏掉的凭据库：%v", err)
	}
}

// TestWriteSchemaFileReportsUnwritableDir 断言 schema 写不进配置目录时报错。
//
// schema 文件是编辑器的补全源，与配置同目录分发；写不进去（目录只读、名字被
// 占）必须让 config new / init 失败，否则用户拿到的是一份指向不存在文件的
// $schema——编辑器静默失去补全，且没有任何提示。
func TestWriteSchemaFileReportsUnwritableDir(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	// 把 schema 的目标名字占成一个目录：os.WriteFile 必然失败
	if err := os.Mkdir(filepath.Join(dir, schema.FileName), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := writeSchemaFile(configPath)
	if err == nil {
		t.Fatal("schema 写不进去时应报错")
	}
	if !strings.Contains(err.Error(), schema.FileName) {
		t.Errorf("错误应点明目标文件：%v", err)
	}
}

// TestConfigInitReportsMissingSchemaWrite 断言 schema 落盘失败时 config init 也
// 失败，不留下一个已经改好、却缺了 schema 的半成品。
func TestConfigInitReportsMissingSchemaWrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	path := filepath.Join(dir, "config.json")
	// 手写一份「过了校验、且声明 $schema 指向本目录」的配置：只有带上 $schema
	// 才会走到重写 schema 文件那一步，而 runtimes 保证 Validate 通过。
	data := `{"$schema": "` + schema.Reference + `", "runtimes": {"main": {"main_agent": "main"}}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	// 在 config init 会重写 schema 的位置放一个目录：写入必然失败
	if err := os.Mkdir(filepath.Join(dir, schema.FileName), 0o755); err != nil {
		t.Fatal(err)
	}

	output, err := execConfigInit(t, path, "")
	if err == nil {
		t.Fatalf("schema 写失败时 config init 应报错：\n%s", output)
	}
	if !strings.Contains(err.Error(), schema.FileName) {
		t.Errorf("错误应点明 schema 文件：%v", err)
	}
}

// TestConfigInitReportsBackupFailure 断言补全前的备份失败时中止，不覆盖原文件。
//
// 备份是「用户配置只有一个副本」这道保险；备份失败（配置目录只读、正在被别的
// 进程占用）却继续写入，等于在没有任何回退路径的情况下改掉用户手写的文件。
func TestConfigInitReportsBackupFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"runtimes": {"main": {"main_agent": "main"}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 备份目标被目录占住 → 写入 .bak 必然失败
	if err := os.Mkdir(path+".bak", 0o755); err != nil {
		t.Fatal(err)
	}

	output, err := execConfigInit(t, path, "")
	if err == nil {
		t.Fatalf("备份失败时应中止：\n%s", output)
	}
	if !strings.Contains(err.Error(), path+".bak") {
		t.Errorf("错误应点明备份目标：%v", err)
	}
}

// TestConfigNewReportsSaveBytesFailure 断言骨架序列化后写盘失败时报错，而不是
// 报告成功。
//
// 目标路径本身被一个只读目录占住（或路径就是目录）时，SaveBytes 失败；此时
// 若吞掉错误，用户看到「已写入空配置骨架」却什么都没有，接着运行的每条命令
// 都会在「配置不存在」上打转。
func TestConfigNewReportsSaveBytesFailure(t *testing.T) {
	dir := t.TempDir()
	// 目标路径本身就是一个目录：写文件必然失败，而 stat 是成功的
	path := filepath.Join(dir, "config.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	// 目录存在 → 走「已存在」分支；--force 绕过它进入真正的写盘
	_, err := execConfigNew(t, path, "--force")
	if err == nil {
		t.Fatal("目标路径是目录时写盘应失败")
	}
}

// TestConfigTargetPathFallsBackToWorkingDirectory 断言 --config 与 ASSISTANT_CONFIG
// 都为空时，缺省落点是「当前目录/config.json」。
//
// 这是「在项目目录里直接 assistant config new」的默认体验；退回 requests 之外的
// 位置（用户级配置目录）会让用户在项目里看不到自己刚生成的配置。
func TestConfigTargetPathFallsBackToWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CONFIG", "")
	t.Chdir(dir)

	path, err := configTargetPath("")
	if err != nil {
		t.Fatalf("缺省落点不该报错：%v", err)
	}
	if path != filepath.Join(dir, "config.json") {
		t.Errorf("path = %q, want 当前目录下的 config.json", path)
	}

	// ASSISTANT_CONFIG 非空时优先于缺省值（但不能压过显式 --config）
	t.Setenv("ASSISTANT_CONFIG", filepath.Join(dir, "from-env.json"))
	path, err = configTargetPath("")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "from-env.json") {
		t.Errorf("path = %q, want 取自 ASSISTANT_CONFIG", path)
	}
}

// TestConfigTargetPathRejectsBrokenWorkingDirectory 断言取不到当前目录时
// configTargetPath 报错而不是返回一个拼接出来的相对路径。
//
// 当前目录被删除（进程在已删目录里继续跑）时 os.Getwd 会失败；此时若继续，
// 「config.json」会被写到进程的某个未知位置，用户拿到的是一份找不到的配置。
func TestConfigTargetPathRejectsBrokenWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CONFIG", "")
	t.Chdir(dir)
	// 删掉当前目录本身：此后 os.Getwd 回 ENOENT
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := configTargetPath(""); err == nil {
		t.Error("当前目录不可用时 configTargetPath 应报错")
	}
}

// TestConfigInitReportsProviderOptionFailure 断言 --provider 之外还要求
// --api-key-stdin 而标准输入读不到内容时，config init 中止且不改写原配置。
//
// api_key 是派生会话凭据的唯一输入；读不到却继续补全，会在配置里留下一个空
// api_key 的 provider，之后每次会话都以空密钥去认证——错误必须在写入前拦住。
func TestConfigInitReportsProviderOptionFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	path := filepath.Join(dir, "config.json")
	original := `{"runtimes": {"main": {"main_agent": "main"}}}` + "\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := execConfigInit(t, path, "", "--provider", "minimax", "--api-key-stdin")
	if err == nil {
		t.Fatal("api_key 读不到时应报错")
	}
	if !strings.Contains(err.Error(), "没有读到 api_key") {
		t.Errorf("错误应点明 api_key 读不到：%v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != original {
		t.Errorf("失败时不该改写原配置：\n%s", data)
	}
}

// TestPrefillInstancesSkipsAlreadyRegisteredHost 断言已在配置里登记过的站点不被
// 重复预填（跳过 known 命中的身份）。
//
// 「跳过」这条分支是 config init 幂等性的来源：补全必须能把同一份配置反复跑
// 而不产生重复通道；重复的通道会以「实例键重复」被 Validate 拒绝，让补全永远
// 失败。
func TestPrefillInstancesSkipsAlreadyRegisteredHost(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", storePath)

	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: "https://known.example.com", User: "dev"})
	if err := credentials.Save(storePath, store); err != nil {
		t.Fatal(err)
	}

	file := &instances.File{Channels: []instances.Channel{{
		Type: instances.ChannelGitea, Host: "https://known.example.com",
		Reviewer: "ai", Merger: "merge",
	}}}
	changes, err := prefillInstances(filepath.Join(dir, "config.json"), file)
	if err != nil {
		t.Fatalf("预填不该报错：%v", err)
	}
	if len(changes) != 0 {
		t.Errorf("已登记的站点不该产生改动：%v", changes)
	}
	if len(file.Channels) != 1 {
		t.Errorf("不该追加重复通道，got %+v", file.Channels)
	}
}

// TestPrefillInstancesReportsMissingWorkingDirectory 断言凭据库路径本身取不出来时
// 预填报错，而不是当成「没有已登录平台」生成一份空配置。
//
// Path() 在既没有 ASSISTANT_CREDENTIALS 又定位不到平台标准配置目录时报错；此处
// 静默跳过会让用户拿到一份缺了全部平台的配置，真正的问题（取不到路径）被掩盖。
func TestPrefillInstancesReportsMissingWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	// HOME 清空且 XDG_CONFIG_HOME 置为相对路径：os.UserConfigDir 两者都拒
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "relative/path")

	if _, err := prefillInstances(filepath.Join(dir, "config.json"), &instances.File{}); err == nil {
		t.Error("凭据库路径取不到时预填应报错")
	}
}

// TestBackupConfigReportsMissingSource 断言待备份的原文件读不出来时报错。
//
// 备份失败却继续写入，等于在没有任何回退路径的情况下改掉用户手写的文件；
// 源文件在 stat 与 ReadFile 之间消失（被别的进程清理）是这条分支的常见形态。
func TestBackupConfigReportsMissingSource(t *testing.T) {
	dir := t.TempDir()
	if err := backupConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("源文件不存在时备份应报错")
	}
}
