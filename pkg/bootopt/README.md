# Boot Time Autoresearch (`pkg/bootopt`)

Autonomous bootstrap optimization using LLM-generated hypotheses.

## How it works

1. **Hypothesis generation** — LLM (Anthropic/Opus) analyzes current bootstrap code and proposes a speed improvement
2. **Patch application** — Framework applies the proposed change to a copy of the codebase
3. **Correctness test** — Run 1 iteration to verify the change doesn't break bootstrap
4. **Performance test** — Run 10 iterations, measure mean/median/p95 boot time
5. **Decision** — If faster and correct, keep. If slower or broken, discard.
6. **Iterate** — Feed results back to LLM for next hypothesis

## Running

```bash
go run ./cmd/bootopt \
  -iterations 20 \
  -anthropic-key $ANTHROPIC_API_KEY \
  -test-command "make test-bootstrap"
```

## Architecture

- `hypothesis.go` — LLM prompt construction, response parsing
- `patch.go` — Git patch application, rollback
- `test.go` — Test runner (correctness + performance)
- `measure.go` — Time measurement utilities
- `state.go` — Iteration state, results DB
- `main.go` — CLI and orchestration loop

## Measurement Strategy

Since we can't provision real Replicated VMs in a loop, we measure what we can:

1. **Script generation time** — pure Go functions (fast, not the bottleneck)
2. **Containerized bootstrap** — Docker with stubbed network calls (catches structural issues)
3. **Step-level timing** — Each bootstrap phase is timed independently
4. **Real VM validation** — Final winning changes are validated on real Replicated VMs manually

The framework optimizes the *known* parts (Node install, OpenClaw install, config, gateway start) and tracks hypotheses about the *unknown* parts (sandbox startup, bridge download).
