package instances

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"assistant/schema"
)

// schemaDefinition 是 schema 里的一段定义（只取校验需要的部分）。
type schemaDefinition struct {
	Properties           map[string]json.RawMessage `json:"properties"`
	AdditionalProperties *bool                      `json:"additionalProperties"`
}

func (d schemaDefinition) propertyNames() []string {
	names := make([]string, 0, len(d.Properties))
	for name := range d.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// configSchema 解析随二进制分发的 schema（config.json 的补全与文档来源）。
func configSchema(t *testing.T) (schemaDefinition, map[string]schemaDefinition) {
	t.Helper()
	var document struct {
		Schema           string                      `json:"$schema"`
		ID               string                      `json:"$id"`
		schemaDefinition                             // 内嵌：properties/additionalProperties
		Defs             map[string]schemaDefinition `json:"$defs"`
	}
	if err := json.Unmarshal(schema.Config, &document); err != nil {
		t.Fatalf("schema 不是合法 JSON：%v", err)
	}
	if !strings.Contains(document.Schema, "json-schema.org") || document.ID == "" {
		t.Fatalf("schema 缺少 $schema/$id：%q %q", document.Schema, document.ID)
	}
	if len(document.Defs) == 0 {
		t.Fatal("schema 缺少 $defs")
	}
	return document.schemaDefinition, document.Defs
}

// jsonTagNames 收集结构体导出字段的 JSON 键名（不含 omitempty 等选项）。
func jsonTagNames(value any) []string {
	typ := reflect.TypeOf(value)
	names := make([]string, 0, typ.NumField())
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		if name := strings.Split(tag, ",")[0]; name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// schema 与结构体必须一一对应：谁先改，测试都会拦下来。schema 是配置面的契约
// （编辑器补全 + 悬停文档），结构体 tag 是加载器的契约（DisallowUnknownFields），
// 两者漂移的代价是「编辑器说没问题、加载器却拒绝」。
func TestSchemaMatchesConfigStructs(t *testing.T) {
	root, defs := configSchema(t)
	cases := []struct {
		name string
		from []string
		want []string
	}{
		{"config.json", root.propertyNames(), jsonTagNames(File{})},
		{"$defs.instance", defs["instance"].propertyNames(), jsonTagNames(Instance{})},
		{"$defs.repoObject", defs["repoObject"].propertyNames(), jsonTagNames(Repo{})},
		{"$defs.account", defs["account"].propertyNames(), jsonTagNames(Account{})},
		{"$defs.provider", defs["provider"].propertyNames(), jsonTagNames(Provider{})},
		{"$defs.weixin", defs["weixin"].propertyNames(), jsonTagNames(Weixin{})},
		{"$defs.qq", defs["qq"].propertyNames(), jsonTagNames(QQ{})},
		{"$defs.agent", defs["agent"].propertyNames(), jsonTagNames(Agent{})},
	}
	for _, testCase := range cases {
		from := strings.Join(testCase.from, ",")
		want := strings.Join(testCase.want, ",")
		if from != want {
			t.Errorf("%s 与结构体不一致：\nschema: %s\n结构体: %s", testCase.name, from, want)
		}
	}

	// additionalProperties 必须与加载器的严格程度一致：provider 之外的未知键都会
	// 被 DisallowUnknownFields 拒绝；provider 内部的未识别键则原样保留。
	for _, name := range []string{"instance", "repoObject", "account", "weixin", "qq", "agent"} {
		if additional := defs[name].AdditionalProperties; additional == nil || *additional {
			t.Errorf("$defs.%s 必须 additionalProperties: false", name)
		}
	}
	if additional := defs["provider"].AdditionalProperties; additional == nil || !*additional {
		t.Error("$defs.provider 必须 additionalProperties: true（未识别键原样保留）")
	}
	if additional := root.AdditionalProperties; additional == nil || *additional {
		t.Error("config.json 顶层必须 additionalProperties: false")
	}

	// 仓库条目支持 "owner/name" 简写：结构体靠自定义 UnmarshalJSON，schema 靠 oneOf。
	for name, definition := range defs {
		if name != "repo" {
			continue
		}
		if len(definition.Properties) != 0 {
			t.Errorf("$defs.repo 应该是 oneOf（简写或对象），不该直接写 properties")
		}
	}
}
