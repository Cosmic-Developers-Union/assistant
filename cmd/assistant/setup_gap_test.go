package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
)

// setupFake 是 setup 命令族最小可编程的 Gitea 替身。
//
// setup 要的不是「读仓库现状」，而是一整条写链路——自证管理员、查建号、发令牌、
// 落到仓库与分支保护。这条链只有在一次真正跑通的 setup 里才会全部经过，所以替身
// 必须应答 Admin 接口的每一个端点，且应答要**沿用真实 Gitea 的 JSON 形状**：建号、
// 加协作者、分支保护都走官方 SDK，SDK 解不出字段就会把成功判成失败，测出来的错误
// 跟被测逻辑无关。
type setupFake struct {
	server *httptest.Server
	// login 是 /api/v1/user 在「以管理员令牌访问」时返回的账号名（setup 用它自证管理员）。
	login string
	// loginByToken 是 /api/v1/user 在「以某个机器人令牌访问」时返回的账号名：setup
	// 收敛令牌前会先拿凭据库里的旧令牌问站点「它还是谁的」，替身必须如实回答，
	// 否则复用判定永远走不到。
	loginByToken map[string]string
	// whoami 是 SDK 的 /user 在「以基础（管理员）令牌访问」时返回的账号名，也就是
	// setup 自证管理员用的身份。
	whoami string
	// isAdmin 是 /api/v1/user 的 is_admin：false 时 setup.Run 会以「不是管理员」收场。
	isAdmin bool
	// repos 是 GetRepo 报告为「已存在」的仓库（owner/name）。不在其中的给 404，
	// 让 setup 走建仓分支。
	repos map[string]bool
	// emptyRepos 里的仓库报 empty=true（无默认分支），setup 会跳过分支保护。
	emptyRepos map[string]bool
	// tokens 是 GET /users/{name}/tokens 返回的既有令牌；按令牌名过滤后决定
	// ConvergeToken 是复用还是新建。
	tokens map[string][]map[string]any
	// collaboratorPermissions 决定 GET collaborators/{user}/permission 的授权结果，
	// init 用它复现「令牌没有仓库管理权限」。
	collaboratorPermissions map[string]string
	// failTokenCreate 让 POST /users/{name}/tokens 返回 403，复现「令牌被站点拒绝」。
	failTokenCreate bool
	// recorded 是收到的全部请求（"METHOD PATH"），写操作是否真发生由它证明。
	recorded []string
}

func newSetupFake(t *testing.T) *setupFake {
	t.Helper()
	fake := &setupFake{
		login:                   "root",
		isAdmin:                 true,
		repos:                   map[string]bool{},
		emptyRepos:              map[string]bool{},
		tokens:                  map[string][]map[string]any{},
		collaboratorPermissions: map[string]string{},
		loginByToken:            map[string]string{},
	}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fake.recorded = append(fake.recorded, r.Method+" "+r.URL.Path)
		fake.route(w, r)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

// route 按真实 Gitea 的端点形状分派。顺序敏感：越具体的后缀越先判。
func (fake *setupFake) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case path == "/user":
		// ValidateToken 也用这个端点确认令牌归属：带上某个机器人令牌时答它自己的
		// 账号名，否则答管理员身份。
		identity := fake.login
		if fake.whoami != "" {
			identity = fake.whoami
		}
		if token := botTokenOf(r); token != "" {
			if name, ok := fake.loginByToken[token]; ok {
				identity = name
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "login": identity, "user_name": identity, "is_admin": fake.isAdmin,
		})
	case path == "/version":
		json.NewEncoder(w).Encode(map[string]string{"version": "1.22.0"})
	case path == "/repos/search":
		// SyncMergeSecrets 用 ListAllRepos 扫「merge 是管理员协作者的仓库」再下发
		// MERGE_TOKEN。它按实例上真实存在的仓库遍历，所以替身必须从 seed 出来的
		// fake.repos 还原清单：永远回空数组会让 secret 下发静默跳过，测出来的
		// 「完成」其实一次 secret 都没写。
		fake.writeRepoSearch(w)
	case strings.HasSuffix(path, "/tokens") || strings.Contains(path, "/tokens/"):
		fake.writeTokens(w, r, tokenOwnerName(path))
	case strings.HasSuffix(path, "/collaborators"):
		fake.writeCollaborators(w, r, path)
	case strings.HasSuffix(path, "/permission"):
		// 逐人权限查询走 SDK 的 CollaboratorPermission；应用传入的是登录名
		// （UserName），替身按 {user} 段回话即可，与 Gitea 端点语义一致。
		user := segmentBefore(path, "permission")
		permission := fake.collaboratorPermissions[user]
		if permission == "" {
			permission = "write"
		}
		json.NewEncoder(w).Encode(map[string]any{"permission": permission})
	case strings.HasSuffix(path, "/branch_protections"):
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode([]any{})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"rule_name": "main"})
	case strings.Contains(path, "/branch_protections/"):
		json.NewEncoder(w).Encode(map[string]any{"rule_name": "main"})
	case strings.Contains(path, "/actions/secrets/"):
		w.WriteHeader(http.StatusCreated)
	case strings.HasPrefix(path, "/users/"):
		if fake.userExists(strings.TrimPrefix(path, "/users/")) {
			// 站点把登录名回显成 user_name：SDK 的 User 只按 json:"login_name"
			// 反序列化 user_name，缺了它列表里的协作者会变成空名字，后续按名字
			// 查权限就永远落空。
			name := strings.TrimPrefix(path, "/users/")
			json.NewEncoder(w).Encode(map[string]any{"id": 2, "login": name, "user_name": name})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "user not found"})
	case strings.HasPrefix(path, "/orgs/"):
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "org not found"})
	case strings.HasSuffix(path, "/labels"):
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode([]any{})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 100, "name": "label", "color": "#FFFFFF"})
	case strings.HasPrefix(path, "/repos/"):
		fake.writeRepo(w, r, path)
	default:
		json.NewEncoder(w).Encode(map[string]any{})
	}
}

// userExists 让机器人账号报告为已存在——setup 对已存在的账号只重置密码，不建号，
// 这样替身不必模拟 SDK 的建号请求体解析。
func (fake *setupFake) userExists(name string) bool {
	return name == "ai" || name == "merge"
}

// writeTokens 应答令牌管理端点：GET 报告站点上的既有令牌（不足一页即视为末页，
// 否则 setup 会一直翻页），DELETE 应答未实现也不影响复用路径，POST 回 sha1
// （真实 Gitea 只在创建响应的 sha1 里给一次明文令牌）。
//
// 条目里必须同时有 name 与 token_last_eight：setup 的复用判定是「同名 + 尾 8 位
// 命中」，只给 sha1 会让替身永远判成「需要新建」，把复用测试测成新建测试。
func (fake *setupFake) writeTokens(w http.ResponseWriter, r *http.Request, path string) {
	user := tokenOwnerName(path)
	if r.Method == http.MethodGet {
		if strings.Contains(path, "/tokens/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		page := r.URL.Query().Get("page")
		if page != "" && page != "1" {
			json.NewEncoder(w).Encode([]any{})
			return
		}
		items := fake.tokens[user]
		if items == nil {
			items = []map[string]any{}
		}
		json.NewEncoder(w).Encode(items)
		return
	}
	if fake.failTokenCreate {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"message": "tokens rejected"})
		return
	}
	// 建令牌的应答必须把「新令牌」跟请求方区分开：Agent 归站点所有，收敛后返回的
	// sha1 就是站点签发的新令牌，绝不是提交请求用的那个管理员令牌。回一个固定串
	// 会让「复用」与「重建」在凭据库里长得一模一样，复用断言就此失去意义。
	newToken := "bot-token-created"
	if token := botTokenOf(r); token != "" {
		newToken = token + "-rotated"
	}
	json.NewEncoder(w).Encode(map[string]string{"name": "assistant", "sha1": newToken})
}

// botTokenOf 取出请求携带的机器人令牌（没有则空串）。setup 的裸 HTTP 调用用
// "token <令牌>" 作 Authorization（见 giteaAdmin.do），SDK 调用若带令牌也是同一
// 形状，因此两种前缀都要认。
func botTokenOf(r *http.Request) string {
	header := r.Header.Get("Authorization")
	for _, prefix := range []string{"token ", "Bearer "} {
		if token, ok := strings.CutPrefix(header, prefix); ok {
			return token
		}
	}
	return ""
}

// tokenOwnerName 从 "/users/{name}/tokens" 取出 {name}。
func tokenOwnerName(path string) string {
	rest := strings.TrimPrefix(path, "/users/")
	name, _, _ := strings.Cut(rest, "/")
	return name
}

// writeCollaborators 应答协作者清单：init 会拿它跟开发者身份比对，setup 的
// SyncMergeSecrets 则靠它找出「谁是仓库管理员」。写协作者（PUT）也在这里应答，
// 并把该用户记进已授权清单，使后续的逐人权限查询能看见新增的协作者。
func (fake *setupFake) writeCollaborators(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodGet {
		if user := segmentBefore(path, "collaborators"); user != "" {
			if fake.collaboratorPermissions[user] == "" {
				fake.collaboratorPermissions[user] = "write"
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	owner, name := repoFromRepoPath(path)
	items := []any{}
	for user := range fake.collaboratorPermissions {
		items = append(items, map[string]any{
			"login": user, "user_name": user,
			"permissions": map[string]any{
				"admin": fake.collaboratorPermissions[user] == "admin",
				"push":  true, "pull": true,
			},
		})
	}
	_ = owner
	_ = name
	json.NewEncoder(w).Encode(items)
}

// writeRepo 应答单个仓库：不在 fake.repos 里给 404，在册的给带默认分支的仓库对象。
func (fake *setupFake) writeRepo(w http.ResponseWriter, r *http.Request, path string) {
	owner, name := repoFromRepoPath(path)
	if !fake.repos[owner+"/"+name] {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "repo not found"})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"id":             9,
		"name":           name,
		"full_name":      owner + "/" + name,
		"default_branch": "main",
		"empty":          fake.emptyRepos[owner+"/"+name],
		"owner":          map[string]any{"login": owner},
	})
}

// writeRepoSearch 应答 /repos/search：把 fake.repos 里在册的仓库逐条还原成
// full_name（SDK 只读这个字段拼实例），顺序稳定以免断言随 map 遍历漂移。
func (fake *setupFake) writeRepoSearch(w http.ResponseWriter) {
	names := make([]string, 0, len(fake.repos))
	for fullName := range fake.repos {
		if fullName != "" {
			names = append(names, fullName)
		}
	}
	sort.Strings(names)
	items := make([]any, 0, len(names))
	for _, fullName := range names {
		owner, name, _ := strings.Cut(fullName, "/")
		items = append(items, map[string]any{
			"id": 9, "name": name, "full_name": fullName, "owner": map[string]any{"login": owner},
		})
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": items})
}

// repoFromRepoPath 从 "/repos/{owner}/{name}[/...]" 取出 owner 与 name。
func repoFromRepoPath(path string) (string, string) {
	segments := strings.Split(strings.TrimPrefix(path, "/repos/"), "/")
	if len(segments) < 2 {
		return segments[0], ""
	}
	return segments[0], segments[1]
}

// segmentBefore 返回 path 中 marker 的前一段（"/repos/a/b/collaborators/d/..." 取 "b"）。
func segmentBefore(path, marker string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for index, segment := range segments {
		if segment == marker && index > 0 {
			return segments[index-1]
		}
	}
	return ""
}

// hasRequest 报告替身是否收到过 "METHOD PATH"（前缀匹配，忽略查询串）。
func (fake *setupFake) hasRequest(method, path string) bool {
	for _, entry := range fake.recorded {
		if strings.HasPrefix(entry, method+" "+path) {
			return true
		}
	}
	return false
}

// TestRunSetupDryRunCompletesWithoutWriting 断言 setup 的 dry-run 会把整条链路走完
// 却一个字节都不落盘。
//
// dry-run 是操作者第一次接入新站点时唯一安全的姿势：它真的去站点自证管理员、核对
// 两个机器人账号是否存在、把仓库与分支保护逐项走一遍，然后把「将要写入的配置与
// 凭据」只打印出来。所以这条测试要同时钉住两件事——(1) 链路真的走通（替身收到了
// 账号探测，说明没有半途静默降级）；(2) config.json 与凭据库都没被改写（dry-run
// 若漏一个分支就会把真实文件覆盖掉，而操作者以为自己在「试跑」）。
//
// 钉「账号探测」而不是「令牌收敛」是刻意的：internal/setup 的 ensureAccount 在
// DryRun 下于 ConvergeToken 之前就返回，因此 dry-run 全程一个令牌端点都不碰——把
// 断言写成令牌端点，测的是一条永远不会发生的请求。
func TestRunSetupDryRunCompletesWithoutWriting(t *testing.T) {
	_, credentialsPath := isolateSetupRun(t)

	fake := newSetupFake(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	saveSetupConfig(t, configPath, fake.server.URL)
	grantSetupAdmin(t, fake.server.URL)

	before := readFileOrEmpty(t, configPath)
	credentialsBefore := readFileOrEmpty(t, credentialsPath)

	// 仓库在站点上已存在：dry-run 会走到分支保护之前，却又因 DryRun 不落任何写
	// 操作，正好覆盖「链路走完、一个字节不写」这一支。
	fake.repos["acme/rocket"] = true

	out, err := runSetupCommand(t, configPath,
		"--host", fake.server.URL, "--repos", "acme/rocket", "--dry-run")
	if err != nil {
		t.Fatalf("dry-run 不该失败：%v\n%s", err, out)
	}
	// (1) 链路真的走通：管理员自证与两个账号的存在性核对都发生过。
	if !fake.hasRequest(http.MethodGet, "/api/v1/user") {
		t.Errorf("应从站点自证管理员身份，实际请求：%v", fake.recorded)
	}
	for _, name := range []string{"ai", "merge"} {
		if !fake.hasRequest(http.MethodGet, "/api/v1/users/"+name) {
			t.Errorf("dry-run 应核对 %s 账号是否存在，实际请求：%v", name, fake.recorded)
		}
	}
	if !fake.hasRequest(http.MethodGet, "/api/v1/repos/acme/rocket") {
		t.Errorf("dry-run 应读取目标仓库，实际请求：%v", fake.recorded)
	}
	if !strings.Contains(out, "dry-run") {
		t.Errorf("dry-run 必须明说自己没写盘：\n%s", out)
	}
	// (2) 一个字节都没写：配置与凭据库都比对字节，凭据库内容为空即「没写」。
	// 用内容比对而不是 os.Stat：隔离目录里的凭据文件由 grantSetupAdmin 预先写好，
	// 只判存在与否会把「预先写好的那一份」当成 dry-run 的产物。
	if got := readFileOrEmpty(t, configPath); got != before {
		t.Errorf("dry-run 改写了配置：\nbefore=%s\nafter=%s", before, got)
	}
	if got := readFileOrEmpty(t, credentialsPath); got != credentialsBefore {
		t.Errorf("dry-run 改写了凭据库：\nbefore=%s\nafter=%s", credentialsBefore, got)
	}
}

// TestRunSetupFailsWhenTokenConvergenceRejected 断言站点拒绝收敛机器人令牌时 setup
// 必须失败，而不是带着「账号已就绪」的错觉继续。
//
// 令牌是 setup 唯一真正交付给运行时的东西：config.json 里的通道、daemon 的每次
// 调用都靠它。站点拒绝创建令牌（管理令牌权限不足是最常见的原因）若被吞掉，操作者
// 会拿到一份配置完整、令牌却是空的站点——daemon 起不来，且错误现象与配置无关，
// 排查要从头来过。
func TestRunSetupFailsWhenTokenConvergenceRejected(t *testing.T) {
	isolateSetupRun(t)

	fake := newSetupFake(t)
	fake.failTokenCreate = true
	configPath := filepath.Join(t.TempDir(), "config.json")
	saveSetupConfig(t, configPath, fake.server.URL)
	grantSetupAdmin(t, fake.server.URL)

	// 非 dry-run：dry-run 在收敛令牌时只打印、不发请求（见 ensureAccount），
	// 因此「站点拒绝建令牌」这一支只有真正落盘的运行才会经过。
	out, err := runSetupCommand(t, configPath, "--host", fake.server.URL)
	if err == nil {
		t.Fatalf("站点拒绝发令牌时 setup 应报错，而不是报告完成：\n%s", out)
	}
	if !strings.Contains(err.Error(), "tokens rejected") {
		t.Errorf("错误应来自站点拒绝这次令牌创建：%v", err)
	}
	if strings.Contains(out, "完成：") {
		t.Errorf("失败不该继续打印完成：\n%s", out)
	}
}

// TestRunSetupReusesValidCredentialsInsteadOfCreating 断言凭据库里已有且仍有效的
// 机器人令牌会被复用，不再新建。
//
// 这条口径决定 setup 能不能反复跑：每次重跑都新建令牌会迅速撞上 Gitea 的令牌数
// 上限，旧令牌还散落在各处无人清理；反过来，把「能复用」写成「永远新建」虽然不报
// 错，却让「重跑 setup」变成一件需要人工收拾的事。断言直接看替身有没有收到
// POST /users/{name}/tokens（建令牌的端点），以及凭据库里令牌有没有被换掉。
//
// 站点必须如实回答 ValidateToken 的归属查询：setup 在收敛前先用旧令牌访问
// /api/v1/user 确认它仍属于该账号，替身若把任何令牌都答成 root，这块逻辑就测不到。
//
// 注意复用只到 purpose 这一层：cmd/assistant 的 tokenForPurpose 只按 (host,
// purpose) 解析（store.CredentialFor），所以凭据库里 review/merge 各一条、账号名
// 与机器人一致时，setup.Options.existingToken 就能拿到令牌去问站点「它还是谁」。
func TestRunSetupReusesValidCredentialsInsteadOfCreating(t *testing.T) {
	_, credentialPath := isolateSetupRun(t)

	fake := newSetupFake(t)
	const reviewerToken = "same-tail-token-abcdefgh"
	const mergerToken = "merge-tail-token-ijklmnop"
	// 两个令牌在站点上都还属于对应账号，因此都判定为「可复用」。
	fake.loginByToken[reviewerToken] = "ai"
	fake.loginByToken[mergerToken] = "merge"
	// 站点上的令牌清单也要如实回答：收敛令牌时先列举该账号现有令牌，再按尾 8 位
	// 认出「保留下来的那条」，认出才走复用分支。清单为空时站点会认为旧令牌不存在，
	// 于是重新签发一条——复用与重建在日志与凭据库里就长得一模一样，这条断言也就
	// 失去了意义。Gitea 的列举应答只带 token_last_eight（没有 sha1），所以比对只能
	// 靠它，替身必须给出与种子令牌一致的尾 8 位。
	fake.tokens["ai"] = []map[string]any{
		{"id": 1, "name": "assistant", "token_last_eight": "abcdefgh"},
	}
	fake.tokens["merge"] = []map[string]any{
		{"id": 2, "name": "assistant", "token_last_eight": "ijklmnop"},
	}
	fake.repos["acme/rocket"] = true

	configPath := filepath.Join(t.TempDir(), "config.json")
	saveSetupConfig(t, configPath, fake.server.URL)
	// 一次写全三条：管理员凭据是 setup 能启动的前提，两条机器人令牌是复用的输入。
	writePurposeCredentials(t, []credentials.Credential{
		{Host: fake.server.URL, User: "root", Purpose: credentials.PurposeAdmin, Token: "t-admin"},
		{Host: fake.server.URL, User: "ai", Purpose: credentials.PurposeReview, Token: reviewerToken},
		{Host: fake.server.URL, User: "merge", Purpose: credentials.PurposeMerge, Token: mergerToken},
	})

	out, err := runSetupCommand(t, configPath, "--host", fake.server.URL, "--repos", "acme/rocket")
	if err != nil {
		t.Fatalf("可复用时 setup 不该失败：%v\n%s", err, out)
	}
	if !strings.Contains(out, "复用 ai 的现有令牌") {
		t.Errorf("setup 应报告复用了 ai 的现有令牌：\n%s", out)
	}
	if !strings.Contains(out, "复用 merge 的现有令牌") {
		t.Errorf("setup 应报告复用了 merge 的现有令牌：\n%s", out)
	}
	// 复用的反面就是这台替身收到的建令牌请求：一条都不该有。
	for _, name := range []string{"ai", "merge"} {
		if fake.hasRequest(http.MethodPost, "/api/v1/users/"+name+"/tokens") {
			t.Errorf("%s 的令牌仍有效，不该新建：%v", name, fake.recorded)
		}
	}

	// 复用必须是「原样保留」：凭据库里两条令牌都不该被换掉，否则下一次运行又会
	// 判成失效并重建，复用口径形同虚设。
	store, err := credentials.Load(credentialPath)
	if err != nil {
		t.Fatalf("回读凭据库：%v", err)
	}
	if !storeHasToken(store, credentials.PurposeReview, reviewerToken) {
		t.Errorf("ai 的令牌应原样保留，got %+v", store.Credentials)
	}
	if !storeHasToken(store, credentials.PurposeMerge, mergerToken) {
		t.Errorf("merge 的令牌应原样保留，got %+v", store.Credentials)
	}
}

// storeHasToken 报告凭据库里某用途的令牌是否正是期望值。
func storeHasToken(store *credentials.File, purpose, token string) bool {
	for _, credential := range store.Credentials {
		if credential.Purpose == purpose {
			return credential.Token == token
		}
	}
	return false
}

// TestRunSetupWritesConfigAndCredentials 断言非 dry-run 时配置与机器人令牌都真的
// 落到隔离的临时目录里，且配置里的通道被就地更新为 setup 结果。
//
// 这是 setup 唯一的「成功即写盘」路径：写错落点或漏写凭据，操作者拿到的是一份空
// 配置与一个空凭据库，而站点上账号令牌都已建好——下次运行只能靠人回忆。断言直接
// 读回文件内容，而不是只看退出码。
func TestRunSetupWritesConfigAndCredentials(t *testing.T) {
	_, credentialPath := isolateSetupRun(t)

	fake := newSetupFake(t)
	fake.repos["acme/rocket"] = true
	// 站点上 merge 已是目标仓库的管理员协作者——这是 SyncMergeSecrets 下发
	// MERGE_TOKEN 的前提，缺了它 secret 会静默跳过。权限走
	// collaboratorPermission 的逐人查询端点，所以这里同时说清 merge 的管理员
	// 身份；写协作者那条 PUT 也顺手把它记进已授权清单。
	fake.collaboratorPermissions["merge"] = "admin"
	configPath := filepath.Join(t.TempDir(), "config.json")
	saveSetupConfig(t, configPath, fake.server.URL)
	grantSetupAdmin(t, fake.server.URL)

	out, err := runSetupCommand(t, configPath, "--host", fake.server.URL, "--repos", "acme/rocket")
	if err != nil {
		t.Fatalf("setup 不该失败：%v\n%s", err, out)
	}
	if !strings.Contains(out, "配置已写入") {
		t.Errorf("成功路径应报告已写入：\n%s", out)
	}
	if !fake.hasRequest(http.MethodPut, "/api/v1/repos/acme/rocket/actions/secrets/") {
		t.Errorf("应下发机器人令牌到仓库 secret：%v", fake.recorded)
	}

	file, err := instances.Load(configPath)
	if err != nil {
		t.Fatalf("回读配置：%v", err)
	}
	channel, ok := findGiteaChannel(file, fake.server.URL)
	if !ok || channel == nil {
		t.Fatalf("写回的配置里没有站点通道：%+v", file.Channels)
	}
	if channel.Reviewer != "ai" || channel.Merger != "merge" {
		t.Errorf("通道身份应写为 ai/merge，got reviewer=%q merger=%q", channel.Reviewer, channel.Merger)
	}
	if len(channel.Repos) != 1 || channel.Repos[0].Name != "acme/rocket" {
		t.Errorf("通道仓库应写为 acme/rocket，got %+v", channel.Repos)
	}

	store, err := credentials.Load(credentialPath)
	if err != nil {
		t.Fatalf("回读凭据库：%v", err)
	}
	if len(store.Credentials) == 0 {
		t.Fatal("非 dry-run 必须把机器人令牌写入凭据库")
	}
}

// TestRunSetupReportsUnwritableCredentialPath 断言机器人令牌写不进去时报错。
//
// 令牌比配置更难补救：它只在创建响应的 sha1 里出现一次，丢了就只能重建账号。落盘
// 失败若被吞掉，站点上会留下一个无人知晓其令牌的机器人账号，且每次重跑 setup 都
// 会再建一批令牌。
func TestRunSetupReportsUnwritableCredentialPath(t *testing.T) {
	isolateSetupRun(t)

	fake := newSetupFake(t)
	fake.repos["acme/rocket"] = true
	configPath := filepath.Join(t.TempDir(), "config.json")
	saveSetupConfig(t, configPath, fake.server.URL)
	grantSetupAdmin(t, fake.server.URL)

	// 凭据落点的父路径是文件：credentials.Save 必然失败。
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(blocker, "credentials.json"))

	out, err := runSetupCommand(t, configPath, "--host", fake.server.URL, "--repos", "acme/rocket")
	if err == nil {
		t.Fatalf("令牌写不进去时 setup 应报错，而不是报告完成：\n%s", out)
	}
	if strings.Contains(out, "完成：") {
		t.Errorf("写入失败不该继续打印完成：\n%s", out)
	}
	// 配置写成功了（它在凭据之前）：断言它真的落盘，才能证明失败点确实在凭据，
	// 而不是更早的配置写入。
	if _, err := instances.Load(configPath); err != nil {
		t.Errorf("配置应先于凭据写完：%v", err)
	}
}

// TestLoadInstanceFileForSetupTreatsMissingExplicitFileAsNew 断言 setup 读取已有配置
// 时把「显式路径不存在」当成首次创建，而兄弟 .env 坏掉时必须当场报错。
//
// 「显式路径不存在 → 首次创建」是 setup 幂等的前提：它允许 `--config ./config.json`
// 在干净目录里跑第一次。反过来，文件存在却读不出来时若也被当成新配置，就会把整份已有
// 实例配置覆盖成空白——这是不可逆的破坏，所以坏 .env 必须立刻冒出来。
func TestLoadInstanceFileForSetupTreatsMissingExplicitFileAsNew(t *testing.T) {
	// 缺省落点分支要用隔离环境，先钉住环境再动文件系统。
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	// (a) 显式指向不存在的文件：回显该路径 + nil 配置，不报错。
	missing := filepath.Join(t.TempDir(), "config.json")
	path, file, err := loadInstanceFileForSetup(missing)
	if err != nil {
		t.Fatalf("指向不存在的显式路径不该报错：%v", err)
	}
	if path != missing {
		t.Errorf("应回显显式路径 %q，got %q", missing, path)
	}
	if file != nil {
		t.Errorf("文件不存在时不该返回配置，got %+v", file)
	}

	// (b) 兄弟 .env 坏掉：config.LoadBeside 失败，必须原样上报。
	brokenDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(brokenDir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, ".env"), []byte("NOT_A_VALID_LINE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadInstanceFileForSetup(filepath.Join(brokenDir, "config.json")); err == nil {
		t.Fatal("兄弟 .env 语法坏掉时应报错，而不是当作新配置继续")
	}

	// (c) 缺省落点：显式参数为空时退回 ASSISTANT_CONFIG（或标准路径），文件不存在
	// 仍不报错——setup 只在缺省目录里也可能跑第一次。
	isolated := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("ASSISTANT_CONFIG", isolated)
	path, file, err = loadInstanceFileForSetup("")
	if err != nil {
		t.Fatalf("缺省落点不存在时不该报错：%v", err)
	}
	if path != isolated {
		t.Errorf("应退回 ASSISTANT_CONFIG %q，got %q", isolated, path)
	}
	if file != nil {
		t.Errorf("文件不存在时不该返回配置，got %+v", file)
	}

	// (d) 标准路径分支：ASSISTANT_CONFIG 也空时由 instances.DefaultConfigPath 决定，
	// 隔离后的 XDG 目录里没有配置文件，于是同样只回显路径、nil 配置、不报错。
	t.Setenv("ASSISTANT_CONFIG", "")
	standard, err := instances.DefaultConfigPath()
	if err != nil {
		t.Fatalf("隔离环境下应能解析标准配置路径：%v", err)
	}
	path, file, err = loadInstanceFileForSetup("")
	if err != nil {
		t.Fatalf("标准落点不存在时不该报错：%v", err)
	}
	if path != standard {
		t.Errorf("应退回平台标准路径 %q，got %q", standard, path)
	}
	if file != nil {
		t.Errorf("标准落点不存在时不该返回配置，got %+v", file)
	}
}

// TestSetupDryRunKeepsExistingRepoNames 断言 --repos 缺省时回退到配置里已登记的
// 仓库清单：setup 的增量姿势（先在配置里写好仓库，再跑 setup 让它去站点落地）全靠
// 这一步，回退失效会让第二次 setup 变成「一个仓库都不配置」的空跑。
func TestSetupDryRunKeepsExistingRepoNames(t *testing.T) {
	isolateSetupRun(t)

	fake := newSetupFake(t)
	fake.repos["acme/rocket"] = true
	configPath := filepath.Join(t.TempDir(), "config.json")
	saveSetupConfig(t, configPath, fake.server.URL, instances.Repo{Name: "acme/rocket"})
	grantSetupAdmin(t, fake.server.URL)

	out, err := runSetupCommand(t, configPath, "--host", fake.server.URL, "--dry-run")
	if err != nil {
		t.Fatalf("dry-run 不该失败：%v\n%s", err, out)
	}
	// 已有清单里的仓库必须被真的处理过（读取它是 setupRepository 的第一步）。
	if !fake.hasRequest(http.MethodGet, "/api/v1/repos/acme/rocket") {
		t.Errorf("配置里已登记的仓库应被处理，实际请求：%v", fake.recorded)
	}
	if !strings.Contains(out, "repos=1") {
		t.Errorf("应保留已有仓库清单（repos=1）：\n%s", out)
	}
}

// isolateSetupRun 隔离 setup 端到端测试的全部落点：凭据库、配置目录、数据目录都
// 指向临时目录，绝不碰真实用户配置目录。返回一个空的配置落点与凭据库路径（供断言
// 「有没有写」）。
func isolateSetupRun(t *testing.T) (configPath, credentialPath string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	credentialPath = isolateCredentials(t)
	return filepath.Join(t.TempDir(), "config.json"), credentialPath
}

// readFileOrEmpty 读取文件内容，不存在时返回空串（供「有没有被改写」的比对）。
func readFileOrEmpty(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(content)
}
