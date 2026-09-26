package provider

import (
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
)

// aliasHandlerStub 是实现了 Aliases 的测试 handler：aliasesOf 的别名分支
// 只有靠它才能走到（生产 handler 均未实现该可选接口）。
type aliasHandlerStub struct {
	name    string
	aliases []string
	err     error
	seen    *Session
}

func (s aliasHandlerStub) Name() string { return s.name }

func (s aliasHandlerStub) Aliases() []string { return s.aliases }

func (s aliasHandlerStub) Prepare(session *Session) error {
	if s.seen != nil {
		*s.seen = *session
	}
	return s.err
}

// plainHandlerStub 不实现 Aliases：aliasesOf 应返回 nil。
type plainHandlerStub struct {
	name string
}

func (s plainHandlerStub) Name() string           { return s.name }
func (s plainHandlerStub) Prepare(*Session) error { return nil }

// aliasesOf 只对实现了 aliasHandler 的 handler 返回别名。
func TestAliasesOf(t *testing.T) {
	withAliases := aliasHandlerStub{name: "Main", aliases: []string{"Alias1", "Alias2"}}
	if got := aliasesOf(withAliases); len(got) != 2 || got[0] != "Alias1" {
		t.Errorf("aliasesOf(有别名) = %v, want [Alias1 Alias2]", got)
	}

	if got := aliasesOf(plainHandlerStub{name: "Plain"}); got != nil {
		t.Errorf("aliasesOf(无别名) = %v, want nil", got)
	}
}

// Register/RegisterPreset 的重名、空名与 nil 都是装配期错误，必须 panic
// （在程序启动前暴露，而不是运行时静默用错 handler）。
func TestRegisterPanicsOnInvalidHandlers(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler Handler
	}{
		{name: "nil handler", handler: nil},
		{name: "空名字", handler: plainHandlerStub{name: "   "}},
		// 别名解析后为空（规范化后为 ""）同样要拒绝
		{name: "别名全为空白的名字", handler: aliasHandlerStub{name: "HasBlankAlias", aliases: []string{"  "}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%s) 未 panic，want 装配期错误", test.name)
				}
			}()
			Register(test.handler)
		})
	}
}

// 注册后主名与别名都能查到（不区分大小写、忽略首尾空白）。
func TestRegisterIndexesAliases(t *testing.T) {
	handler := aliasHandlerStub{name: "UniqueMainForAliasTest", aliases: []string{"UniqueAliasForTest"}}
	Register(handler)

	for _, name := range []string{
		"UniqueMainForAliasTest",
		"uniquemainforaliastest",
		"  UniqueMainForAliasTest  ",
		"UniqueAliasForTest",
		"uniquealiasfortest",
	} {
		got, ok := Lookup(name)
		if !ok {
			t.Errorf("Lookup(%q) 未命中（别名也应可查）", name)
			continue
		}
		if got.Name() != "UniqueMainForAliasTest" {
			t.Errorf("Lookup(%q).Name() = %q", name, got.Name())
		}
	}

	if _, ok := Lookup("UniqueNotRegisteredForTest"); ok {
		t.Error("未注册的名字不应命中")
	}
}

// 重名注册 panic：主名或别名撞上已注册的名字都要在装配期报错。
func TestRegisterPanicsOnDuplicate(t *testing.T) {
	Register(plainHandlerStub{name: "UniqueDuplicateBase"})

	for _, test := range []struct {
		name    string
		handler Handler
	}{
		{name: "同名", handler: plainHandlerStub{name: "UniqueDuplicateBase"}},
		{name: "同名不同大小写", handler: plainHandlerStub{name: "uniqueduplicatebase"}},
		{name: "别名撞主名", handler: aliasHandlerStub{
			name: "UniqueDuplicateOther", aliases: []string{"UniqueDuplicateBase"},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: Register 未 panic，want 重名错误", test.name)
				}
			}()
			Register(test.handler)
		})
	}
}

// cloneAnyMap 是深拷贝：修改副本不影响原 map（会话间隔离），且嵌套结构也要独立。
func TestCloneAnyMapIsDeepCopy(t *testing.T) {
	source := map[string]any{
		"scalar": "a",
		"nested": map[string]any{"inner": "b"},
		"list":   []any{"x", "y"},
	}
	clone := cloneAnyMap(source)

	clone["scalar"] = "changed"
	clone["nested"].(map[string]any)["inner"] = "changed"
	clone["list"].([]any)[0] = "changed"

	if source["scalar"] != "a" {
		t.Errorf("标量被改动：%v", source["scalar"])
	}
	if source["nested"].(map[string]any)["inner"] != "b" {
		t.Errorf("嵌套 map 未深拷贝：%v", source["nested"])
	}
	if source["list"].([]any)[0] != "x" {
		t.Errorf("切片未深拷贝：%v", source["list"])
	}
}

// 空输入返回可写的空 map（而非 nil）：调用方随后直接赋值不应 panic。
func TestCloneAnyMapEmpty(t *testing.T) {
	for _, source := range []map[string]any{nil, {}} {
		clone := cloneAnyMap(source)
		if clone == nil {
			t.Fatalf("cloneAnyMap(%v) = nil, want 可写空 map", source)
		}
		clone["k"] = "v" // nil map 会在此 panic
	}
}

// 无法 JSON 序列化的值（如 chan）退回原 map——宁可不隔离也不能丢配置。
func TestCloneAnyMapFallsBackOnMarshalFailure(t *testing.T) {
	source := map[string]any{"bad": make(chan int)}
	clone := cloneAnyMap(source)
	if _, ok := clone["bad"]; !ok {
		t.Errorf("序列化失败时应退回原 map，得到 %v", clone)
	}
}

// cloneStringMap 是独立的拷贝（改动副本不影响原 map）。
func TestCloneStringMapIsolates(t *testing.T) {
	source := map[string]string{"a": "1"}
	clone := cloneStringMap(source)
	clone["a"] = "2"
	if source["a"] != "1" {
		t.Errorf("原 map 被改动：%v", source)
	}
}

// Apply 对未注册的 provider 原样返回（不报错、不改动 base）：调用方据此
// 把「没有代码级特化」当作正常情形。
func TestApplyUnknownProviderReturnsBase(t *testing.T) {
	base := claudecfg.Overrides{Env: map[string]string{"ANTHROPIC_BASE_URL": "https://x.example"}}
	got, err := Apply("UniqueNotRegisteredForApplyTest", "review", "session-1", base)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got.Env["ANTHROPIC_BASE_URL"] != "https://x.example" {
		t.Errorf("未注册 provider 应原样返回 base：%+v", got.Env)
	}
}

// Apply 会为空的 sessionID 生成新 ID，且不修改传入的 base。
func TestApplyGeneratesSessionIDAndKeepsBase(t *testing.T) {
	base := claudecfg.Overrides{Env: map[string]string{"KEEP": "me"}}
	got, err := Apply("", "review", "", base)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got.Env["KEEP"] != "me" {
		t.Errorf("base 覆盖丢失：%+v", got.Env)
	}
	if _, mutated := base.Env["KEEP"]; !mutated {
		t.Errorf("base 不应被就地修改：%+v", base.Env)
	}
}

// Prepare 返回错误时 Apply 必须把错误带出来（宁可不跑，不带病跑）。
func TestApplyPropagatesPrepareError(t *testing.T) {
	Register(aliasHandlerStub{name: "UniqueFailingPrepare", err: errStub})
	_, err := Apply("UniqueFailingPrepare", "review", "session-1", claudecfg.Overrides{})
	if err == nil {
		t.Fatal("Apply() error = nil, want Prepare 的错误")
	}
}

// errStub 是 provider 测试用的固定错误。
var errStub = stubError("prepare 失败")

type stubError string

func (e stubError) Error() string { return string(e) }
