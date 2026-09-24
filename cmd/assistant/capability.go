package main

import (
	"fmt"

	"assistant/internal/credentials"
)

// requireAdminIdentity 是管理操作的前置门禁：本地凭据库里记录了该站点的登录身份
// 且明确不是管理员时立即拒绝，并给出两条可行动路径。
//
// 有意为之的两点：
//   - 没有身份记录（没跑过 assistant login，或环境变量单实例模式）时不拦截，
//     交由命令自身的凭据校验（setup.NewAdmin 会在线核对 is_admin）决定；
//   - 传了显式管理员凭据（--admin-token / --oauth / --admin-user）时不拦截：
//     那是调用者主动声明「我用另一个管理员身份」，权威判定仍以服务端为准。
//
// 记录在案的身份只是加速失败的缓存；真正的授权事实永远来自服务端。
func requireAdminIdentity(configPath, host string) error {
	path, err := credentials.Path()
	if err != nil {
		return nil
	}
	store, err := credentials.Load(path)
	if err != nil {
		return nil
	}
	identity, ok := store.IdentityFor(host)
	if !ok || identity.IsAdmin {
		return nil
	}
	return fmt.Errorf(
		"当前登录身份 @%s 不是 %s 的实例管理员：该操作需要管理员权限。\n"+
			"  用管理员账号重新登录：assistant login %s --user <管理员账号>\n"+
			"  或显式提供管理员凭据：--admin-token / --admin-user / --oauth\n"+
			"  查看本地身份：assistant auth status %s",
		identity.User, host, host, host)
}
