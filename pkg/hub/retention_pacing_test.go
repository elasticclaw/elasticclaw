package hub

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// ---------------------------------------------------------------------------
// Pacing: every bulk write loop yields the write lock and honours the budget
// ---------------------------------------------------------------------------

// pacedRetentionPhases enumerates every phase that begins write transactions in
// bulk. Each row seeds more work than one write-lock hold covers, runs a full
// cycle, and reports how much of that work is done. The test below asserts
// that a phase with a budget finishes the work and pauses between holds, and
// that a phase whose budget is already spent stops after exactly one hold.
//
// TestEveryBulkWriteLoopIsPaced ties this table to the source: every function
// that constructs a retentionPacer must be named here, so a new paced phase
// cannot be added without a row, and a loop that writes without a pacer is
// rejected outright.
type pacedRetentionPhase struct {
	// name is the pacer's phase name, which is what its budget line says.
	name string
	// pacerBuiltIn is the function in retention.go that constructs the pacer.
	pacerBuiltIn string
	// total is how many items seed creates; firstHold is how many one
	// write-lock hold covers, which is all a spent budget allows.
	total, firstHold int
	seed             func(t *testing.T, s *Server, now time.Time)
	done             func(t *testing.T, s *Server) int
}

func countRetentionRows(t *testing.T, s *Server, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func pacedRetentionPhases() []pacedRetentionPhase {
	const checkpoints = 2*retentionCheckpointBatch + 3
	// perClaw is more than one hold covers, so a budget spent on the first claw
	// leaves the second untouched only if the pacer is shared.
	const perClaw = retentionCheckpointBatch + 2
	const rows = 2*retentionDeleteBatch + 7
	tree := strings.Repeat("ab", 32)
	return []pacedRetentionPhase{
		{
			name: "blob reference backfill", pacerBuiltIn: "backfillCheckpointBlobRefs",
			total: checkpoints, firstHold: retentionCheckpointBatch,
			seed: func(t *testing.T, s *Server, now time.Time) {
				if err := os.MkdirAll(filepath.Join(checkpointsRoot(), "blobs", "sha256"), 0o750); err != nil {
					t.Fatal(err)
				}
				insertRetentionClaw(t, s, "claw", now)
				for i := 0; i < checkpoints; i++ {
					insertRetentionCheckpoint(t, s, retentionCheckpoint{
						id: fmt.Sprintf("cp-%d", i), clawID: "claw", status: "ready",
						createdAt: now.Add(-time.Hour), rootTree: tree, noBlobRefs: true})
				}
			},
			done: func(t *testing.T, s *Server) int {
				return countRetentionRows(t, s, `SELECT COUNT(DISTINCT checkpoint_id) FROM checkpoint_blob_refs`)
			},
		},
		{
			// TWO finalized claws, each with more victims than one hold covers.
			// The budget bounds the PHASE: a spent budget must stop after one
			// hold on the first claw and never reach the second. Note what this
			// row cannot see: a pacer built per claw with an already-spent
			// budget also stops on the first claw, because the phase loop
			// breaks on errRetentionBudget. The property that a fresh pacer per
			// claw actually violates -- the budget's deadline restarting with
			// every claw -- needs a clock, and is TestCompactionBudgetSpansClaws.
			name: "compaction", pacerBuiltIn: "compactFinalizedCheckpoints",
			total: perClaw * 2, firstHold: retentionCheckpointBatch,
			seed: func(t *testing.T, s *Server, now time.Time) {
				for _, claw := range []string{"claw-a", "claw-b"} {
					insertRetentionClaw(t, s, claw, now.Add(-400*24*time.Hour))
					// One more than perClaw: the survivor is kept.
					for i := 0; i <= perClaw; i++ {
						insertRetentionCheckpoint(t, s, retentionCheckpoint{
							id: fmt.Sprintf("%s-cp-%d", claw, i), clawID: claw, status: "ready",
							createdAt: now.Add(-30 * 24 * time.Hour), rootTree: tree, writeManifest: true})
					}
				}
			},
			done: func(t *testing.T, s *Server) int {
				return countRetentionRows(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE status='compacted'`)
			},
		},
		{
			name: "compaction", pacerBuiltIn: "compactFinalizedCheckpoints",
			total: perClaw * 2, firstHold: retentionCheckpointBatch,
			seed: func(t *testing.T, s *Server, now time.Time) {
				// The skipped-row release shares compaction's pacer and budget,
				// across claws.
				for _, claw := range []string{"claw-a", "claw-b"} {
					insertRetentionClaw(t, s, claw, now.Add(-400*24*time.Hour))
					for i := 0; i < perClaw; i++ {
						insertRetentionCheckpoint(t, s, retentionCheckpoint{
							id: fmt.Sprintf("%s-cp-%d", claw, i), clawID: claw, status: "skipped",
							createdAt: now.Add(-30 * 24 * time.Hour), rootTree: tree})
					}
				}
			},
			done: func(t *testing.T, s *Server) int {
				return countRetentionRows(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE status='skipped' AND root_tree_sha256=''`)
			},
		},
		{
			name: "checkpoints", pacerBuiltIn: "applyRetentionWindow",
			total: checkpoints, firstHold: retentionCheckpointBatch,
			seed: func(t *testing.T, s *Server, now time.Time) {
				insertRetentionClaw(t, s, "claw", now)
				for i := 0; i < checkpoints; i++ {
					insertRetentionCheckpoint(t, s, retentionCheckpoint{
						id: fmt.Sprintf("cp-%d", i), clawID: "claw", status: "ready",
						createdAt: now.Add(-400 * 24 * time.Hour), rootTree: tree, writeManifest: true})
				}
			},
			done: func(t *testing.T, s *Server) int {
				return checkpoints - countRetentionRows(t, s, `SELECT COUNT(*) FROM claw_checkpoints`)
			},
		},
		{
			name: "task_run_events", pacerBuiltIn: "applyRetentionWindow",
			total: rows, firstHold: retentionDeleteBatch,
			seed: func(t *testing.T, s *Server, now time.Time) {
				old := now.Add(-400 * 24 * time.Hour).UnixMilli()
				if _, err := s.db.Exec(`INSERT INTO task_runs(id, tenant_id, initial_attempt_id, run_kind, owner_type, created_at, updated_at)
					VALUES('run','tenant','attempt','code_task','manual',?,?)`, old, old); err != nil {
					t.Fatal(err)
				}
				tx, err := s.db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < rows; i++ {
					if _, err := tx.Exec(`INSERT INTO task_run_events(id, tenant_id, run_id, event_key, event_type, event_time, observed_at, created_at)
						VALUES(?,'tenant','run',?,'task_start',?,?,?)`, fmt.Sprintf("ev-%d", i), fmt.Sprintf("ev-%d", i), old, old, old); err != nil {
						t.Fatal(err)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			},
			done: func(t *testing.T, s *Server) int {
				return rows - countRetentionRows(t, s, `SELECT COUNT(*) FROM task_run_events`)
			},
		},
		{
			name: "messages", pacerBuiltIn: "applyRetentionWindow",
			total: rows, firstHold: retentionDeleteBatch,
			seed: func(t *testing.T, s *Server, now time.Time) {
				insertRetentionClaw(t, s, "claw", now)
				old := now.Add(-400 * 24 * time.Hour)
				tx, err := s.db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < rows; i++ {
					if _, err := tx.Exec(`INSERT INTO messages(id, claw_id, tenant_id, role, content, created_at) VALUES(?,?,?,?,?,?)`,
						fmt.Sprintf("msg-%d", i), "claw", "tenant", "user", "old", old); err != nil {
						t.Fatal(err)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			},
			done: func(t *testing.T, s *Server) int {
				return rows - countRetentionRows(t, s, `SELECT COUNT(*) FROM messages`)
			},
		},
		{
			// One hold per tree, so the items are expansion rows and a hold
			// covers one tree's worth.
			name: "tree reference gc", pacerBuiltIn: "retentionSweepCycle",
			total: 6, firstHold: 2,
			seed: func(t *testing.T, s *Server, now time.Time) {
				for i := 0; i < 3; i++ {
					files := []types.CheckpointFile{
						{Path: "a", SHA256: strings.Repeat(fmt.Sprintf("%d", i), 64)},
						{Path: "b", SHA256: strings.Repeat(fmt.Sprintf("%d", i+5), 64)},
					}
					if _, err := s.insertTreeBlobRefs(strings.Repeat(fmt.Sprintf("%x", 10+i), 64), files); err != nil {
						t.Fatal(err)
					}
				}
			},
			done: func(t *testing.T, s *Server) int {
				return 6 - countRetentionRows(t, s, `SELECT COUNT(*) FROM tree_blob_refs`)
			},
		},
	}
}

// countRetentionPauses replaces the pacer's sleep with a counter for the
// duration of the test.
func countRetentionPauses(t *testing.T) *int {
	t.Helper()
	var pauses int
	previous := retentionSleep
	retentionSleep = func(time.Duration) { pauses++ }
	t.Cleanup(func() { retentionSleep = previous })
	return &pauses
}

func TestPacedRetentionPhasesPauseAndStopAtTheBudget(t *testing.T) {
	policy := retentionPolicy{enabled: true, retentionSettings: retentionSettings{
		interval: time.Hour, maxAge: defaultRetentionMaxAge, compactAfter: defaultRetentionCompactAfter}}
	for _, phase := range pacedRetentionPhases() {
		t.Run(phase.name+" with budget", func(t *testing.T) {
			s := newRetentionTestServer(t)
			pauses := countRetentionPauses(t)
			phase.seed(t, s, time.Now())
			out := captureRetentionLog(t, func() { s.retentionSweepCycle(policy) })
			if got := phase.done(t, s); got != phase.total {
				t.Fatalf("%d of %d items done, want all:\n%s", got, phase.total, out)
			}
			if *pauses < 2 {
				t.Fatalf("%d pause(s) across %d items in holds of %d: the write lock was not released between holds", *pauses, phase.total, phase.firstHold)
			}
			if containsString(out, "cycle budget") {
				t.Fatalf("stopped at the budget with a full budget:\n%s", out)
			}
		})
		t.Run(phase.name+" budget spent", func(t *testing.T) {
			s := newRetentionTestServer(t)
			countRetentionPauses(t)
			previous := retentionRowBudget
			retentionRowBudget = -time.Second
			t.Cleanup(func() { retentionRowBudget = previous })
			phase.seed(t, s, time.Now())
			out := captureRetentionLog(t, func() { s.retentionSweepCycle(policy) })
			if got := phase.done(t, s); got != phase.firstHold {
				t.Fatalf("%d items done with a spent budget, want exactly one hold of %d:\n%s", got, phase.firstHold, out)
			}
			if want := "[retention] " + phase.name + ": stopping after"; !containsString(out, want) {
				t.Fatalf("no %q line:\n%s", want, out)
			}
			if !containsString(out, "cycle done") {
				t.Fatalf("a spent budget must not lose the cycle summary:\n%s", out)
			}
		})
	}
}

// TestEveryBulkWriteLoopIsPaced reads retention.go and rejects any loop that
// begins write transactions on s.db -- directly, or through a function in the
// file that does -- unless the loop, directly or through such a function,
// yields to a retentionPacer. It also requires every function that constructs
// a pacer to have a row in pacedRetentionPhases, so the behavioural test above
// covers it.
//
// What it cannot see: writes reached through a method on a receiver other than
// s that is not declared in retention.go, a closure assigned to a local (the
// backfill's flush -- its loop is caught through the tree expansion it also
// calls), and loops in other files. Callees are matched by name, so two
// functions sharing a name are merged, conservatively, into one set of facts.
func TestEveryBulkWriteLoopIsPaced(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "retention.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	decls := map[string][]*ast.FuncDecl{}
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			decls[fd.Name.Name] = append(decls[fd.Name.Name], fd)
		}
	}
	isSelector := func(x ast.Expr, name string) bool {
		id, ok := x.(*ast.Ident)
		return ok && id.Name == name
	}
	// s.db.Exec / s.db.Begin (and their Context forms) begin a write-lock hold.
	// tx.Exec does not: it is inside a hold the caller already bounded.
	directWrite := func(call *ast.CallExpr) bool {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		db, ok := sel.X.(*ast.SelectorExpr)
		if !ok || !isSelector(db.X, "s") || db.Sel.Name != "db" {
			return false
		}
		switch sel.Sel.Name {
		case "Exec", "ExecContext", "Begin", "BeginTx":
			return true
		}
		return false
	}
	directYield := func(call *ast.CallExpr) bool {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "yield"
	}
	calleeName := func(call *ast.CallExpr) string {
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if _, ok := decls[name]; ok {
			return name
		}
		return ""
	}
	// closure computes the set of functions that, directly or through a callee
	// in the file, satisfy direct.
	closure := func(direct func(*ast.CallExpr) bool) map[string]bool {
		set := map[string]bool{}
		for changed := true; changed; {
			changed = false
			for name, fds := range decls {
				if set[name] {
					continue
				}
				for _, fd := range fds {
					ast.Inspect(fd.Body, func(n ast.Node) bool {
						if call, ok := n.(*ast.CallExpr); ok && (direct(call) || set[calleeName(call)]) {
							set[name] = true
							changed = true
						}
						return true
					})
				}
			}
		}
		return set
	}
	writes, yields := closure(directWrite), closure(directYield)
	bodyHas := func(body ast.Node, direct func(*ast.CallExpr) bool, set map[string]bool) bool {
		found := false
		ast.Inspect(body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && (direct(call) || set[calleeName(call)]) {
				found = true
			}
			return !found
		})
		return found
	}

	// boundedByConstruction names loops that begin write transactions and are
	// allowed not to yield, with the reason. Empty today; a future entry must
	// say why the number of holds is bounded by something other than a pacer.
	boundedByConstruction := map[string]string{}

	writingLoops := 0
	for name, fds := range decls {
		for _, fd := range fds {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				var body *ast.BlockStmt
				switch loop := n.(type) {
				case *ast.ForStmt:
					body = loop.Body
				case *ast.RangeStmt:
					body = loop.Body
				default:
					return true
				}
				if !bodyHas(body, directWrite, writes) {
					return true
				}
				writingLoops++
				if bodyHas(body, directYield, yields) {
					return true
				}
				if why, ok := boundedByConstruction[name]; ok {
					t.Logf("%s: unpaced write loop at %s allowed: %s", name, fset.Position(n.Pos()), why)
					return true
				}
				t.Errorf("%s: the loop at %s begins write transactions without yielding to a retentionPacer; batch it and call yield after every commit, or add it to boundedByConstruction with the reason",
					name, fset.Position(n.Pos()))
				return true
			})
		}
	}
	if writingLoops < 6 {
		t.Fatalf("found only %d loops that begin write transactions; the detector is not seeing the phases it exists to check", writingLoops)
	}

	// Every pacer constructor has a behavioural row, and every row names a
	// real constructor.
	constructors := map[string]bool{}
	for name, fds := range decls {
		for _, fd := range fds {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && calleeName(call) == "newRetentionPacer" {
					constructors[name] = true
				}
				return true
			})
		}
	}
	covered := map[string]bool{}
	for _, phase := range pacedRetentionPhases() {
		covered[phase.pacerBuiltIn] = true
		if !constructors[phase.pacerBuiltIn] {
			t.Errorf("pacedRetentionPhases names %s as building a pacer, but nothing there calls newRetentionPacer", phase.pacerBuiltIn)
		}
	}
	var missing []string
	for name := range constructors {
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("functions that build a retentionPacer without a row in pacedRetentionPhases: %s", strings.Join(missing, ", "))
	}

	// One budget per PHASE. A pacer is constructed only at a phase's entry
	// point: never inside a loop body, and never in a function that a loop
	// body in this file reaches. Moving newRetentionPacer into the per-claw
	// path handed every claw a fresh budget, passed the loop check above and
	// both behavioural subtests on a one-claw fixture, and left the phase
	// unbounded again. The two-claw compaction rows in pacedRetentionPhases
	// catch that behaviourally; this catches it structurally, for every phase.
	//
	// The sweeper's tick loop is the one loop that legitimately reaches a
	// constructor: it runs one whole cycle per tick.
	const tickLoop = "retentionSweeper"
	reachable := map[string]bool{}
	for name, fds := range decls {
		if name == tickLoop {
			continue
		}
		for _, fd := range fds {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				var body *ast.BlockStmt
				switch loop := n.(type) {
				case *ast.ForStmt:
					body = loop.Body
				case *ast.RangeStmt:
					body = loop.Body
				default:
					return true
				}
				ast.Inspect(body, func(m ast.Node) bool {
					if call, ok := m.(*ast.CallExpr); ok {
						if callee := calleeName(call); callee != "" {
							reachable[callee] = true
						}
					}
					return true
				})
				return true
			})
		}
	}
	for changed := true; changed; {
		changed = false
		for name := range reachable {
			for _, fd := range decls[name] {
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok {
						if callee := calleeName(call); callee != "" && !reachable[callee] {
							reachable[callee] = true
							changed = true
						}
					}
					return true
				})
			}
		}
	}
	if reachable["newRetentionPacer"] {
		var via []string
		for name := range constructors {
			if reachable[name] {
				via = append(via, name)
			}
		}
		sort.Strings(via)
		t.Errorf("newRetentionPacer is reachable from a loop body (through %s): a pacer built per item gives every item a fresh budget and leaves the phase unbounded; construct it once at the phase entry and pass it down", strings.Join(via, ", "))
	}
}

// ---------------------------------------------------------------------------
// The dry run writes nothing but the backfill
// ---------------------------------------------------------------------------

// The dry run is the step the documentation says to run first, on a hub that
// is by hypothesis nearly full. Building an index there needs free space for
// the whole B-tree before anything has been reclaimed.
func TestDryRunDoesNotBuildRetentionIndexes(t *testing.T) {
	s := newRetentionTestServer(t)
	for _, idx := range retentionIndexes {
		if _, err := s.db.Exec(`DROP INDEX ` + idx.name); err != nil {
			t.Fatal(err)
		}
	}
	policy := retentionPolicy{enabled: true, retentionSettings: retentionSettings{
		interval: time.Hour, maxAge: defaultRetentionMaxAge, compactAfter: defaultRetentionCompactAfter, dryRun: true}}
	s.retentionSweepCycle(policy)
	for _, idx := range retentionIndexes {
		if retentionIndexExists(t, s, idx.name) {
			t.Fatalf("a dry run built %s", idx.name)
		}
	}
	// A real cycle builds them once it has reclaimed something to build from
	// (buildRetentionIndexesAfterCycle): give it one aged orphan blob.
	orphan := writeRetentionBlob(t, []byte("unreferenced"))
	ageBlob(t, orphan)
	policy.dryRun = false
	s.retentionSweepCycle(policy)
	for _, idx := range retentionIndexes {
		if !retentionIndexExists(t, s, idx.name) {
			t.Fatalf("a real cycle that reclaimed bytes did not build %s", idx.name)
		}
	}
}

// ---------------------------------------------------------------------------
// Per-item error lines are capped
// ---------------------------------------------------------------------------

func TestRetentionErrorLogCapsPerItemLines(t *testing.T) {
	const extra = 37
	l := &retentionErrorLog{phase: "blob sweep"}
	out := captureRetentionLog(t, func() {
		for i := 0; i < retentionErrorLogMax+extra; i++ {
			l.add("remove blob %d: read-only file system", i)
		}
		l.flush()
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != retentionErrorLogMax+1 {
		t.Fatalf("%d lines for %d failures, want %d per-item lines and one summary:\n%s",
			len(lines), retentionErrorLogMax+extra, retentionErrorLogMax, out)
	}
	for i := 0; i < retentionErrorLogMax; i++ {
		if want := fmt.Sprintf("[retention] remove blob %d: read-only file system", i); lines[i] != want {
			t.Fatalf("line %d = %q, want %q", i, lines[i], want)
		}
	}
	summary := lines[retentionErrorLogMax]
	for _, want := range []string{"[retention] blob sweep:", fmt.Sprintf("%d more item error(s)", extra), "remove blob 0: read-only file system"} {
		if !containsString(summary, want) {
			t.Fatalf("summary %q does not say %q", summary, want)
		}
	}

	// Under the cap there is nothing to summarise.
	l = &retentionErrorLog{phase: "checkpoints"}
	out = captureRetentionLog(t, func() {
		l.add("remove expired manifest x: permission denied")
		l.flush()
	})
	if strings.Count(out, "\n") != 1 || containsString(out, "more item error") {
		t.Fatalf("one failure produced:\n%s", out)
	}
}

// The same through a phase: unremovable diagnostics logs past the cap are
// counted, not logged one by one.
func TestDiagnosticsPruneCapsItsErrorLines(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unwritable directory does not stop os.Remove")
	}
	const extra = 3
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(hubDataDir(), "diagnostics")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-200 * 24 * time.Hour)
	for i := 0; i < retentionErrorLogMax+extra; i++ {
		path := filepath.Join(dir, fmt.Sprintf("claw-%d.log", i))
		if err := os.WriteFile(path, []byte("captured gateway log"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })

	var result diagnosticsPruneResult
	out := captureRetentionLog(t, func() {
		var err error
		result, err = pruneDiagnosticsLogs(hubDataDir(), time.Now(), false)
		if err != nil {
			t.Fatalf("pruneDiagnosticsLogs: %v", err)
		}
	})
	if result.itemErrors != retentionErrorLogMax+extra {
		t.Fatalf("itemErrors = %d, want %d: the cap is on lines, not on the count", result.itemErrors, retentionErrorLogMax+extra)
	}
	if n := strings.Count(out, "[retention] remove diagnostics log"); n != retentionErrorLogMax {
		t.Fatalf("%d per-item lines, want %d:\n%s", n, retentionErrorLogMax, out)
	}
	if !containsString(out, fmt.Sprintf("[retention] diagnostics: %d more item error(s)", extra)) {
		t.Fatalf("no summary of the %d swallowed failures:\n%s", extra, out)
	}
}

// ---------------------------------------------------------------------------
// The policy is read once, at boot
// ---------------------------------------------------------------------------

// The settings and AI-config apply paths replace s.hubCfg wholesale from a
// proposed YAML. A sweeper that re-read it every tick could be armed -- or
// silenced -- by a proposal, with no policy line and none of the grace a
// restart gives. The sweeper runs what it booted with and says when the
// configuration has drifted.
func TestSweeperRunsTheBootPolicyUntilRestart(t *testing.T) {
	s := newRetentionTestServer(t)
	enabled := true
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: &enabled, DryRun: true}}
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	insertRetentionClaw(t, s, "claw", reference.Add(-400*24*time.Hour))
	manifest := insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: reference.Add(-400 * 24 * time.Hour), writeManifest: true})

	var run *retentionRun
	boot := captureRetentionLog(t, func() { run = s.startRetentionRun() })
	if !containsString(boot, "[retention] enabled: dry_run=true") {
		t.Fatalf("boot line:\n%s", boot)
	}

	// A settings apply now flips dry_run off. Deletion must NOT be armed.
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: &enabled, DryRun: false}}
	out := captureRetentionLog(t, run.cycle)
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("a configuration change armed deletion without a restart: %v", err)
	}
	if !containsString(out, "cycle start: dry_run=true") {
		t.Fatalf("the cycle did not run the boot policy:\n%s", out)
	}
	for _, want := range []string{
		"[retention] configuration changed since boot",
		"configured [enabled=true dry_run=false",
		"running [enabled=true dry_run=true",
		"until the hub restarts",
	} {
		if !containsString(out, want) {
			t.Fatalf("no %q in:\n%s", want, out)
		}
	}
	// Said once per change, not once per tick.
	if again := captureRetentionLog(t, run.cycle); containsString(again, "configuration changed") {
		t.Fatalf("the divergence line repeats every tick:\n%s", again)
	}

	// A proposal that omits the section entirely does not silence the sweeper.
	s.hubCfg = &types.HubConfig{}
	out = captureRetentionLog(t, run.cycle)
	if !containsString(out, "cycle start: dry_run=true") || !containsString(out, "configured [enabled=false") {
		t.Fatalf("an omitted section must neither stop the cycle nor go unreported:\n%s", out)
	}

	// And putting it back is reported too.
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: &enabled, DryRun: true}}
	if out = captureRetentionLog(t, run.cycle); !containsString(out, "matches the running policy again") {
		t.Fatalf("a configuration restored to the boot policy went unreported:\n%s", out)
	}
	if n := countRetentionRows(t, s, `SELECT COUNT(*) FROM claw_checkpoints`); n != 1 {
		t.Fatalf("%d rows left after four dry-run cycles, want 1", n)
	}
}

// A disabled sweeper says so on every tick rather than returning silently:
// silence is what a wedged sweeper looks like too.
func TestDisabledCycleSaysSo(t *testing.T) {
	s := newRetentionTestServer(t)
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(false)}}
	out := captureRetentionLog(t, s.retentionSweepOnce)
	if !containsString(out, "[retention] cycle skipped: retention disabled") {
		t.Fatalf("a disabled cycle left no trace:\n%q", out)
	}
	if containsString(out, "cycle start") {
		t.Fatalf("a disabled cycle ran:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// A blob write survives the sweep pruning its directory
// ---------------------------------------------------------------------------

// pruneEmptyDirs removes a fan-out directory it observed empty, outside any
// interlock. A writer between its MkdirAll and its OpenFile finds the
// directory gone; the create must recreate it once rather than fail the
// checkpoint.
func TestBlobWriteSurvivesTheSweepPruningItsDirectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(checkpointsRoot(), "blobs", "sha256")
	for name, write := range map[string]func(path string) error{
		"message blob via writeFileAtomic": func(path string) error {
			return writeFileAtomic(path, []byte("blob"), 0o640)
		},
		"upload via createExclusiveRecreatingDir": func(path string) error {
			f, err := createExclusiveRecreatingDir(path+".tmp-x", 0o640)
			if err != nil {
				return err
			}
			if _, err := f.Write([]byte("blob")); err != nil {
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			return os.Rename(path+".tmp-x", path)
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := checkpointBlobPath(strings.Repeat(fmt.Sprintf("%02x", len(name)), 32))
			// The window: the writer's MkdirAll has returned, and the sweep
			// then pruned the directory it had just created.
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				t.Fatal(err)
			}
			pruneEmptyDirs(root)
			if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Fatalf("fixture: the fan-out directory was not pruned (%v)", err)
			}
			if err := write(path); err != nil {
				t.Fatalf("the write failed after the sweep pruned its directory: %v", err)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "blob" {
				t.Fatalf("blob after write: %q, %v", data, err)
			}
		})
	}
}
