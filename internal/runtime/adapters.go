package runtime

import (
	"context"
	"encoding/base64"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bytes"
	"github.com/Cosmic-Developers-Union/assistant/internal/claude"
	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
	"github.com/Cosmic-Developers-Union/assistant/skills"
	"github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"
)

// GiteaAPI 是发现事件所需的只读平台面。
type GiteaAPI interface {
	ListRepositories(context.Context) ([]status.Repository, error)
	ListOpenPullRequests(context.Context, status.Repository) ([]status.PullRequest, error)
	ListTriageIssues(context.Context, status.Repository) ([]status.Issue, error)
	ListMentionedPullRequests(context.Context, string) ([]status.PullRequest, error)
	ListPullReviews(context.Context, status.Repository, int64) ([]status.Review, error)
	ListIssueCommentsSince(context.Context, status.Repository, int64, time.Time) ([]status.Comment, error)
}

// GiteaSource 发现账号可见标签待办与全站新提及，召唤不依赖仓库 workflow。
type GiteaSource struct {
	API       GiteaAPI
	Host      string
	Identity  string
	ReviewBot string
	TriageBot string
}

// Poll 每轮重读平台事实，不从 agent 输出或历史结局构造待办。
func (s GiteaSource) Poll(ctx context.Context) ([]Event, error) {
	repos, repoErr := s.API.ListRepositories(ctx)
	var result []Event
	var failures []error
	if repoErr != nil {
		failures = append(failures, repoErr)
	}
	for _, repo := range repos {
		if s.ReviewBot != "" {
			pulls, err := s.API.ListOpenPullRequests(ctx, repo)
			if err != nil {
				failures = append(failures, fmt.Errorf("读取 %s PR: %w", repo.FullName(), err))
			}
			for _, pr := range pulls {
				if !pr.Open || pr.Draft || isWIP(pr.Title) {
					continue
				}
				labeled := false
				for _, label := range pr.Labels {
					if label.Name == status.LabelReview {
						labeled = true
					}
				}
				if labeled {
					result = append(result, Event{Bot: s.ReviewBot, Kind: "gitea-review", Host: s.Host, Repo: repo.FullName(), Number: pr.Index, Title: pr.Title, Head: pr.HeadSHA, Ref: fmt.Sprintf("refs/pull/%d/head", pr.Index), Base: pr.BaseRef})
				}
			}
		}
		if s.TriageBot != "" {
			issues, err := s.API.ListTriageIssues(ctx, repo)
			if err != nil {
				failures = append(failures, fmt.Errorf("读取 %s 分诊: %w", repo.FullName(), err))
			}
			for _, issue := range issues {
				if !issue.IsPull {
					result = append(result, Event{Bot: s.TriageBot, Kind: "triage", Host: s.Host, Repo: repo.FullName(), Number: issue.Index, Title: issue.Title, Base: "HEAD"})
				}
			}
		}
	}
	if s.ReviewBot != "" && s.Identity != "" {
		mentioned, err := s.API.ListMentionedPullRequests(ctx, s.Identity)
		if err != nil {
			failures = append(failures, err)
		}
		seen := map[string]bool{}
		for _, event := range result {
			seen[event.key()] = true
		}
		for _, pr := range mentioned {
			if !pr.Open || pr.Draft || isWIP(pr.Title) {
				continue
			}
			ev := Event{Bot: s.ReviewBot, Kind: "gitea-review", Host: s.Host, Repo: pr.Repository.FullName(), Number: pr.Index, Title: pr.Title, Head: pr.HeadSHA, Ref: fmt.Sprintf("refs/pull/%d/head", pr.Index), Base: pr.BaseRef}
			if seen[ev.key()] {
				continue
			}
			requested, err := s.freshMention(ctx, pr)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if requested {
				result = append(result, ev)
				seen[ev.key()] = true
			}
		}
	}

	return result, errors.Join(failures...)
}

func (s GiteaSource) freshMention(ctx context.Context, pr status.PullRequest) (bool, error) {
	reviews, err := s.API.ListPullReviews(ctx, pr.Repository, pr.Index)
	if err != nil {
		return false, err
	}
	var since time.Time
	for _, review := range reviews {
		if review.User == s.Identity && review.State != status.ReviewStatePending && review.State != status.ReviewStateRequestReview && review.Submitted.After(since) {
			since = review.Submitted
		}
	}
	// 全站搜索已由 Gitea 解析正文/评论的 mention；没有任何回应时直接处理。
	if since.IsZero() {
		return true, nil
	}
	comments, err := s.API.ListIssueCommentsSince(ctx, pr.Repository, pr.Index, since)
	if err != nil {
		return false, err
	}
	for _, comment := range comments {
		if comment.User != s.Identity && comment.Created.After(since) && mentionsAccount(comment.Body, s.Identity) {
			return true, nil
		}
	}
	return false, nil
}
func mentionsAccount(text, user string) bool {
	text = strings.ToLower(text)
	mention := "@" + strings.ToLower(user)
	for {
		before, after, ok := strings.Cut(text, mention)
		if !ok {
			return false
		}
		boundary := func(c byte) bool {
			return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
		}
		if (len(before) == 0 || !boundary(before[len(before)-1])) && (len(after) == 0 || !boundary(after[0])) {
			return true
		}
		text = after
	}
}

func isWIP(title string) bool {
	text := strings.ToUpper(strings.TrimSpace(title))
	return strings.HasPrefix(text, "WIP:") || strings.HasPrefix(text, "[WIP]") || text == "WIP" || strings.HasPrefix(text, "WIP ")
}

// WorktreeWorkspace 在受管克隆中准备独立 worktree，并串行化共享 Git 元数据。
type WorktreeWorkspace struct {
	Root    string
	Connect Connect
	Spec    WorkspaceSpec
}

func eventTemplate(value string, ev Event) string {
	values := map[string]string{"event.repo": ev.Repo, "event.pr.ref": ev.Ref, "event.pr.head": ev.Head, "event.base": ev.Base}
	return reference.ReplaceAllStringFunc(value, func(match string) string { return values[strings.TrimSpace(match[2:len(match)-2])] })
}

// Prepare 钉住发现时的 head；平台 head 漂移则放行下一轮重试。
func (w *WorktreeWorkspace) Prepare(ctx context.Context, ev Event) (string, func(), error) {
	mutex := workspaceLock(w.Root + "/" + w.Connect.URL)
	mutex.Lock()
	defer mutex.Unlock()
	repo := eventTemplate(w.Spec.Repo, ev)
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || !filepath.IsLocal(repo) || strings.Contains(repo, "..") || strings.Contains(repo, "\\") {
		return "", nil, fmt.Errorf("工作区仓库必须是 owner/name")
	}
	host, err := url.Parse(w.Connect.URL)
	if err != nil {
		return "", nil, err
	}
	// host 与 URL 路径一起散列，避免子路径部署或不同端口相撞。
	local := LocalSession{Root: filepath.Join(w.Root, "workspaces")}
	eventDir, err := local.Dir(ctx, ev)
	if err != nil {
		return "", nil, err
	}
	clone := filepath.Join(w.Root, "repos", safeHost(w.Connect.URL), owner, name)
	if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
		return "", nil, err
	}
	env := gitEnvironment(w.Connect.Token)
	if _, err := os.Stat(filepath.Join(clone, ".git")); errors.Is(err, os.ErrNotExist) {
		if _, err := git(ctx, "", env, "clone", "--quiet", strings.TrimRight(host.String(), "/")+"/"+repo+".git", clone); err != nil {
			return "", nil, err
		}
	} else if err != nil {
		return "", nil, err
	}
	if _, err := git(ctx, clone, env, "fetch", "--quiet", "--prune", "origin"); err != nil {
		return "", nil, err
	}
	// 基线跟随服务端默认分支；不假定所有仓库都叫 main。
	base, err := git(ctx, clone, nil, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", nil, err
	}
	if _, err := git(ctx, clone, nil, "checkout", "--quiet", "--detach", "--force", base); err != nil {
		return "", nil, err
	}
	ref := eventTemplate(w.Spec.Ref, ev)
	if ref == "" {
		if ev.Kind == "gitea-review" {
			ref = ev.Ref
		} else {
			ref = base
		}
	}
	pinned := ref
	if ev.Kind == "gitea-review" {
		if strings.HasPrefix(ref, "-") {
			return "", nil, fmt.Errorf("工作区引用不能是选项")
		}
		if _, err := git(ctx, clone, env, "fetch", "--quiet", "origin", ref); err != nil {
			return "", nil, err
		}
		head, err := git(ctx, clone, nil, "rev-parse", "FETCH_HEAD")
		if err != nil {
			return "", nil, err
		}
		if head != ev.Head {
			return "", nil, fmt.Errorf("PR head 已变化，等待下一轮重新发现")
		}
		pinned = head
	}
	dir := filepath.Join(eventDir, "tree")
	_, _ = git(ctx, clone, nil, "worktree", "remove", "--force", dir)
	if err := os.RemoveAll(dir); err != nil {
		return "", nil, err
	}
	if _, err := git(ctx, clone, nil, "worktree", "add", "--quiet", "--detach", dir, pinned); err != nil {
		return "", nil, err
	}
	cleanup := func() {
		mutex.Lock()
		defer mutex.Unlock()
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = git(cleanCtx, clone, nil, "worktree", "remove", "--force", dir)
	}
	if err := pinStandard(clone, dir); err != nil {
		// 此处仍持有锁，不调用再次上锁的 cleanup。
		_, _ = git(ctx, clone, nil, "worktree", "remove", "--force", dir)
		return "", nil, err
	}
	return dir, cleanup, nil
}

func gitEnvironment(token string) []string {
	return []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+token))}
}
func git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), env...)
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git %s 执行失败: %w", args[0], err)
	}
	return strings.TrimSpace(string(output)), nil
}

// DirectoryWorkspace 为消息用户保留稳定工作目录。
type DirectoryWorkspace struct{ Root string }

// Prepare 按通道与用户散列隔离目录，保留用户文件。
func (w DirectoryWorkspace) Prepare(ctx context.Context, ev Event) (string, func(), error) {
	dir := filepath.Join(w.Root, safeHost(ev.Host+"\x00"+ev.User))
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	return dir, func() {}, nil
}

// ClaudeRunner 复用已有进程与流解析适配器，参数只取自 AgentSpec 与事件。
type ClaudeRunner struct {
	Process claude.Runner
	Debug   bool
	Log     func(string, ...any)
}

// Run 显式固定会话根与项目名，已有记录才 --resume。
func (r ClaudeRunner) Run(ctx context.Context, spec AgentSpec, ev Event, dir, id string) (Outcome, error) {
	outcome := claude.NewOutcome()
	_, statErr := os.Stat(transcript(spec.SessionDir, id))
	resume := statErr == nil
	if r.Debug && r.Log != nil {
		r.Log("会话决策: bot=%s kind=%s id=%s resume=%t（本地记录存在时续接）", ev.Bot, ev.Kind, id, resume)
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return outcome, statErr
	}
	servers := map[string]MCP{}
	for name, server := range spec.Servers {
		servers[name] = server
	}
	data, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {

		return outcome, err
	}
	// 临时工具配置包含平台令牌，执行结束立即删除，不进入会话副本。
	file, err := os.CreateTemp(spec.SessionDir, ".mcp-*.json")
	if err != nil {
		return outcome, err
	}
	path := file.Name()
	defer os.Remove(path)
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return outcome, err
	}
	prompt := spec.Prompt
	if ev.Kind == "chat" {
		prompt = ev.Text + "\n" + prompt
	} else {
		command := "review pr"
		if ev.Kind == "triage" {
			command = "triage issue"
		}
		prompt = fmt.Sprintf("%s %s#%d\n站点: %s\n标题: %s\nhead: %s\n%s", command, ev.Repo, ev.Number, ev.Host, ev.Title, ev.Head, prompt)
	}
	system := spec.System
	projectRules, err := os.ReadFile(filepath.Join(dir, ".assistant", "review.md"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return outcome, fmt.Errorf("读取项目会话约定: %w", err)
	}
	if len(projectRules) > 0 {
		system += "\n项目约定:\n" + string(projectRules)
	}
	if ev.Kind != "chat" {
		system = skills.ReviewPrompt() + "\n" + system
	}
	args := claude.ArgsOptions{Prompt: prompt, AppendSystemPrompt: system, MCPConfigPath: path, StrictMCP: true, SettingSources: "project", PermissionMode: "auto", Verbose: true, Model: spec.Model, Session: claude.Session{ID: id, Resume: resume}, MaxTurns: 300}
	bin := spec.Bin
	if bin == "" {
		bin = "claude"
	}
	process := r.Process
	if process == nil {
		process = claude.NewExecRunner()
	}
	err = process.Run(ctx, claude.BuildSpec(args, bin, dir, claude.SessionEnv(claude.EnvConfig{ConfigDir: spec.SessionDir, ProjectDirName: "assistant", PinProjectDir: true}), 0), func(line []byte) {
		claude.Feed(&outcome, line, claude.ProgressTerse, func(text string) {
			if r.Log != nil {
				for _, server := range spec.Servers {
					for key, value := range server.Env {
						if secretKey(key) && value != "" {
							text = strings.ReplaceAll(text, value, "***")
						}
					}
				}
				r.Log("%s", text)
			}
		})
	})
	outcome.Resumed = resume
	if err == nil && (!outcome.SawResult || outcome.IsError) {
		err = fmt.Errorf("agent 会话未正常结束（%s）", outcome.Subtype)
	}
	secrets := []string{}
	for _, server := range spec.Servers {
		for key, value := range server.Env {
			if secretKey(key) {
				secrets = append(secrets, value)
			}
		}
	}
	err = credentials.RedactError(err, secrets...)

	return outcome, err
}

// S3Bucket 只存放会话归档，不创建桶或改变远端配置。
type S3Bucket struct {
	Client *minio.Client
	Bucket string
	Prefix string
}

// NewS3Bucket 在本地装配客户端，真正取用时才访问网络。
func NewS3Bucket(cfg SessionConfig) (*S3Bucket, error) {
	endpoint := cfg.Endpoint
	secure := true
	if rest, ok := strings.CutPrefix(endpoint, "https://"); ok {
		endpoint = rest
	} else if rest, ok := strings.CutPrefix(endpoint, "http://"); ok {
		endpoint = rest
		secure = false
	}
	client, err := minio.New(endpoint, &minio.Options{Creds: miniocreds.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""), Secure: secure})
	if err != nil {
		return nil, fmt.Errorf("配置 S3: %w", err)
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &S3Bucket{Client: client, Bucket: cfg.Bucket, Prefix: prefix}, nil
}

// Fetch 保留不存在与请求失败的区别，后者不得退回新会话。
func (s *S3Bucket) Fetch(ctx context.Context, key string) ([]byte, bool, error) {
	object, err := s.Client.GetObject(ctx, s.Bucket, s.Prefix+key, minio.GetObjectOptions{})
	if err != nil {
		return nil, false, err
	}
	defer object.Close()
	data, err := io.ReadAll(io.LimitReader(object, 256<<20+1))
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return nil, false, nil
	}
	if len(data) > 256<<20 {
		return nil, false, fmt.Errorf("会话归档过大")
	}
	return data, err == nil, err
}

// Put 覆盖一个会话对象，使重试固化幂等。
func (s *S3Bucket) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.Client.PutObject(ctx, s.Bucket, s.Prefix+key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: "application/gzip"})
	return err
}

func pinStandard(clone, dir string) error {
	for _, name := range []string{".claude", ".assistant"} {
		target := filepath.Join(dir, name)
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		source := filepath.Join(clone, name)
		if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := os.CopyFS(target, os.DirFS(source)); err != nil {
			return err
		}
	}
	return nil
}

var workspaceLocks sync.Map

func workspaceLock(key string) *sync.Mutex {
	value, _ := workspaceLocks.LoadOrStore(key, new(sync.Mutex))
	return value.(*sync.Mutex)
}
