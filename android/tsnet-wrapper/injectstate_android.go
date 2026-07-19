//go:build android

// injectstate_android.go — Kotlin 侧网络状态注入桥。
//
// gomobile bind 导出:InjectNetworkState(jsonState string)
//
// Kotlin 侧调用示例:
//   val json = JSONArray().apply {
//       put(JSONObject().apply {
//           put("name", "wlan0")
//           put("addresses", JSONArray(listOf("192.168.1.42")))
//           put("up", true)
//           put("mtu", 1500)
//       })
//   }.toString()
//   Minwrap.injectNetworkState(json)
//
// 什么时候调:
//   1. TsnetService.onStartCommand,注册 ConnectivityManager.NetworkCallback
//   2. onAvailable / onLost / onCapabilitiesChanged / onLinkPropertiesChanged 时
//      重新组织当前所有网卡状态,序列化 JSON,调本函数
//   3. Server.Start() 前先注入一次(防止 tsnet 初始扫描时 netmon 为空)

package minwrap

import "sync"

var (
	stateMu                 sync.Mutex
	injectedNetworkStateJSON string
)

// InjectNetworkState 由 Kotlin 侧调用,注入当前网卡状态 JSON。
// JSON 格式见 netmon_android.go 中的 injectedInterface。
// 空字符串表示清空注入 state(回退到 anet fallback)。
//
// 为什么用 string 而非 []byte:
//   - Kotlin 侧 org.json.JSONObject.toString() / kotlinx.serialization 天生输出 String
//   - gomobile bind 生成的 Java 签名 String → 无需额外 encoding
//   - []byte 会在 Kotlin 侧多一次 toByteArray(Charsets.UTF_8),无收益
func InjectNetworkState(jsonState string) {
	stateMu.Lock()
	injectedNetworkStateJSON = jsonState
	stateMu.Unlock()
}
