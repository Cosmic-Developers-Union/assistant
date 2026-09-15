// Package schema 持有随二进制分发的 JSON Schema：config.json 的补全与悬停文档
// （编辑器读配置文件里的 $schema 键），也是配置面的契约——internal/instances 的
// 结构体 tag 与它必须一一对应（见 instances 包的 schema_test.go）。
//
// 权威文件是 schema/config.schema.json（随版本提交）：既给仓库内的示例配置用
// （config.example.json 以相对路径引用它），也由 `assistant config init` 原样写到
// 用户 config.json 旁边——不能只给仓库 URL，私有站点上的编辑器拉不到。
package schema

import _ "embed"

// FileName 是写到用户配置目录旁边时使用的文件名（与 $schema 的默认取值一致）。
const FileName = "config.schema.json"

// Reference 是配置里默认写下的 $schema 取值：相对 config.json 自身位置。
const Reference = "./" + FileName

// Config 是 config.json 的 JSON Schema 内容（与 schema/config.schema.json 同一份）。
//
//go:embed config.schema.json
var Config []byte
