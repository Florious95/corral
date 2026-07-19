// dryrun — pure-Go 冒烟测试 tsnet.Server.Start 代码路径。
//
// 与 minwrap 共享同一 tailscale/tsnet 依赖(靠 replace 或直接 import tsnet),
// 但不走 gomobile bind、不走 Android build tag。
//
// 目标(Phase 3 首要):
//   1. 证明 tsnet.Server.Start() 能起来并打印 login URL
//   2. 量 "Server.Start → login URL 出现" 耗时,作为冷启动下限参考
//   3. 优雅 shutdown
//
// 运行:
//   cd android/tsnet-wrapper/cmd/dryrun
//   go run . -state /tmp/tsnet-dryrun-state
//
// 使用完 rm -rf /tmp/tsnet-dryrun-state 即可清所有 tsnet 状态(machine key 等)。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

func main() {
	stateDir := flag.String("state", "", "state dir (required; use a fresh temp dir per run)")
	hostname := flag.String("hostname", "tsnet-dryrun", "tsnet hostname")
	timeout := flag.Duration("timeout", 20*time.Second, "how long to wait for login URL before giving up")
	flag.Parse()

	if *stateDir == "" {
		log.Fatal("must pass -state <dir>")
	}
	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		log.Fatalf("mkdir state: %v", err)
	}

	// 捕获 tsnet 内部日志:tsnet.Server.Logf 默认打 stderr,我们劫持成 pipe
	// 好扫描 login URL 并计时。
	pr, pw := io.Pipe()
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)

	loginURLCh := make(chan string, 1)
	var scannerWG sync.WaitGroup
	scannerWG.Add(1)
	go func() {
		defer scannerWG.Done()
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Println("tsnet>", line)
			if strings.Contains(line, "https://login.tailscale.com/") ||
				strings.Contains(line, "To authenticate, visit:") {
				select {
				case loginURLCh <- line:
				default:
				}
			}
		}
	}()

	srv := &tsnet.Server{
		Dir:      *stateDir,
		Hostname: *hostname,
		Logf: func(format string, a ...any) {
			fmt.Fprintf(pw, format+"\n", a...)
		},
	}

	fmt.Printf("[dryrun] tsnet.Server.Start ... (state=%s, hostname=%s)\n", *stateDir, *hostname)
	startAt := time.Now()

	startErrCh := make(chan error, 1)
	go func() {
		startErrCh <- srv.Start()
	}()

	// 等三种事件:Start 出错、拿到 login URL、超时或用户 Ctrl-C
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	var (
		loginURL       string
		startFailed    error
		startCompleted bool
	)

	// tsnet.Server.Start 是非阻塞的:很快返回 nil,login flow 在后台 goroutine 跑。
	// 所以我们等两个事件:(a) login URL 打印(冷启动指标),(b) timeout/signal。
	// startErrCh 只用于捕获 Start 立即失败(如 state dir 无权限)。
outer:
	for {
		select {
		case err := <-startErrCh:
			startCompleted = true
			if err != nil {
				startFailed = err
				break outer
			}
			// Start 成功返回,继续等 login URL
		case url := <-loginURLCh:
			loginURL = url
			break outer
		case <-ctx.Done():
			break outer
		case s := <-sig:
			fmt.Printf("[dryrun] got %v, shutting down\n", s)
			break outer
		}
	}

	elapsed := time.Since(startAt)

	fmt.Println()
	fmt.Println("======================================")
	fmt.Println("[dryrun] result")
	fmt.Println("======================================")
	if startFailed != nil {
		fmt.Printf("  Start FAILED: %v\n", startFailed)
	} else if loginURL != "" {
		fmt.Printf("  login URL:    %s\n", loginURL)
		fmt.Printf("  elapsed:      %v (Server.Start → login URL 首次打印)\n", elapsed)
	} else if startCompleted {
		fmt.Printf("  Start completed without login URL — 可能已 authenticated,或超时。elapsed=%v\n", elapsed)
	} else {
		fmt.Printf("  timeout %v 内未见 login URL,Start 也未完成。可能网络问题。elapsed=%v\n", *timeout, elapsed)
	}
	fmt.Println("======================================")

	fmt.Println("[dryrun] srv.Close ...")
	if err := srv.Close(); err != nil {
		fmt.Printf("[dryrun] Close err: %v\n", err)
	}
	pw.Close()
	scannerWG.Wait()

	if startFailed != nil {
		os.Exit(2)
	}
	if loginURL == "" {
		os.Exit(3)
	}
}
