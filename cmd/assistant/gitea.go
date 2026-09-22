package main

import (
	"fmt"
	"slices"
	"strings"

	"assistant/internal/instances"
)

// gitea 通道视图 helpers：登记命令族（login/repos/setup/init/doctor）统一在
// channels[type=gitea] 上读写站点与仓库（instances 已废弃，载入即迁移）。

// giteaChannels 可写的 gitea 通道清单（file.Channels 的 gitea 子集）。
func giteaChannels(file *instances.File) []*instances.Channel {
	channels := make([]*instances.Channel, 0, len(file.Channels))
	for index := range file.Channels {
		if file.Channels[index].Type == instances.ChannelGitea {
			channels = append(channels, &file.Channels[index])
		}
	}
	return channels
}

// findGiteaChannel 按站点地址取 gitea 通道（sameHost 语义：忽略尾斜杠与大小写）。
func findGiteaChannel(file *instances.File, host string) (*instances.Channel, bool) {
	for _, channel := range giteaChannels(file) {
		if sameHost(channel.Host, host) {
			return channel, true
		}
	}
	return nil, false
}

// upsertGiteaChannel 取（或新建）站点的 gitea 通道：新建时填 reviewer/merger 缺省。
func upsertGiteaChannel(file *instances.File, host string) *instances.Channel {
	if channel, ok := findGiteaChannel(file, host); ok {
		return channel
	}
	file.AddGiteaChannel(instances.Channel{
		Type: instances.ChannelGitea,
		Host: strings.TrimRight(strings.TrimSpace(host), "/"),
	})
	channel := &file.Channels[len(file.Channels)-1]
	channel.Normalize()
	return channel
}

// sameGiteaHost 判断两个站点地址是否同站（去尾斜杠比较）。
func sameGiteaHost(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}

// selectRepoGitea 选择仓库登记所属的 gitea 通道：--host 指定 > 检出 remote 对应
// 的站点（autoHosts，仅命中配置时生效）> 已登记该仓库的通道（repoName 非空时）>
// 唯一 gitea 通道。歧义或多站点时报可行动错误。
func selectRepoGitea(file *instances.File, hostFlag, repoName string, autoHosts ...string) (*instances.Channel, error) {
	if host := strings.TrimRight(strings.TrimSpace(hostFlag), "/"); host != "" {
		channel, ok := findGiteaChannel(file, host)
		if !ok {
			return nil, fmt.Errorf("平台 %s 不在配置中：先 assistant login %s", host, host)
		}
		return channel, nil
	}
	for _, hint := range autoHosts {
		if channel, ok := findGiteaChannel(file, hint); ok {
			return channel, nil
		}
	}
	if repoName != "" {
		var matches []*instances.Channel
		for _, channel := range giteaChannels(file) {
			if _, ok := channel.FindRepo(repoName); ok {
				matches = append(matches, channel)
			}
		}
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
		default:
			return nil, fmt.Errorf("仓库 %s 登记在多个平台，请用 --host 指定", repoName)
		}
	}
	channels := giteaChannels(file)
	switch len(channels) {
	case 0:
		return nil, fmt.Errorf("配置中没有平台：先 assistant login <host>")
	case 1:
		return channels[0], nil
	default:
		hosts := make([]string, 0, len(channels))
		for _, channel := range channels {
			hosts = append(hosts, channel.Host)
		}
		return nil, fmt.Errorf("配置中有多个平台（%s），请用 --host 指定", strings.Join(hosts, "、"))
	}
}

// giteaHostCount 返回 gitea 通道站点数（空配置判断用）。
func giteaHostCount(file *instances.File) int {
	return len(giteaChannels(file))
}

// sortChannelRepos 按名字排序通道内仓库（展示稳定）。
func sortChannelRepos(channel *instances.Channel) {
	slices.SortFunc(channel.Repos, func(a, b instances.Repo) int { return strings.Compare(a.Name, b.Name) })
}

// saveConfig 校验并落盘配置（登记命令族共享；载入后的配置已含迁移结果，写回
// 即规范形）。
func saveConfig(file *instances.File, path string) error {
	file.Normalize()
	if err := file.Validate(); err != nil {
		return err
	}
	return instances.Save(path, file)
}

// giteaViews 只读视图：gitea 通道 + 遗留 instances（手工构造、未走 Load 的
// 场景）统一折算成 Instance 形态，供 doctor/调度等只读消费方遍历。
func giteaViews(file *instances.File) []instances.Instance {
	views := make([]instances.Instance, 0, len(file.Channels)+len(file.Instances))
	for index := range file.Channels {
		channel := &file.Channels[index]
		if channel.Type == instances.ChannelGitea {
			views = append(views, channelInstance(channel))
		}
	}
	return append(views, file.Instances...)
}

// channelInstance 把 gitea 通道折算成旧 Instance 视图：setup/status 等引擎
// API 仍按 Instance 形态消费站点与仓库，写回路径一律落回通道。
func channelInstance(channel *instances.Channel) instances.Instance {
	return instances.Instance{
		Host:     channel.Host,
		Provider: channel.Provider,
		Reviewer: instances.Account{Name: channel.Reviewer},
		Merger:   instances.Account{Name: channel.Merger},
		Repos:    channel.Repos,
	}
}

// configHasNoPlatform 判断配置是否已没有任何平台与运行内容（login remove 用它
// 决定要不要删掉整个 config.json）。
func configHasNoPlatform(file *instances.File) bool {
	return len(file.Channels) == 0 &&
		len(file.Runtimes) == 0 && len(file.Providers) == 0 && len(file.Agents) == 0 &&
		file.DefaultProvider == "" && file.DefaultRuntime == "" &&
		len(file.Optimizations.Env) == 0 && len(file.Optimizations.Settings) == 0 &&
		len(file.Optimizations.MCP) == 0
}
