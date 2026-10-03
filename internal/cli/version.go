package cli

import "runtime/debug"

// binaryVersion 复用构建时注入的版本；直接 go build 时使用 Go 记录的源码提交。
// 版本只来自当前二进制，不读取用户项目的 Git 版本或运行配置。
func binaryVersion() string {
	if version != "" && version != "dev" {
		return version
	}
	info, _ := debug.ReadBuildInfo()
	return sourceVersion(info)
}

func sourceVersion(info *debug.BuildInfo) string {
	if info != nil {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return "sha-" + setting.Value
			}
		}
	}
	return "dev"
}
