package repoinstall

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"
)

//go:embed templates/*.tmpl
var templateFiles embed.FS

// TemplateData 是渲染生成文件的绑定数据：身份是约定（ai/merge），镜像可覆盖。
type TemplateData struct {
	// Reviewer 是内容评审者账号名（约定 ai）。
	Reviewer string
	// Merger 是状态评审者/合并者账号名（约定 merge）。
	Merger string
	// Image 是仓库 workflow 运行的 assistant 容器镜像。
	Image string
}

// renderTemplate 用 << >> 定界符渲染模板：workflow 里的 Actions 表达式
// ${{ ... }} 与 Go 模板的 {{ }} 冲突，因此不使用默认定界符。
func renderTemplate(name string, data TemplateData) (string, error) {
	content, err := templateFiles.ReadFile("templates/" + name)
	if err != nil {
		return "", fmt.Errorf("读取模板 %s: %w", name, err)
	}
	parsed, err := template.New(name).Delims("<<", ">>").Parse(string(content))
	if err != nil {
		return "", fmt.Errorf("解析模板 %s: %w", name, err)
	}
	var buffer bytes.Buffer
	if err := parsed.Execute(&buffer, data); err != nil {
		return "", fmt.Errorf("渲染模板 %s: %w", name, err)
	}
	return buffer.String(), nil
}
