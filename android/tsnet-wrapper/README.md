# tsnet-wrapper

> ⚠️ **状态:V2 归档,V3 备用**(2026-07-12 后)
>
> 用户新验收标准简化了手机侧路径:手机装官方 Tailscale + 打开 VPN 就能看到 tailnet 上的电脑和 pane 树。**App 内嵌 tsnet 不再是 V2 的 P0 路径**。
>
> 本 wrapper 完整保留(源码 + AAR + build.sh + dry-run + 三层 netmon fallback),V3 若需要"手机不装官方 Tailscale"路径,从这里直接起手,首日就能装到 App 里。已实测的资产:
> - `tsnet-wrapper-arm64.aar` 7.5 MB(gomobile bind)
> - `cmd/dryrun/main.go` 冷启动 5.51 秒(macOS/含团队代理)
> - `netmon_android.go` 三层 fallback 绕 Go PR #61089 未合 netlink 权限坑
> - `injectstate_android.go` Kotlin ConnectivityManager 桥
>
> 详见 [../../Docs/App/V2-tsnet-简化路径.md](../../Docs/App/V2-tsnet-简化路径.md) §7 降级说明。

---

**Spike 2 产出(V1 阶段,2026-07-12)。V3 备用起手。**

190 行最小 Kotlin/Java-可用 tsnet 包装,gomobile bind 一键出 AAR。已实测:
- **arm64-only AAR:7.5 MB 压缩 / 20.6 MB 未压缩**
- **fat 4 架构 AAR:31.5 MB 压缩 / 86 MB 未压缩**
- Java 桥 classes.jar 仅 11 KB,全部体积在 Go native lib

详细决策 & 背景见 [../../Docs/App/V2-tsnet-简化路径.md](../../Docs/App/V2-tsnet-简化路径.md)。

---

## 导出 API(gomobile 兼容)

**包名**:`minwrap`(Spike 2 起的名,V2 kickoff 决定是否改成 `tsnetwrapper` 等语义化名 —— 改名会让 gomobile 生成的 Kotlin 类名跟着变,build 需重跑验)。gomobile bind 后 Kotlin 侧类名 = 包名首字母大写,即 **`Minwrap`**。

| Go 函数 | Kotlin 对应 | 语义 |
|---|---|---|
| `Start(authKey, stateDir, hostname) error` | `Minwrap.start(...)` | 初始化 tsnet 节点,幂等 |
| `AwaitRunning(timeoutMs int64) error` | `Minwrap.awaitRunning(...)` | 阻塞等节点上线 |
| `Dial(hostPort, timeoutMs) (int64, error)` | `Minwrap.dial(...)` | 打开 TCP 连接,返 handle |
| `Read(connID, buf []byte) *ReadResult` | `Minwrap.read(id, buf)` | 读,`ReadResult{N, EOF, Err}` |
| `Write(connID, buf []byte) (int32, error)` | `Minwrap.write(id, buf)` | 写 |
| `Close(connID) error` | `Minwrap.close(id)` | 关单个连接 |
| `Shutdown()` | `Minwrap.shutdown()` | 关 tsnet + 所有连接 |
| `TailscaleIPs() string` | `Minwrap.tailscaleIPs()` | 返 `"100.x.x.x\|fd7a:..."` |
| `UptimeSeconds() int64` | `Minwrap.uptimeSeconds()` | 运行秒数 |

**未来要加(V2 Phase 1)**:`InjectNetworkState(json string)` —— Kotlin ConnectivityManager 侧同步网卡状态给 tsnet netmon(绕开 Android 11+ netlink 权限坑)。见 [Docs/App/V2-kickoff-首日清单.md](../../Docs/App/V2-kickoff-首日清单.md) Phase 1。

**为什么这个签名**:`net.Conn` / `net.Listener` / `context.Context` 都不是 gomobile bind 可导出类型。这里用 `int64 handle + sync.Map` 的经典桥接模式,Read 用 `*ReadResult` struct 是因为 gomobile 不支持 `(int, error, bool)` 三返回值。

## 构建

```bash
# 依赖
export ANDROID_HOME=~/Library/Android/sdk
export ANDROID_NDK_HOME=~/Library/Android/sdk/ndk/android-ndk-r27c  # 或更新版本
export GOPROXY="https://goproxy.cn,direct"   # 国内网必需
export PATH=~/go/bin:$PATH
go install golang.org/x/mobile/cmd/gomobile@latest
go install golang.org/x/mobile/cmd/gobind@latest
gomobile init

# arm64-only(推荐,dogfood 首版)
gomobile bind \
  -target=android/arm64 \
  -androidapi 24 \
  -ldflags="-s -w" \
  -trimpath \
  -o tsnet-wrapper-arm64.aar .

# fat 全架构(Play 上架用 App Bundle 拆)
gomobile bind \
  -target=android/arm,android/arm64,android/386,android/amd64 \
  -androidapi 24 \
  -ldflags="-s -w" \
  -trimpath \
  -o tsnet-wrapper-fat.aar .
```

**Go 版本要求**:tailscale v1.98.3 要求 Go >= 1.26.3。本机 Go 1.26.1 会触发 `GOTOOLCHAIN` 自动下载 1.26.5(需 GOPROXY 通),或手动升 Go。

**耗时**(参考):
- arm64-only bind ≈ 3:37
- fat 4 架构 bind ≈ 46 秒(gomobile 并行)

## V2 kickoff 时要做

1. **加 06-android-netmon patch 等价逻辑**:不用 patch tailscale 源码,直接在本包里 `import _ "wlynxg/anet"` + `netmon.RegisterInterfaceGetter(...)`(参考 [TailSocks patch 06](https://github.com/bropines/tailsocks/blob/main/appctr/patches/06-android-netmon.patch),96 行)。V2 wrapper 自己注册就行,不 patch tailscale。
2. **Kotlin 侧网络状态注入**:ConnectivityManager NetworkCallback → 序列化 JSON → 调 `injectNetworkState(json string)`(需要加这个导出函数)。
3. **接入 stub 工程**:把出的 aar 放 `android/app/libs/`,`app/build.gradle.kts` 加 `implementation(files("libs/tsnet-wrapper-arm64.aar"))`。
4. **冒烟测试**:真 auth key 起 tsnet → Dial gateway 节点 → 收发一次 → shutdown。量冷/热启动耗时。

## 已知未做

- 反向 `Listen()` 未导出(V2 首版不需要,gateway 主动 Dial 我们)
- state 目录未加密(machine key/node key 敏感,V2 前决定是否包 EncryptedFile)
- 多 tailnet 未支持(单例 `srv`,V2 首版够用)
