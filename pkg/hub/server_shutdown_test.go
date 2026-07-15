package hub

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestRunShutsDownCleanly(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listener unavailable: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close()

	s := &Server{addr: addr, db: db, hubCfg: &types.HubConfig{}}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- s.run(ctx, RunOptions{NoWebUI: true}) }()

	deadline := time.Now().Add(2 * time.Second)
	var conn net.Conn
	for time.Now().Before(deadline) {
		conn, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("server did not start: %v", err)
	}
	conn.Close()
	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run returned %v, want clean exit", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
}
