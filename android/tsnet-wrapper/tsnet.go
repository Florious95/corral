// Minimal tsnet wrapper for V2 Android AAR size estimation.
// Exposes just: Start / Dial / Close, plus a conn handle table for gomobile bind.
package minwrap

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	_ "golang.org/x/mobile/bind"
	"tailscale.com/tsnet"
)

var (
	srv       *tsnet.Server
	mu        sync.Mutex
	conns     sync.Map
	nextConnID int64
	startTime time.Time
)

// Start initializes tsnet with the given auth key and state dir. Idempotent.
func Start(authKey, stateDir, hostname string) error {
	mu.Lock()
	defer mu.Unlock()
	if srv != nil {
		return nil
	}
	s := &tsnet.Server{
		AuthKey:  authKey,
		Dir:      stateDir,
		Hostname: hostname,
		Ephemeral: false,
	}
	if err := s.Start(); err != nil {
		return err
	}
	srv = s
	startTime = time.Now()
	return nil
}

// AwaitRunning blocks until the node is authenticated and running or ctx times out.
func AwaitRunning(timeoutMillis int64) error {
	mu.Lock()
	s := srv
	mu.Unlock()
	if s == nil {
		return fmt.Errorf("tsnet not started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMillis)*time.Millisecond)
	defer cancel()
	_, err := s.Up(ctx)
	return err
}

// Dial opens a TCP connection to hostPort (e.g. "gateway:1234" or "100.64.0.1:1234").
// Returns a handle id usable with Read/Write/Close.
func Dial(hostPort string, timeoutMillis int64) (int64, error) {
	mu.Lock()
	s := srv
	mu.Unlock()
	if s == nil {
		return 0, fmt.Errorf("tsnet not started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMillis)*time.Millisecond)
	defer cancel()
	c, err := s.Dial(ctx, "tcp", hostPort)
	if err != nil {
		return 0, err
	}
	id := atomic.AddInt64(&nextConnID, 1)
	conns.Store(id, c)
	return id, nil
}

// Read reads up to len(buf) bytes into buf. Returns bytes read or error string.
// gomobile can pass []byte but the return convention favors a struct.
type ReadResult struct {
	N   int32
	EOF bool
	Err string
}

func Read(connID int64, buf []byte) *ReadResult {
	v, ok := conns.Load(connID)
	if !ok {
		return &ReadResult{Err: "unknown connID"}
	}
	c := v.(net.Conn)
	n, err := c.Read(buf)
	r := &ReadResult{N: int32(n)}
	if err == io.EOF {
		r.EOF = true
	} else if err != nil {
		r.Err = err.Error()
	}
	return r
}

func Write(connID int64, buf []byte) (int32, error) {
	v, ok := conns.Load(connID)
	if !ok {
		return 0, fmt.Errorf("unknown connID")
	}
	c := v.(net.Conn)
	n, err := c.Write(buf)
	return int32(n), err
}

func Close(connID int64) error {
	v, ok := conns.LoadAndDelete(connID)
	if !ok {
		return nil
	}
	return v.(net.Conn).Close()
}

// Shutdown closes tsnet and all conns.
func Shutdown() {
	mu.Lock()
	s := srv
	srv = nil
	mu.Unlock()
	conns.Range(func(k, v interface{}) bool {
		v.(net.Conn).Close()
		conns.Delete(k)
		return true
	})
	if s != nil {
		s.Close()
	}
}

// TailscaleIPs returns the node's IPv4 and IPv6 addresses as "ip4|ip6".
func TailscaleIPs() string {
	mu.Lock()
	s := srv
	mu.Unlock()
	if s == nil {
		return ""
	}
	ip4, ip6 := s.TailscaleIPs()
	return ip4.String() + "|" + ip6.String()
}

// UptimeSeconds returns seconds since Start() succeeded.
func UptimeSeconds() int64 {
	mu.Lock()
	defer mu.Unlock()
	if srv == nil {
		return 0
	}
	return int64(time.Since(startTime).Seconds())
}
