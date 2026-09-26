package main

import (
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
)

// 站点与账号用常量固定下来：这些用例断言的是「账号解析」的判定，不关心站点真伪。
const loginTokenHost = "https://gitea.example.com"

// storeWith 造一份只含指定凭据的凭据库；identity 为空表示该站点没有当前登录身份。
func storeWith(t *testing.T, identity string, users ...string) *credentials.File {
	t.Helper()
	file := &credentials.File{Version: credentials.CurrentVersion}
	if identity != "" {
		file.SetIdentity(credentials.Identity{Host: loginTokenHost, User: identity})
	}
	for _, user := range users {
		file.SetCredential(credentials.Credential{
			Host: loginTokenHost, User: user,
			Purpose: credentials.PurposeMCP, Token: "t-" + user,
		})
	}
	return file
}

// TestResolveTokenUserPrefersExplicitFlag 断言 --user 的优先级最高：它是操作者
// 明确点名的账号，即使站点有当前登录身份、凭据库里还有别的账号，也必须听它的，
// 否则在同一个站点上轮换另一个账号的令牌会「悄悄换成自己那条」。
func TestResolveTokenUserPrefersExplicitFlag(t *testing.T) {
	file := storeWith(t, "alice", "alice", "bob")
	user, err := resolveTokenUser(file, loginTokenHost, "  bob  ")
	if err != nil {
		t.Fatalf("resolveTokenUser: %v", err)
	}
	if user != "bob" {
		t.Errorf("user = %q, want bob（--user 必须去空白后优先）", user)
	}
}

// TestResolveTokenUserFallsBackToIdentity 断言没有 --user 时用当前登录身份裁决：
// 站点同时存在多个账号时，「现在以谁登录」是唯一有依据的答案，猜别的账号会让
// 操作者看到一份与自己无关的令牌。
func TestResolveTokenUserFallsBackToIdentity(t *testing.T) {
	file := storeWith(t, "bob", "alice", "bob")
	user, err := resolveTokenUser(file, loginTokenHost, "")
	if err != nil {
		t.Fatalf("resolveTokenUser: %v", err)
	}
	if user != "bob" {
		t.Errorf("user = %q, want bob（身份记录优先于唯一账号）", user)
	}
}

// TestResolveTokenUserUsesSoleAccount 断言只有一个账号时直接用它：新机器上
// login add 之后还没有身份记录，此时唯一账号是不言自明的答案。
func TestResolveTokenUserUsesSoleAccount(t *testing.T) {
	file := storeWith(t, "", "alice")
	user, err := resolveTokenUser(file, loginTokenHost, "")
	if err != nil {
		t.Fatalf("resolveTokenUser: %v", err)
	}
	if user != "alice" {
		t.Errorf("user = %q, want alice", user)
	}
}

// TestResolveTokenUserRejectsEmptyStore 断言凭据库里没有该站点账号时给出注册引导，
// 而不是回一个空账号名让后续操作失败在更远的地方。
func TestResolveTokenUserRejectsEmptyStore(t *testing.T) {
	file := storeWith(t, "")
	_, err := resolveTokenUser(file, loginTokenHost, "")
	if err == nil || !strings.Contains(err.Error(), "先 assistant login add") {
		t.Errorf("err = %v, want 含「先 assistant login add」", err)
	}
	if err != nil && !strings.Contains(err.Error(), loginTokenHost) {
		t.Errorf("err = %v, want 点名站点 %s", err, loginTokenHost)
	}
}

// TestResolveTokenUserRejectsAmbiguousAccounts 断言多账号且无身份记录时报错并点名
// 每个候选账号：这是唯一「必须让人来决定」的情形，@ 前缀与顿号分隔是错误信息的
// 约定写法（prefixedUsers 的唯一消费者）。
func TestResolveTokenUserRejectsAmbiguousAccounts(t *testing.T) {
	file := storeWith(t, "", "alice", "bob")
	_, err := resolveTokenUser(file, loginTokenHost, "")
	if err == nil {
		t.Fatal("期望报错（多账号且无身份），实际没有")
	}
	for _, want := range []string{"@alice", "@bob", "请用 --user 指定"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want 含 %q", err, want)
		}
	}
}
