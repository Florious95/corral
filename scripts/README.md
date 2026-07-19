# Corral helper scripts

## 一、启动服务(两个后台进程)

**终端 A:Gateway**
```bash
cd gateway
go run .
```
监听 `0.0.0.0:8787`。

**终端 B:Web(Vite dev)**
```bash
cd web
npm run dev -- --host 0.0.0.0
```
监听 `0.0.0.0:5173`。

## 二、启动一个 CLI pane（手动）

```bash
# 新开一个终端
tmux new-session -s demo -n claude

# 在 tmux 里运行受支持的 CLI
claude
```

Ctrl+B、D 分离。

## 三、访问

- 电脑浏览器:http://localhost:5173/
- 同 wifi 手机浏览器:http://<电脑IP>:5173/

## 四、验证路径

1. 首页看到"所有节点 · 查看全部 N 个 pane"
2. 点进"所有节点",看到 `demo` session 里的 `claude` window
3. 点右侧 ☆ 收藏 → 返回首页,收藏区出现
4. 从首页点击 claude → 进入对话页
5. 输入"你好,能听到吗?"→ 点发送 → 看 Claude 回复实时流出来

## 五、遇到 pane 消失了

Gateway 会 404;返回首页刷新即可。

## 六、发布已构建的 Web 资源

本机发布：

```bash
./scripts/publish-web-dist.sh
```

远端发布不含任何内置主机或凭据，必须显式传入：

```bash
RC_SSH_HOST=user@example-host \
RC_CREDENTIAL_FILE=/secure/path/password-file \
RC_TARGET_URL=http://example-host:8787 \
./scripts/publish-web-dist.sh
```

## 七、关掉服务

```bash
pkill corral-gateway
# vite 用 ctrl+c
tmux kill-session -t demo
```
