package pool

import (
	"net"
	"sync/atomic"
	"testing"

	N "github.com/sagernet/sing/common/network"
)

type readHalfOnlyConn struct{ net.Conn }

func (c *readHalfOnlyConn) CloseRead() error { return nil }

type writeHalfOnlyConn struct{ net.Conn }

func (c *writeHalfOnlyConn) CloseWrite() error { return nil }

func TestTrackedConnAdvertisesOnlyUnderlyingHalfClose(t *testing.T) {
	for _, mode := range []string{"none", "read", "write", "both"} {
		t.Run(mode, func(t *testing.T) {
			left, right := net.Pipe()
			defer right.Close()
			var raw net.Conn = left
			switch mode {
			case "read":
				raw = &readHalfOnlyConn{Conn: left}
			case "write":
				raw = &writeHalfOnlyConn{Conn: left}
			case "both":
				raw = &halfCloseConn{Conn: left}
			}
			var released atomic.Int32
			conn := (&trackedConn{Conn: raw, release: func() { released.Add(1) }}).withHalfClose()
			defer conn.Close()
			_, hasRead := conn.(N.ReadCloser)
			writer, hasWrite := conn.(N.WriteCloser)
			if hasRead != (mode == "read" || mode == "both") || hasWrite != (mode == "write" || mode == "both") {
				t.Fatalf("mode=%s read=%v write=%v", mode, hasRead, hasWrite)
			}
			// Match sing-box's EOF path: unsupported half-close closes the tracked
			// stream, whereas a real half-close retains its active accounting.
			if hasWrite {
				if err := writer.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				if released.Load() != 0 {
					t.Fatal("half-close released stream")
				}
			} else if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			conn.Close()
			if released.Load() != 1 {
				t.Fatalf("release count=%d", released.Load())
			}
		})
	}
}
