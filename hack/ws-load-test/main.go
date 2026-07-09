// Command ws-load-test is the phase-2 item 2.3 acceptance load test: it
// connects N simulated claws (default 100) to a running hub over the claw
// WebSocket, keeps them chatty (heartbeats, streaming chunks, finalized
// messages), and samples the hub's go_goroutines gauge from /metrics to
// verify the worker pool keeps goroutine count stable instead of growing
// without bound.
//
// Usage:
//
//	go run ./hack/ws-load-test -hub http://localhost:8080 -claw-token <token> \
//	    -claws 100 -duration 60s
//
// The program exits non-zero when the goroutine count at the end of the
// run exceeds the first stable sample by more than -max-growth.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

type wsMessage struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

func main() {
	hubURL := flag.String("hub", "http://localhost:8080", "hub base URL")
	clawToken := flag.String("claw-token", os.Getenv("ELASTICCLAW_CLAW_TOKEN"), "claw token (or ELASTICCLAW_CLAW_TOKEN)")
	clawCount := flag.Int("claws", 100, "number of simulated claws")
	duration := flag.Duration("duration", 60*time.Second, "how long to keep the load running")
	sampleEvery := flag.Duration("sample-every", 5*time.Second, "goroutine sampling interval")
	maxGrowth := flag.Float64("max-growth", 150, "maximum allowed goroutine growth over the run (absolute)")
	flag.Parse()

	if *clawToken == "" {
		log.Fatal("missing -claw-token (or ELASTICCLAW_CLAW_TOKEN)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *duration+30*time.Second)
	defer cancel()

	wsURL := strings.Replace(strings.Replace(*hubURL, "https://", "wss://", 1), "http://", "ws://", 1) + "/claw/ws"

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var connected, failed int64
	var mu sync.Mutex

	for i := 0; i < *clawCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := runClaw(ctx, wsURL, *clawToken, i, stop); err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				log.Printf("claw %03d: %v", i, err)
				return
			}
			mu.Lock()
			connected++
			mu.Unlock()
		}(i)
		time.Sleep(20 * time.Millisecond) // gentle ramp-up
	}

	// Sample goroutines while the load runs.
	var baseline, last float64
	baselineSet := false
	deadline := time.Now().Add(*duration)
	for time.Now().Before(deadline) {
		time.Sleep(*sampleEvery)
		g, err := scrapeGoroutines(*hubURL)
		if err != nil {
			log.Printf("sample: %v", err)
			continue
		}
		last = g
		// Let the ramp-up settle before taking the baseline.
		if !baselineSet && time.Now().After(deadline.Add(-*duration+15*time.Second)) {
			baseline = g
			baselineSet = true
		}
		log.Printf("go_goroutines=%d", int(g))
	}

	close(stop)
	wg.Wait()

	fmt.Printf("claws connected=%d failed=%d baseline=%d final=%d\n",
		connected, failed, int(baseline), int(last))
	if connected == 0 {
		log.Fatal("FAIL: no claw ever connected — check -hub and -claw-token")
	}
	if !baselineSet {
		log.Fatal("run too short to take a stable baseline; increase -duration")
	}
	if growth := last - baseline; growth > *maxGrowth {
		log.Fatalf("FAIL: goroutines grew by %d (baseline %d -> final %d), limit %d",
			int(growth), int(baseline), int(last), int(*maxGrowth))
	}
	fmt.Println("PASS: goroutine count stable")
}

func runClaw(ctx context.Context, wsURL, token string, i int, stop <-chan struct{}) error {
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "load test done")
	ready := true
	if err := wsjson.Write(ctx, conn, wsMessage{
		Type: "register",
		Payload: map[string]interface{}{
			"claw_id":       fmt.Sprintf("load-test-claw-%03d", i),
			"name":          fmt.Sprintf("load-%03d", i),
			"template":      "elasticclaw",
			"token":         token,
			"gateway_ready": &ready,
		},
	}); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	var ack wsMessage
	if err := wsjson.Read(ctx, conn, &ack); err != nil || ack.Type != "registered" {
		return fmt.Errorf("registration ack: type=%q err=%v", ack.Type, err)
	}

	// Drain server pushes so the connection does not stall. A read error
	// means the connection died; report it so the claw is not counted as
	// connected for the whole run.
	readErr := make(chan error, 1)
	go func() {
		for {
			var msg wsMessage
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				readErr <- err
				return
			}
		}
	}()

	heartbeat := time.NewTicker(2 * time.Second)
	defer heartbeat.Stop()
	chatter := time.NewTicker(time.Duration(500+rand.Intn(1000)) * time.Millisecond)
	defer chatter.Stop()
	turn := 0
	for {
		select {
		case <-stop:
			return nil
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return fmt.Errorf("read: %w", err)
		case <-heartbeat.C:
			if err := wsjson.Write(ctx, conn, wsMessage{Type: "heartbeat", Payload: map[string]interface{}{
				"gateway_healthy": true, "context_usage": rand.Intn(50),
			}}); err != nil {
				return fmt.Errorf("heartbeat write: %w", err)
			}
		case <-chatter.C:
			// One short streamed turn: a few chunks, then the final message.
			for c := 0; c < 3; c++ {
				if err := wsjson.Write(ctx, conn, wsMessage{Type: "chunk", Payload: map[string]string{
					"content": fmt.Sprintf("chunk %d of turn %d from claw %03d ", c, turn, i),
				}}); err != nil {
					return fmt.Errorf("chunk write: %w", err)
				}
			}
			if err := wsjson.Write(ctx, conn, wsMessage{Type: "message", Payload: map[string]string{
				"content": fmt.Sprintf("turn %d complete from claw %03d", turn, i),
			}}); err != nil {
				return fmt.Errorf("message write: %w", err)
			}
			turn++
		}
	}
}

// metricsClient bounds the /metrics scrape so a stalled hub yields a sample
// failure instead of hanging the load test.
var metricsClient = &http.Client{Timeout: 10 * time.Second}

func scrapeGoroutines(hubURL string) (float64, error) {
	resp, err := metricsClient.Get(strings.TrimRight(hubURL, "/") + "/metrics")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("/metrics status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "go_goroutines ") {
			return strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "go_goroutines")), 64)
		}
	}
	return 0, fmt.Errorf("go_goroutines not found in /metrics (is the hub built with metrics enabled?)")
}
