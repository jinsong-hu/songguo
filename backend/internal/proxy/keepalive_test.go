package proxy

import (
	"io"
	"log/slog"
	"net"
	"testing"
)

// enableKeepAlive runs on every hijacked WebSocket connection, so it must be
// safe on whatever the hijack hands back — including connection types it cannot
// do anything with. A panic here would take down a live relay.
func TestEnableKeepAliveIsSafeOnAnyConn(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("plain TCP is configured", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()

		done := make(chan net.Conn, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				done <- nil
				return
			}
			done <- c
		}()

		client, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer client.Close()

		server := <-done
		if server == nil {
			t.Fatal("accept failed")
		}
		defer server.Close()

		enableKeepAlive(server, logger) // must not panic, must not error out
	})

	t.Run("non-TCP conn is a no-op", func(t *testing.T) {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		// net.Pipe is not a TCPConn and has no keepalive to set. The old
		// behaviour (nothing) is correct here; what matters is that it does not
		// panic on the type assertion.
		enableKeepAlive(a, logger)
	})

	t.Run("nil conn is a no-op", func(t *testing.T) {
		var c net.Conn
		enableKeepAlive(c, logger)
	})
}
