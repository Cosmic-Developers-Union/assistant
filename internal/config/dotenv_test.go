package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDotEnvSearchesParentDirectories(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "one", "two")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	dotEnvPath := filepath.Join(root, ".env")
	if err := os.WriteFile(dotEnvPath, []byte("GITEA_HOST=https://gitea.example.com\nGITEA_ACCESS_TOKEN=from-file\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	unsetEnvironmentVariable(t, "GITEA_HOST")
	unsetEnvironmentVariable(t, "GITEA_ACCESS_TOKEN")

	loadedPath, err := LoadDotEnv(nested)
	if err != nil {
		t.Fatalf("LoadDotEnv() error = %v", err)
	}
	if loadedPath != dotEnvPath {
		t.Errorf("LoadDotEnv() path = %q, want %q", loadedPath, dotEnvPath)
	}
	if got := os.Getenv("GITEA_HOST"); got != "https://gitea.example.com" {
		t.Errorf("GITEA_HOST = %q", got)
	}
	if got := os.Getenv("GITEA_ACCESS_TOKEN"); got != "from-file" {
		t.Errorf("GITEA_ACCESS_TOKEN = %q", got)
	}
}

func TestLoadDotEnvDoesNotOverrideProcessEnvironment(t *testing.T) {
	directory := t.TempDir()
	dotEnvPath := filepath.Join(directory, ".env")
	if err := os.WriteFile(dotEnvPath, []byte("GITEA_HOST=https://file.example.com\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("GITEA_HOST", "https://environment.example.com")

	if _, err := LoadDotEnv(directory); err != nil {
		t.Fatalf("LoadDotEnv() error = %v", err)
	}
	if got := os.Getenv("GITEA_HOST"); got != "https://environment.example.com" {
		t.Errorf("GITEA_HOST = %q", got)
	}
}

func TestLoadDotEnvAllowsMissingFile(t *testing.T) {
	directory := t.TempDir()
	loadedPath, err := LoadDotEnv(directory)
	if err != nil {
		t.Fatalf("LoadDotEnv() error = %v", err)
	}
	if loadedPath != "" {
		t.Errorf("LoadDotEnv() path = %q", loadedPath)
	}
}

// TestLoadDotEnvParsesFileSyntax 覆盖 .env 的合法书写形式：空行、注释、
// export 前缀、单双引号、行尾注释与 = 两侧空白都必须解析成期望的值。
func TestLoadDotEnvParsesFileSyntax(t *testing.T) {
	directory := t.TempDir()
	content := "" +
		"# 整行注释\n" +
		"\n" +
		"   \n" +
		"DOTENV_BARE=bare-value\n" +
		"export DOTENV_EXPORTED=exported-value\n" +
		"DOTENV_SINGLE='single value'\n" +
		`DOTENV_DOUBLE="double value"` + "\n" +
		"DOTENV_COMMENTED=kept # 行尾注释\n" +
		"DOTENV_SINGLE_QUOTED_HASH='a#b'\n" +
		"  DOTENV_SPACED  =  spaced-value  \n" +
		"DOTENV_EMPTY=\n"
	for _, name := range []string{
		"DOTENV_BARE", "DOTENV_EXPORTED", "DOTENV_SINGLE", "DOTENV_DOUBLE",
		"DOTENV_COMMENTED", "DOTENV_SINGLE_QUOTED_HASH", "DOTENV_SPACED", "DOTENV_EMPTY",
	} {
		unsetEnvironmentVariable(t, name)
	}
	if err := os.WriteFile(filepath.Join(directory, ".env"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := LoadDotEnv(directory); err != nil {
		t.Fatalf("LoadDotEnv() error = %v", err)
	}

	want := map[string]string{
		"DOTENV_BARE":               "bare-value",
		"DOTENV_EXPORTED":           "exported-value",
		"DOTENV_SINGLE":             "single value",
		"DOTENV_DOUBLE":             "double value",
		"DOTENV_COMMENTED":          "kept",
		"DOTENV_SINGLE_QUOTED_HASH": "a#b",
		"DOTENV_SPACED":             "spaced-value",
		"DOTENV_EMPTY":              "",
	}
	for name, expected := range want {
		if got := os.Getenv(name); got != expected {
			t.Errorf("%s = %q, want %q", name, got, expected)
		}
	}
}

// TestLoadDotEnvRejectsMalformedFile 覆盖畸形 .env：godotenv 静态解析失败时
// 必须返回带路径的错误，而不是静默忽略（否则运营看到的是「变量没生效」）。
func TestLoadDotEnvRejectsMalformedFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, ".env")
	if err := os.WriteFile(path, []byte("DOTENV_BROKEN='unterminated\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loadedPath, err := LoadDotEnv(directory)
	if err == nil {
		t.Fatalf("LoadDotEnv() error = nil, want 畸形文件报错")
	}
	if loadedPath != "" {
		t.Errorf("LoadDotEnv() path = %q, want 空串", loadedPath)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("错误信息 %q 未包含路径 %q", err, path)
	}
}

// TestLoadDotEnvRejectsNonRegularFile 覆盖 .env 是目录的情况：必须报「不是
// 普通文件」，而不是把目录当配置读。
func TestLoadDotEnvRejectsNonRegularFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, ".env")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}

	if _, err := LoadDotEnv(directory); err == nil {
		t.Fatal("LoadDotEnv() error = nil, want 非普通文件报错")
	} else if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error = %v, want not a regular file", err)
	}
}

// TestLoadDotEnvStopsAtFirstMatch 覆盖向上搜索的边界：只有最近的一层 .env
// 生效，更上层的同名文件不被加载。
func TestLoadDotEnvStopsAtFirstMatch(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "one", "two")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	outer := filepath.Join(root, ".env")
	if err := os.WriteFile(outer, []byte("DOTENV_NEAREST=outer\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	inner := filepath.Join(root, "one", ".env")
	if err := os.WriteFile(inner, []byte("DOTENV_NEAREST=inner\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	unsetEnvironmentVariable(t, "DOTENV_NEAREST")

	loadedPath, err := LoadDotEnv(nested)
	if err != nil {
		t.Fatalf("LoadDotEnv() error = %v", err)
	}
	if loadedPath != inner {
		t.Errorf("LoadDotEnv() path = %q, want 最近的 %q", loadedPath, inner)
	}
	if got := os.Getenv("DOTENV_NEAREST"); got != "inner" {
		t.Errorf("DOTENV_NEAREST = %q, want inner", got)
	}
}

// TestLoadDotEnvRejectsRelativePathWithMissingFile 确认相对起点也按绝对路径
// 向上搜索：父目录存在 .env 时能从子目录命中。
func TestLoadDotEnvResolvesRelativeStartDirectory(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "one")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	dotEnvPath := filepath.Join(root, ".env")
	if err := os.WriteFile(dotEnvPath, []byte("DOTENV_RELATIVE=ok\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	unsetEnvironmentVariable(t, "DOTENV_RELATIVE")
	t.Chdir(nested)

	loadedPath, err := LoadDotEnv(".")
	if err != nil {
		t.Fatalf("LoadDotEnv() error = %v", err)
	}
	if loadedPath != dotEnvPath {
		t.Errorf("LoadDotEnv() path = %q, want %q", loadedPath, dotEnvPath)
	}
	if got := os.Getenv("DOTENV_RELATIVE"); got != "ok" {
		t.Errorf("DOTENV_RELATIVE = %q, want ok", got)
	}
}

func unsetEnvironmentVariable(t *testing.T, name string) {
	t.Helper()
	value, exists := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("Unsetenv(%q) error = %v", name, err)
	}
	t.Cleanup(func() {
		if exists {
			if err := os.Setenv(name, value); err != nil {
				t.Errorf("Setenv(%q) error = %v", name, err)
			}
			return
		}
		if err := os.Unsetenv(name); err != nil {
			t.Errorf("Unsetenv(%q) error = %v", name, err)
		}
	})
}

// TestLoadBesideLoadsConfigSibling 覆盖 LoadBeside 的正常路径：只加载
// config.json 同目录的 .env，且不覆盖已有的进程环境变量。
func TestLoadBesideLoadsConfigSibling(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	dotEnvPath := filepath.Join(directory, ".env")
	if err := os.WriteFile(dotEnvPath, []byte("BESIDE_NEW=from-file\nBESIDE_KEEP=from-file\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	unsetEnvironmentVariable(t, "BESIDE_NEW")
	t.Setenv("BESIDE_KEEP", "from-process")

	loadedPath, err := LoadBeside(configPath)
	if err != nil {
		t.Fatalf("LoadBeside() error = %v", err)
	}
	if loadedPath != dotEnvPath {
		t.Errorf("LoadBeside() path = %q, want %q", loadedPath, dotEnvPath)
	}
	if got := os.Getenv("BESIDE_NEW"); got != "from-file" {
		t.Errorf("BESIDE_NEW = %q, want from-file", got)
	}
	if got := os.Getenv("BESIDE_KEEP"); got != "from-process" {
		t.Errorf("BESIDE_KEEP = %q, want 进程环境优先", got)
	}
}

// TestLoadBesideDoesNotSearchParentDirectories 是本函数与 LoadDotEnv 的关键
// 区别（见 AGENTS.md：用 .env 时只加载 config.json 同目录的那份，不向上搜索）：
// 父目录有 .env、config.json 同目录没有时必须返回空串，且不注入任何变量。
func TestLoadBesideDoesNotSearchParentDirectories(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("BESIDE_PARENT=should-not-load\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	unsetEnvironmentVariable(t, "BESIDE_PARENT")

	loadedPath, err := LoadBeside(filepath.Join(nested, "config.json"))
	if err != nil {
		t.Fatalf("LoadBeside() error = %v", err)
	}
	if loadedPath != "" {
		t.Errorf("LoadBeside() path = %q, want 空串（不向上搜索）", loadedPath)
	}
	if got := os.Getenv("BESIDE_PARENT"); got != "" {
		t.Errorf("BESIDE_PARENT = %q, want 未被加载", got)
	}
}

// TestLoadBesideAllowsMissingFile 覆盖同目录无 .env：返回空串、不报错，密钥
// 引用回落进程环境。
func TestLoadBesideAllowsMissingFile(t *testing.T) {
	loadedPath, err := LoadBeside(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("LoadBeside() error = %v", err)
	}
	if loadedPath != "" {
		t.Errorf("LoadBeside() path = %q, want 空串", loadedPath)
	}
}

// TestLoadBesideResolvesRelativeConfigPath 覆盖 configPath 是相对路径（如
// ./config.json）时按「当前目录的同级 .env」解析：返回的也是相对路径，但
// 指向的确实是同目录那份 .env（不相对进程 cwd 乱找）。
func TestLoadBesideResolvesRelativeConfigPath(t *testing.T) {
	directory := t.TempDir()
	dotEnvPath := filepath.Join(directory, ".env")
	if err := os.WriteFile(dotEnvPath, []byte("BESIDE_RELATIVE=ok\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	unsetEnvironmentVariable(t, "BESIDE_RELATIVE")
	t.Chdir(directory)

	loadedPath, err := LoadBeside("./config.json")
	if err != nil {
		t.Fatalf("LoadBeside() error = %v", err)
	}
	// 返回值沿用传入路径的形态（相对路径就返回相对路径），落点判断用绝对化后的比较
	absolute, err := filepath.Abs(loadedPath)
	if err != nil {
		t.Fatalf("Abs(%q) error = %v", loadedPath, err)
	}
	if absolute != dotEnvPath {
		t.Errorf("LoadBeside() path = %q（绝对化 %q）, want %q", loadedPath, absolute, dotEnvPath)
	}
	if got := os.Getenv("BESIDE_RELATIVE"); got != "ok" {
		t.Errorf("BESIDE_RELATIVE = %q, want ok", got)
	}
}

// TestLoadBesideRejectsMalformedFile 覆盖畸形 .env：报错带路径。
func TestLoadBesideRejectsMalformedFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, ".env")
	if err := os.WriteFile(path, []byte("BESIDE_BROKEN='unterminated\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loadedPath, err := LoadBeside(filepath.Join(directory, "config.json"))
	if err == nil {
		t.Fatal("LoadBeside() error = nil, want 畸形文件报错")
	}
	if loadedPath != "" {
		t.Errorf("LoadBeside() path = %q, want 空串", loadedPath)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("错误信息 %q 未包含路径 %q", err, path)
	}
}

// TestLoadBesideRejectsNonRegularFile 覆盖 .env 是目录：报「不是普通文件」。
func TestLoadBesideRejectsNonRegularFile(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, ".env"), 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}

	if _, err := LoadBeside(filepath.Join(directory, "config.json")); err == nil {
		t.Fatal("LoadBeside() error = nil, want 非普通文件报错")
	} else if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error = %v, want not a regular file", err)
	}
}

// TestLoadBesideRejectsUnreadableFile 覆盖 .env 存在但不可读（权限错误）：
// 报 inspect 错误而不是静默放过——这是 LoadBeside 里除「不存在」之外的
// os.Stat 分支。
func TestLoadBesideRejectsUnreadableFile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("以 root 身份运行时权限位不生效，跳过")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, ".env")
	if err := os.WriteFile(path, []byte("BESIDE_DENIED=1\n"), 0o000); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	// 目录无搜索权限时 os.Stat 返回 EACCES，命中「非不存在错误」分支
	if err := os.Chmod(directory, 0o000); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0o755) })

	_, err := LoadBeside(filepath.Join(directory, "config.json"))
	if err == nil {
		t.Fatal("LoadBeside() error = nil, want 权限错误")
	}
	if !strings.Contains(err.Error(), "inspect") {
		t.Errorf("error = %v, want inspect 语境", err)
	}
}
