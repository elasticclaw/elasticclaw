package hub

import (
	"testing"

	"nhooyr.io/websocket"
)

// The bridge reconnects its status channel within seconds, so the replacement
// is routinely installed before the dropped channel's read loop wakes up to
// clean up after itself. The cleanup must therefore only clear the pointer it
// owns -- otherwise it discards the live replacement, the hub sends no further
// status_ping, lastStatusAt freezes, and the silent-death watchdog replaces a
// claw whose bridge is still sending healthy heartbeats.
func TestStatusChannelCleanupOnlyClearsItsOwnConnection(t *testing.T) {
	oldConn := &websocket.Conn{}
	newConn := &websocket.Conn{}

	cc := &clawConn{id: "claw", statusConn: newConn}

	// What the dropped connection's read loop does on exit.
	clearStatusConnIfOwned(cc, oldConn)

	if cc.statusConn != newConn {
		t.Fatal("the stale read loop cleared the replacement status channel")
	}

	// And it must still clear its own.
	clearStatusConnIfOwned(cc, newConn)
	if cc.statusConn != nil {
		t.Fatal("a read loop must clear the connection it owns")
	}
}
