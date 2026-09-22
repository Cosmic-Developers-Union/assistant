package envref

import (
	"errors"
	"strings"
	"testing"
)

func env(pairs ...string) Lookup {
	values := map[string]string{}
	for index := 0; index+1 < len(pairs); index += 2 {
		values[pairs[index]] = pairs[index+1]
	}
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

// 展开语义全表：裸/花括号引用、缺省值、嵌套缺省、字面量 "$"、未定义报错。
func TestExpandSemantics(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		lookup  Lookup
		want    string
		wantErr string // 含子串即算命中；空串表示应成功
	}{
		{name: "裸引用", value: "$QQ_TOKEN", lookup: env("QQ_TOKEN", "sec"),
			want: "sec"},
		{name: "花括号引用", value: "${QQ_TOKEN}", lookup: env("QQ_TOKEN", "sec"),
			want: "sec"},
		{name: "前缀后缀拼接", value: "sk-${PROVIDER}-key", lookup: env("PROVIDER", "mm"),
			want: "sk-mm-key"},
		{name: "相邻两个引用", value: "$A$B", lookup: env("A", "1", "B", "2"),
			want: "12"},
		{name: "缺省值生效", value: "${MISSING:-fallback}", lookup: env(),
			want: "fallback"},
		{name: "显式空缺省", value: "${MISSING:-}", lookup: env(),
			want: ""},
		{name: "已定义空值取缺省", value: "${EMPTY:-fallback}", lookup: env("EMPTY", ""),
			want: "fallback"},
		{name: "缺省段嵌套引用", value: "${MISSING:-$HOME/x}", lookup: env("HOME", "/u"),
			want: "/u/x"},
		{name: "缺省段嵌套未定义", value: "${MISSING:-$ALSO_MISSING}", lookup: env(),
			wantErr: "ALSO_MISSING"},
		{name: "未定义裸引用", value: "$QQ_TOKEN", lookup: env(),
			wantErr: "未定义的环境变量 QQ_TOKEN"},
		{name: "未定义花括号引用", value: "${QQ_TOKEN}", lookup: env(),
			wantErr: "未定义的环境变量 QQ_TOKEN"},
		{name: "孤立美元号保留", value: "price$ 100", lookup: env(),
			want: "price$ 100"},
		{name: "美元号接数字保留", value: "sk-$5x", lookup: env(),
			want: "sk-$5x"},
		{name: "多个美元号", value: "$$$", lookup: env(),
			want: "$$$"},
		{name: "未闭合引用", value: "${VAR", lookup: env("VAR", "x"),
			wantErr: "未闭合"},
		{name: "空变量名", value: "${}", lookup: env(),
			wantErr: "非法变量名"},
		{name: "数字开头变量名", value: "${1VAR}", lookup: env(),
			wantErr: "非法变量名"},
		{name: "无引用原样", value: "sk-plain-key", lookup: env(),
			want: "sk-plain-key"},
		{name: "空串", value: "", lookup: env(),
			want: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			options := Options{Lookup: testCase.lookup, Field: "channels[qq].app_secret"}
			got, err := Expand(testCase.value, options)
			if testCase.wantErr != "" {
				if err == nil {
					t.Fatalf("应报错，got %q", got)
				}
				if !strings.Contains(err.Error(), testCase.wantErr) {
					t.Errorf("错误信息应含 %q：%v", testCase.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if got != testCase.want {
				t.Errorf("Expand = %q, want %q", got, testCase.want)
			}
		})
	}
}

// 未定义变量的错误信息带字段路径与 .env 提示，可定位到具体配置项。
func TestUndefinedErrorNamesField(t *testing.T) {
	_, err := Expand("$QQ_TOKEN", Options{Field: "channels[qq/support].app_secret"})
	var undefined *UndefinedError
	if !errors.As(err, &undefined) {
		t.Fatalf("应返回 *UndefinedError，got %T：%v", err, err)
	}
	if undefined.Name != "QQ_TOKEN" || undefined.Field != "channels[qq/support].app_secret" {
		t.Errorf("错误字段：%+v", undefined)
	}
	for _, want := range []string{"channels[qq/support].app_secret", "QQ_TOKEN", ".env"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q：%v", want, err)
		}
	}
}

// 语法错误类型可识别。
func TestSyntaxErrorType(t *testing.T) {
	_, err := Expand("${VAR", Options{})
	var syntax *SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatalf("应返回 *SyntaxError，got %T：%v", err, err)
	}
}

// Validate 与 Expand 同判定：合法通过、未定义报错。
func TestValidate(t *testing.T) {
	lookup := env("SET", "x")
	if err := Validate("$SET", Options{Lookup: lookup}); err != nil {
		t.Errorf("已定义引用不应报错：%v", err)
	}
	if err := Validate("$UNSET", Options{Lookup: lookup}); err == nil {
		t.Error("未定义引用应报错")
	}
}

// ExpandEnvMap：值展开、键不动、入参不被修改、错误路径标注 env[KEY]。
func TestExpandEnvMap(t *testing.T) {
	original := map[string]string{"ANTHROPIC_BASE_URL": "$HOST/v1", "KEEP": "plain"}
	expanded, err := ExpandEnvMap(original, Options{Lookup: env("HOST", "https://api"), Field: "providers.mm"})
	if err != nil {
		t.Fatal(err)
	}
	if expanded["ANTHROPIC_BASE_URL"] != "https://api/v1" || expanded["KEEP"] != "plain" {
		t.Errorf("expanded = %v", expanded)
	}
	if original["ANTHROPIC_BASE_URL"] != "$HOST/v1" {
		t.Errorf("入参被修改：%v", original)
	}
	if len(expanded) != len(original) {
		t.Errorf("长度变化：%d vs %d", len(expanded), len(original))
	}

	_, err = ExpandEnvMap(map[string]string{"K": "$MISSING"}, Options{Lookup: env(), Field: "providers.mm"})
	if err == nil || !strings.Contains(err.Error(), "providers.mm.env[K]") {
		t.Errorf("错误应标注 env[K] 字段路径：%v", err)
	}

	if got, err := ExpandEnvMap(nil, Options{}); got != nil || err != nil {
		t.Errorf("nil 入 map 应 nil 出：%v %v", got, err)
	}
}

// ExpandMCPEnv：只动 "env" 键下的字符串值，其余结构与值原样深拷贝。
func TestExpandMCPEnv(t *testing.T) {
	document := map[string]any{
		"gitea": map[string]any{
			"command": "assistant",
			"args":    []any{"mcp", "gitea", "--host", "$HOST"},
			"env":     map[string]any{"GITEA_ACCESS_TOKEN": "$TOKEN", "COUNT": 3},
		},
		"docs": map[string]any{
			"command": "uvx",
			"env":     map[string]any{"NO_REFS": "keep"},
		},
	}
	expanded, err := ExpandMCPEnv(document, Options{Lookup: env("TOKEN", "tok-1"), Field: "agents.support.mcp"})
	if err != nil {
		t.Fatal(err)
	}
	server := expanded.(map[string]any)["gitea"].(map[string]any)
	if got := server["env"].(map[string]any)["GITEA_ACCESS_TOKEN"]; got != "tok-1" {
		t.Errorf("env 值未展开：%v", got)
	}
	if got := server["env"].(map[string]any)["COUNT"]; got != 3 {
		t.Errorf("非字符串 env 值应原样：%v", got)
	}
	args := server["args"].([]any)
	if args[3] != "$HOST" {
		t.Errorf("args 里的字符串不应展开：%v", args)
	}
	if document["gitea"].(map[string]any)["env"].(map[string]any)["GITEA_ACCESS_TOKEN"] != "$TOKEN" {
		t.Errorf("入参被修改：%v", document)
	}

	// 未定义变量：错误路径一路标注到 server 名
	_, err = ExpandMCPEnv(document, Options{Lookup: env(), Field: "agents.support.mcp"})
	if err == nil || !strings.Contains(err.Error(), "agents.support.mcp.gitea.env.GITEA_ACCESS_TOKEN") {
		t.Errorf("错误应标注完整字段路径：%v", err)
	}
}

// 深层嵌套（列表里套 map）也能遍历到 env。
func TestExpandMCPEnvNested(t *testing.T) {
	document := []any{
		map[string]any{"env": map[string]any{"KEY": "$V"}},
		"plain",
	}
	expanded, err := ExpandMCPEnv(document, Options{Lookup: env("V", "val"), Field: "mcp"})
	if err != nil {
		t.Fatal(err)
	}
	list := expanded.([]any)
	if got := list[0].(map[string]any)["env"].(map[string]any)["KEY"]; got != "val" {
		t.Errorf("嵌套 env 未展开：%v", got)
	}
	if list[1] != "plain" {
		t.Errorf("普通元素被改动：%v", list[1])
	}
}

// 确定性：多键未定义时报错稳定（排序遍历，第一个键）。
func TestExpandMCPEnvDeterministicOrder(t *testing.T) {
	document := map[string]any{"env": map[string]any{"A_KEY": "$MISSING_A", "B_KEY": "$MISSING_B"}}
	for range 10 {
		_, err := ExpandMCPEnv(document, Options{Lookup: env(), Field: "mcp.x"})
		if err == nil || !strings.Contains(err.Error(), "A_KEY") {
			t.Fatalf("报错应稳定命中排序首个键：%v", err)
		}
	}
}
