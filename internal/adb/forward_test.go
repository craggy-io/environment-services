package adb_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/internal/adb"
)

func TestForwardAllocatedPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		req, err := readReq(conn)
		if err != nil {
			done <- err
			return
		}
		if req != "host-serial:emulator-test:forward:tcp:0;tcp:27042" {
			done <- fmt.Errorf("request = %q", req)
			return
		}
		_, err = fmt.Fprint(conn, "OKAYOKAY000532123")
		done <- err
	}()
	c := &adb.Client{Addr: listener.Addr().String(), Timeout: time.Second}
	port, err := c.Forward(context.Background(), "emulator-test", 0, 27042)
	if err != nil || port != 32123 {
		t.Fatalf("port=%d err=%v", port, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
