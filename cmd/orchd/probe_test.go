package main

import (
	"context"
	"net"
	"testing"
	"time"
)

// listener starts a TCP server that runs h for every connection.
func listener(t *testing.T, h func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); h(c) }()
		}
	}()
	return ln.Addr().String()
}

func TestProbeBanner(t *testing.T) {
	short := 300 * time.Millisecond
	ctx := context.Background()
	ok := listener(t, func(c net.Conn) { c.Write([]byte("SSH-2.0-OpenSSH_9.6p1 Ubuntu\r\n")) })
	if err := probeBanner(ctx, ok, short, short); err != nil {
		t.Fatalf("a real banner must pass: %v", err)
	}
	// the exact failure seen live: the port accepts, sshd sends nothing
	silent := listener(t, func(c net.Conn) { time.Sleep(2 * time.Second) })
	start := time.Now()
	if err := probeBanner(ctx, silent, short, short); err == nil {
		t.Fatal("an accepted but silent port must not count as ready")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("the silent probe must give up at the read timeout, took %v", time.Since(start))
	}
	garbage := listener(t, func(c net.Conn) { c.Write([]byte("HTTP/1.1 400 Bad Request\r\n")) })
	if err := probeBanner(ctx, garbage, short, short); err == nil {
		t.Fatal("a non-ssh answer must not count as ready")
	}
	closed := listener(t, func(c net.Conn) {}) // accepts and closes at once
	if err := probeBanner(ctx, closed, short, short); err == nil {
		t.Fatal("an immediately closed connection must not count as ready")
	}
	if err := probeBanner(ctx, "127.0.0.1:1", short, short); err == nil {
		t.Fatal("nothing listening must fail")
	}
	// a banner that arrives late but before the read timeout still passes
	late := listener(t, func(c net.Conn) { time.Sleep(100 * time.Millisecond); c.Write([]byte("SSH-2.0-x\n")) })
	if err := probeBanner(ctx, late, short, short); err != nil {
		t.Fatalf("a banner within the timeout must pass: %v", err)
	}
	// the caller's context bounds the read too
	cctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	start = time.Now()
	if err := probeBanner(cctx, silent, time.Second, 5*time.Second); err == nil || time.Since(start) > time.Second {
		t.Fatalf("ctx deadline must bound the probe (err=%v, took %v)", err, time.Since(start))
	}
}
