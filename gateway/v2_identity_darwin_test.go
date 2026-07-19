//go:build darwin

package main

import (
	"os"
	"testing"
	"time"
)

func TestDarwinProcessStartUsesKernelMicrosecondTimestamp(t *testing.T) {
	started, err := v2ProcessStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if started.Sec <= 0 || started.Usec < 0 || started.Usec >= 1_000_000 {
		t.Fatalf("invalid kernel process start: %#v", started)
	}
	value := time.Unix(started.Sec, started.Usec*1000)
	if value.After(time.Now()) || time.Since(value) > time.Hour {
		t.Fatalf("unexpected current test process start: %s", value)
	}
}
