package cli

import (
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	gitremote "github.com/Cosmic-Developers-Union/assistant/internal/integration/gitea"
)

type projectFixture struct {
	host, path, dir      string
	users                map[string]bool
	protection           map[string]any
	writes               int
	requests             int
	admin                bool
	failMethod, failPath string
	onToken              func()
}

func TestProjectMCPInstallWithoutCredentialsOrConfig(t *testing.T) {
	for _, state := range []string{"missing", "invalid", "directory"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			if out, err := exec.Command("git", "init", "--quiet", dir).CombinedOutput(); err != nil {
				t.Fatal(string(out), err)
			}
			credentialPath := filepath.Join(t.TempDir(), "credentials.json")
			switch state {
			case "invalid":
				if err := os.WriteFile(credentialPath, []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(credentialPath, 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("ASSISTANT_CREDENTIALS", credentialPath)
			t.Setenv("ASSISTANT_CONFIG", filepath.Join(dir, "missing-config.json"))
			args := []string{"project", "--dir", dir, "install", "mcp"}
			if _, _, err := command(t, append(args, "--dry-run")...); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, ".mcp.json")
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("演练写入配置", err)
			}
			for range 2 {
				if _, _, err := command(t, args...); err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var config struct {
				Servers map[string]struct {
					Command string   `json:"command"`
					Args    []string `json:"args"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			server := config.Servers["gitea"]
			if server.Command != "assistant" || len(server.Args) != 2 || server.Args[0] != "mcp" || server.Args[1] != "gitea" {
				t.Fatal(string(data))
			}
			if _, _, err := command(t, append(args, "--instance", "future-account")...); err == nil {
				t.Fatal("MCP 安装接受实例绑定")
			}
			if _, _, err := command(t, "project", "--dir", dir, "uninstall", "mcp"); err != nil {
				t.Fatal(err)
			}
			if state == "missing" {
				if _, err := os.Stat(credentialPath); !os.IsNotExist(err) {
					t.Fatal("安装创建了凭据文件", err)
				}
			}
		})
	}
}

func projectTestFixture(t *testing.T) *projectFixture {
	t.Helper()
	f := &projectFixture{users: map[string]bool{"admin": true}, admin: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests++
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" {
			f.writes++
		}
		if r.Method == f.failMethod && r.URL.Path == f.failPath {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"message":"失败"}`)
			return
		}
		switch {
		case r.URL.Path == "/api/v1/version":
			fmt.Fprint(w, `{"version":"1.27.0"}`)
		case r.URL.Path == "/api/v1/user":
			user := strings.TrimPrefix(r.Header.Get("Authorization"), "token token-")
			if basic, _, ok := r.BasicAuth(); ok {
				user = basic
			}
			fmt.Fprintf(w, `{"login":%q,"is_admin":%t}`, user, f.admin && user == "admin")
		case r.URL.Path == "/api/v1/admin/users":
			var body struct {
				User string `json:"username"`
			}
			_ = json.UnmarshalRead(r.Body, &body)
			f.users[body.User] = true
			fmt.Fprintf(w, `{"login":%q}`, body.User)
		case strings.HasSuffix(r.URL.Path, "/tokens"):
			user := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/users/"), "/tokens")
			if f.onToken != nil {
				f.onToken()
			}
			fmt.Fprintf(w, `{"sha1":%q}`, "token-"+user)
		case strings.HasPrefix(r.URL.Path, "/api/v1/users/"):
			user := strings.TrimPrefix(r.URL.Path, "/api/v1/users/")
			if !f.users[user] {
				w.WriteHeader(404)
				return
			}
			fmt.Fprintf(w, `{"login":%q}`, user)
		case strings.HasSuffix(r.URL.Path, "/labels"):
			if r.Method == "GET" {
				fmt.Fprint(w, `[]`)
			} else {
				var body map[string]any
				_ = json.UnmarshalRead(r.Body, &body)
				fmt.Fprintf(w, `{"id":%d,"name":%q}`, f.writes, body["name"])
			}
		case strings.Contains(r.URL.Path, "/labels/"):
			fmt.Fprint(w, `{"id":1,"exclusive":true}`)
		case strings.Contains(r.URL.Path, "/collaborators/"):
			w.WriteHeader(204)
		case strings.Contains(r.URL.Path, "/branch_protections"):
			switch r.Method {
			case "GET":
				if f.protection == nil {
					w.WriteHeader(404)
					return
				}
			case "DELETE":
				f.protection = nil
				w.WriteHeader(204)
				return
			default:
				var body map[string]any
				_ = json.UnmarshalRead(r.Body, &body)
				if f.protection == nil {
					f.protection = map[string]any{}
				}
				for k, v := range body {
					if v != nil {
						f.protection[k] = v
					}
				}
			}
			_ = json.MarshalWrite(w, f.protection)
		case r.URL.Path == "/api/v1/repos/team/repo":
			fmt.Fprint(w, `{"name":"repo","default_branch":"main"}`)
		default:
			t.Errorf("未预期请求: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	f.host = server.URL
	f.path = filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("ASSISTANT_CREDENTIALS", f.path)
	f.dir = t.TempDir()
	for _, args := range [][]string{{"init", "--quiet", f.dir}, {"-C", f.dir, "remote", "add", "gitea", server.URL + "/team/repo.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
	}
	f.save(t, []credentials.Gitea{{Name: "admin", URL: server.URL, Username: "admin", Token: "token-admin"}, {Name: "merge", URL: server.URL, Username: "merge", Token: "token-merge"}})
	return f
}
func (f *projectFixture) save(t *testing.T, entries []credentials.Gitea) {
	t.Helper()
	if err := credentials.Save(f.path, &credentials.File{Instances: credentials.Instances{Gitea: entries}}); err != nil {
		t.Fatal(err)
	}
}
func TestProvisionCommandCreatesAndReusesBothAccounts(t *testing.T) {
	f := projectTestFixture(t)
	out, _, err := command(t, "instance", "provision", "gitea", "--admin", "admin", "--name-prefix", "bots")
	if err != nil || !strings.Contains(out, "bots-ai") || !strings.Contains(out, "bots-merge") || strings.Contains(out, "token-ai") {
		t.Fatal(out, err)
	}
	file, err := credentials.Load(f.path)
	if err != nil || len(file.Instances.Gitea) != 4 {
		t.Fatal(file, err)
	}
	old := f.writes
	if _, _, err := command(t, "instance", "provision", "gitea", "--admin", "admin", "--name-prefix", "bots"); err != nil || f.writes != old {
		t.Fatal("重复接入创建令牌", err, f.writes, old)
	}
}
func TestProvisionCommandDryRunAndFailurePaths(t *testing.T) {
	for _, scenario := range []string{"dry", "same-user", "existing-account", "bad-password-file", "other-platform-name", "platform-error", "save-error", "missing-admin"} {
		t.Run(scenario, func(t *testing.T) {
			f := projectTestFixture(t)
			args := []string{"instance", "provision", "gitea", "--admin", "admin", "--name-prefix", "bots"}
			switch scenario {
			case "dry":
				args = append(args, "--dry-run")
			case "same-user":
				args = append(args, "--reviewer", "merge")
			case "existing-account":
				f.users["ai"] = true
			case "bad-password-file":
				args = append(args, "--reviewer-password-file", filepath.Join(t.TempDir(), "missing"))
			case "other-platform-name":
				file, _ := credentials.Load(f.path)
				file.Instances.QQ = []credentials.QQ{{Name: "bots-ai", AppID: "a", AppSecret: "secret"}}
				if err := credentials.Save(f.path, file); err != nil {
					t.Fatal(err)
				}
			case "platform-error":
				f.failMethod = "POST"
				f.failPath = "/api/v1/admin/users"
			case "save-error":
				f.onToken = func() { _ = os.Remove(f.path); _ = os.Mkdir(f.path, 0700) }
			case "missing-admin":
				args[4] = "missing"
			}
			out, _, err := command(t, args...)
			if scenario == "dry" {
				if err != nil || f.writes != 0 || !strings.Contains(out, "演练") {
					t.Fatal(out, err, f.writes)
				}
			} else if err == nil {
				t.Fatal("失败被吞掉", scenario)
			}
		})
	}
	f := projectTestFixture(t)
	f.users["ai"] = true
	password := filepath.Join(t.TempDir(), "password")
	_ = os.WriteFile(password, []byte("explicit-password"), 0600)
	if _, _, err := command(t, "instance", "provision", "gitea", "--admin", "admin", "--reviewer-password-file", password); err != nil {
		t.Fatal(err)
	}
}
func TestTokenCommandIsReadOnlyAndPrintsOnlyStoredToken(t *testing.T) {
	f := projectTestFixture(t)
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		out, errOut, err := command(t, "instance", "token", "admin")
		if err != nil || out != "token-admin\n" || errOut != "" {
			t.Fatal(out, errOut, err)
		}
	}
	for _, args := range [][]string{
		{"instance", "token", "missing"},
		{"instance", "token", "admin", "--user", "ai"},
		{"instance", "token", "admin", "--password-file", "/missing"},
		{"mcp", "gitea", "--instance", "admin"},
	} {
		out, _, err := command(t, args...)
		if err == nil || out != "" {
			t.Fatal("查询或 MCP 接受了错误的写操作参数", args, out, err)
		}
	}
	after, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if f.requests != 0 || string(before) != string(after) || !info.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("token 查询产生网络请求或修改凭据")
	}
}

func TestCreateTokenCommandOnlyReplacesChosenLocalCredential(t *testing.T) {
	for _, scenario := range []string{"ok", "saved-password", "missing-password", "bad-password", "issue-failure", "save-failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := projectTestFixture(t)
			file, _ := credentials.Load(f.path)
			file.Instances.Gitea[0].Token = "token-old"
			if scenario == "saved-password" {
				if err := file.Instances.Gitea[0].SetPassword("secret-password"); err != nil {
					t.Fatal(err)
				}
			}
			if err := credentials.Save(f.path, file); err != nil {
				t.Fatal(err)
			}
			password := filepath.Join(t.TempDir(), "password")
			_ = os.WriteFile(password, []byte("secret-password"), 0600)
			args := []string{"instance", "create-token", "admin", "--password-file", password}
			switch scenario {
			case "saved-password":
				args = args[:3]
			case "missing-password":
				args = args[:3]
			case "bad-password":
				_ = os.WriteFile(password, nil, 0600)
			case "issue-failure":
				f.failMethod = "POST"
				f.failPath = "/api/v1/users/admin/tokens"
			case "save-failure":
				f.onToken = func() { _ = os.Remove(f.path); _ = os.Mkdir(f.path, 0700) }
			}
			out, _, err := command(t, args...)
			if scenario == "ok" || scenario == "saved-password" {
				if err != nil || strings.Contains(out, "token-admin") {
					t.Fatal(out, err)
				}
				file, err := credentials.Load(f.path)
				if err != nil || file.Instances.Gitea[0].Token != "token-admin" || file.Instances.Gitea[1].Token != "token-merge" {
					t.Fatal(file, err)
				}
			} else if err == nil {
				t.Fatal("失败被忽略")
			}
		})
	}
}
func TestCreateTokenCommandAdminTargetAndLocalIsolation(t *testing.T) {
	for _, scenario := range []string{"new", "named", "existing", "not-admin", "missing-user", "name-collision", "platform-collision", "missing-login-password", "empty-name"} {
		t.Run(scenario, func(t *testing.T) {
			f := projectTestFixture(t)
			f.users["ai"], f.users["merge"] = true, true
			file, err := credentials.Load(f.path)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Instances.Gitea[0].SetPassword("admin-password"); err != nil {
				t.Fatal(err)
			}
			args := []string{"instance", "create-token", "admin", "--user", "ai"}
			name := "admin-ai"
			switch scenario {
			case "named":
				name = "work-ai"
				args = append(args, "--name", name)
			case "empty-name":
				args = append(args, "--name", " ")
			case "existing":
				name = "merge"
				args = []string{"instance", "create-token", "admin", "--user", "merge", "--name", name}
				file.Instances.Gitea[1].Token = "old-merge"
				if err := file.Instances.Gitea[1].SetPassword("merge-password"); err != nil {
					t.Fatal(err)
				}
			case "not-admin":
				f.admin = false
			case "missing-user":
				f.users["ai"] = false
			case "name-collision":
				args = append(args, "--name", "admin")
			case "platform-collision":
				file.Instances.QQ = []credentials.QQ{{Name: name, AppID: "id", AppSecret: "qq-secret"}}
			case "missing-login-password":
				file.Instances.Gitea[0].EncryptedPassword = ""
			}
			if err := credentials.Save(f.path, file); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(f.path)
			out, errOut, err := command(t, args...)
			if scenario != "new" && scenario != "named" && scenario != "existing" {
				if err == nil || f.writes != 0 {
					t.Fatal("拒绝条件未阻止发令牌", err, f.writes)
				}
				after, _ := os.ReadFile(f.path)
				if string(before) != string(after) {
					t.Fatal("失败修改本地凭据")
				}
				return
			}
			if err != nil || f.writes != 1 || strings.Contains(out+errOut, "password") || strings.Contains(out+errOut, "token-") {
				t.Fatal(out, errOut, err, f.writes)
			}
			after, err := credentials.Load(f.path)
			if err != nil {
				t.Fatal(err)
			}
			if after.Instances.Gitea[0] != file.Instances.Gitea[0] {
				t.Fatal("目标发令牌修改了管理员凭据")
			}
			found := false
			for _, account := range after.Instances.Gitea {
				if account.Name != name {
					continue
				}
				found = true
				if account.Token != "token-"+account.Username {
					t.Fatal("目标令牌没有保存")
				}
				password, err := account.PasswordValue()
				if scenario == "existing" {
					if err != nil || password != "merge-password" {
						t.Fatal("既有目标密码没有保留", err)
					}
				} else if account.EncryptedPassword != "" {
					t.Fatal("管理员密码复制到目标账号")
				}
			}
			if !found {
				t.Fatal("目标实例没有保存")
			}
		})
	}
}

func TestProjectCommandLifecycle(t *testing.T) {
	old := version
	version = "v1.2.3"
	t.Cleanup(func() { version = old })
	f := projectTestFixture(t)
	for _, args := range [][]string{{"configure", "--required-checks", "build"}, {"labels"}, {"install", "action"}, {"install", "mcp"}} {
		prefix := []string{"project", "--dir", f.dir}
		if args[0] == "configure" || args[0] == "labels" {
			prefix = append(prefix, "--instance", "admin")
		}
		args = append(prefix, args...)
		if _, _, err := command(t, args...); err != nil {
			t.Fatal(args, err)
		}
	}
	for _, name := range []string{filepath.Join(".gitea", "workflows", "assistant.yml"), ".mcp.json"} {
		if _, err := os.Stat(filepath.Join(f.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	writes := f.writes
	for _, kind := range []string{"action", "mcp"} {
		if _, _, err := command(t, "project", "--dir", f.dir, "uninstall", kind); err != nil {
			t.Fatal(err)
		}
	}
	if f.writes != writes {
		t.Fatal("卸载修改了平台账号或 secret")
	}
}
func TestProjectCommandDryRunAndValidation(t *testing.T) {
	old := version
	version = "v1.2.3"
	t.Cleanup(func() { version = old })
	f := projectTestFixture(t)
	for _, args := range [][]string{{"configure"}, {"labels"}, {"install", "action"}, {"install", "mcp"}, {"uninstall", "action"}, {"uninstall", "mcp"}} {
		prefix := []string{"project", "--dir", f.dir, "--dry-run"}
		if args[0] == "configure" || args[0] == "labels" {
			prefix = append(prefix, "--instance", "admin")
		}
		args = append(prefix, args...)
		if _, _, err := command(t, args...); err != nil {
			t.Fatal(args, err)
		}
	}
	if f.writes != 0 {
		t.Fatal("演练写平台")
	}
	if _, err := os.Stat(filepath.Join(f.dir, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatal("演练写工具配置")
	}
	if _, _, err := command(t, "project", "--dir", "/missing/project", "uninstall", "mcp"); err == nil {
		t.Fatal("非法项目目录被接受")
	}
	for _, args := range [][]string{{"--instance", "missing", "configure"}, {"--instance", "admin", "--repo", "../repo", "labels"}} {
		args = append([]string{"project", "--dir", f.dir}, args...)
		if _, _, err := command(t, args...); err == nil {
			t.Fatal("无效参数被接受", args)
		}
	}
	if _, _, err := command(t, "project", "--dir", f.dir, "--instance", "admin", "--repo", "team/repo", "labels"); err != nil {
		t.Fatal(err)
	}
}
func TestProjectSelectionAndCredentialFailures(t *testing.T) {
	f := projectTestFixture(t)
	if _, _, _, err := giteaInstance("missing"); err == nil {
		t.Fatal("不存在的实例被猜测")
	}
	if _, _, err := projectRepo(f.dir, "", "https://unknown"); err == nil {
		t.Fatal("无匹配 remote 仍选择了仓库")
	}
	if out, err := exec.Command("git", "-C", f.dir, "remote", "add", "other", f.host+"/other/repo.git").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	if _, _, err := projectRepo(f.dir, "", f.host); err == nil {
		t.Fatal("多个仓库 remote 被猜测")
	}
	_ = os.WriteFile(f.path, []byte("bad"), 0600)
	if _, _, _, err := giteaInstance("admin"); err == nil {
		t.Fatal("损坏凭据被忽略")
	}
}

func TestProjectMCPForceFlagAndEmptyFile(t *testing.T) {
	f := projectTestFixture(t)
	path := filepath.Join(f.dir, ".mcp.json")
	for _, original := range []string{"", "broken", `{"mcpServers":{"gitea":{"command":"other"},"keep":{"command":"keep"}}}`} {
		if err := os.WriteFile(path, []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
		_, errOut, err := command(t, "project", "--dir", f.dir, "install", "mcp", "--force")
		if err != nil {
			t.Fatal(err)
		}
		if original != "" && !strings.Contains(errOut, "备份") {
			t.Fatal("缺少恢复线索", errOut)
		}
		data, _ := os.ReadFile(path)
		if !strings.Contains(string(data), `"gitea"`) || strings.Contains(string(data), "--instance") {
			t.Fatal(string(data))
		}
		if strings.Contains(original, "keep") && !strings.Contains(string(data), `"keep"`) {
			t.Fatal("用户配置丢失")
		}
	}
	if f.requests != 0 {
		t.Fatal("MCP 安装访问平台")
	}
}

func TestProjectSSHRemoteUsesRegisteredWebAddress(t *testing.T) {
	remote, ok := gitremote.ParseGitRemoteURL("ssh://git@gitea.example.com:2222/team/repo.git")
	if !ok || !remoteMatchesHost(remote, "https://gitea.example.com:3000/") {
		t.Fatal("SSH 端口被误当成 Web 端口")
	}
	for _, host := range []string{"https://other.example.com", "https://gitea.example.com/other", "%"} {
		if remoteMatchesHost(remote, host) {
			t.Fatal("错误站点被匹配", host)
		}
	}
	if remoteMatchesHost(gitremote.GitRemote{Host: "%"}, "https://gitea.example.com") {
		t.Fatal("坏地址被接受")
	}
	httpRemote, _ := gitremote.ParseGitRemoteURL("http://gitea.example.com:3000/team/repo.git")
	if remoteMatchesHost(httpRemote, "https://gitea.example.com:3000") {
		t.Fatal("HTTP 地址协议被猜测")
	}
}

func TestProjectActionGenerationIsLocalAndUsesBinaryVersion(t *testing.T) {
	for _, state := range []string{"missing", "invalid", "directory", "unrelated-account"} {
		t.Run(state, func(t *testing.T) {
			old := version
			version = "sha-" + strings.Repeat("a", 40)
			t.Cleanup(func() { version = old })
			f := projectTestFixture(t)
			switch state {
			case "missing":
				if err := os.Remove(f.path); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := os.WriteFile(f.path, []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(f.path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(f.path, 0700); err != nil {
					t.Fatal(err)
				}
			case "unrelated-account":
				f.save(t, []credentials.Gitea{{Name: "me", URL: "https://unrelated.invalid", Username: "me", Token: "other"}})
			}
			if out, err := exec.Command("git", "-C", f.dir, "remote", "remove", "gitea").CombinedOutput(); err != nil {
				t.Fatal(string(out), err)
			}
			t.Setenv("ASSISTANT_CONFIG", filepath.Join(f.dir, "missing.yaml"))
			path := filepath.Join(f.dir, ".gitea", "workflows", "assistant.yml")
			args := []string{"project", "--dir", f.dir, "install", "action"}
			if _, _, err := command(t, append(args, "--dry-run")...); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("演练写文件", err)
			}
			for range 2 {
				if _, _, err := command(t, args...); err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile(path)
			if err != nil || strings.Count(string(data), "assistant:"+version) != 2 || strings.Contains(string(data), "other") {
				t.Fatal(string(data), err)
			}
			// 更换执行项目不改变生成物版本；版本来自二进制，不能取目标项目的 Git 信息。
			other := t.TempDir()
			if out, err := exec.Command("git", "init", "--quiet", other).CombinedOutput(); err != nil {
				t.Fatal(string(out), err)
			}
			if _, _, err := command(t, "project", "--dir", other, "install", "action"); err != nil {
				t.Fatal(err)
			}
			otherData, err := os.ReadFile(filepath.Join(other, ".gitea", "workflows", "assistant.yml"))
			if err != nil || string(otherData) != string(data) {
				t.Fatal("版本随目标项目改变", err)
			}
			if _, _, err := command(t, "project", "--dir", f.dir, "uninstall", "action"); err != nil {
				t.Fatal(err)
			}
			if f.requests != 0 {
				t.Fatal("本地生成访问了站点", f.requests)
			}
		})
	}
}

func TestProjectActionRejectsObsoleteParametersAndPreservesUserWorkflow(t *testing.T) {
	old := version
	version = "v1.2.3"
	t.Cleanup(func() { version = old })
	f := projectTestFixture(t)
	for _, flag := range []string{"--version", "--merge-instance", "--instance", "--repo"} {
		if _, _, err := command(t, "project", "--dir", f.dir, "install", "action", flag, "value"); err == nil {
			t.Fatal("本地生成接受无关参数", flag)
		}
	}
	path := filepath.Join(f.dir, ".gitea", "workflows", "assistant.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	original := []byte("name: 用户 workflow")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := command(t, "project", "--dir", f.dir, "install", "action"); err == nil {
		t.Fatal("覆盖用户 workflow")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(original) || f.requests != 0 {
		t.Fatal("修改了用户配置或访问了站点", err)
	}
}
