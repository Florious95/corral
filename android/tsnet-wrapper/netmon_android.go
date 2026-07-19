//go:build android

// netmon_android.go — Android-only interface getter for tsnet.
//
// 背景:tsnet.Server.Start() → netmon → net.Interfaces() 在 Android 11+
// 触发 `netlinkrib: permission denied` (SELinux 禁 NETLINK bind + RTM_GETLINK)。
// Go 官方 PR #61089 挂三年未合。参考 TailSocks patch 06。
//
// 三层 fallback:
//   1. Kotlin 侧通过 InjectNetworkState(json) 注入的最新网卡状态
//   2. wlynxg/anet 库(用户态解析 /proc/net,不需要 netlink 权限)
//   3. 返回空列表(优雅退化,netcheck 不会崩,只是 magicsock 优化受限)
//
// 注意:build 时 `//go:build android` 保证只对 android 目标启用;
// gomobile bind android/arm64 会启用本文件。

package minwrap

import (
	"encoding/json"
	"log/slog"
	"net"

	"github.com/wlynxg/anet"
	"tailscale.com/net/netmon"
)

// injectedNetworkStateJSON 由 InjectNetworkState 写入,由本文件 init 时注册的
// InterfaceGetter 读取。跨文件共享,靠 stateMu(见 injectstate_android.go)保护。
//
// JSON 格式(Kotlin 侧承诺):
//   [
//     {"name": "wlan0", "addresses": ["192.168.1.42", "fe80::..."], "up": true, "mtu": 1500},
//     ...
//   ]

type injectedInterface struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses"`
	Up        bool     `json:"up"`
	MTU       int      `json:"mtu"`
}

func init() {
	netmon.RegisterInterfaceGetter(func() ([]netmon.Interface, error) {
		// 1. 优先读 Kotlin 侧注入的 state
		stateMu.Lock()
		state := injectedNetworkStateJSON
		stateMu.Unlock()

		if state != "" {
			var list []injectedInterface
			if err := json.Unmarshal([]byte(state), &list); err == nil && len(list) > 0 {
				ret := make([]netmon.Interface, 0, len(list))
				for _, iface := range list {
					if !iface.Up {
						continue
					}
					ni := netmon.Interface{
						Interface: &net.Interface{
							Name:  iface.Name,
							MTU:   iface.MTU,
							Flags: net.FlagUp,
						},
					}
					for _, addr := range iface.Addresses {
						ip := net.ParseIP(addr)
						if ip == nil {
							continue
						}
						mask := net.CIDRMask(32, 32)
						if ip.To4() == nil {
							mask = net.CIDRMask(128, 128)
						}
						ni.AltAddrs = append(ni.AltAddrs, &net.IPNet{IP: ip, Mask: mask})
					}
					if len(ni.AltAddrs) > 0 {
						ret = append(ret, ni)
					}
				}
				if len(ret) > 0 {
					return ret, nil
				}
			}
		}

		// 2. 回退到 wlynxg/anet(不需 netlink 权限)
		ifs, err := anet.Interfaces()
		if err != nil {
			// 3. 优雅退化,不 error out —— netcheck 能凑合活着
			slog.Warn("netmon: anet.Interfaces failed, returning empty", "err", err)
			return []netmon.Interface{}, nil
		}

		ret := make([]netmon.Interface, len(ifs))
		for i := range ifs {
			addrs, err := anet.InterfaceAddrsByInterface(&ifs[i])
			if err != nil {
				ret[i] = netmon.Interface{Interface: &ifs[i]}
				continue
			}
			ret[i] = netmon.Interface{
				Interface: &ifs[i],
				AltAddrs:  addrs,
			}
		}
		return ret, nil
	})
}
