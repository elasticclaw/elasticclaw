package hub

import (
	"fmt"
	"time"
)

type retryOptions struct {
	Label    string
	Attempts int
	Delays   []time.Duration
	Sleep    func(time.Duration)
	Run      func() error
}

// retryOperation runs a bounded operation with an explicit delay before each
// attempt. A missing delay is treated as zero; the last supplied delay is
// reused when there are more attempts than delays.
func retryOperation(opts retryOptions) error {
	if opts.Attempts < 1 {
		opts.Attempts = 1
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	if opts.Label == "" {
		opts.Label = "operation"
	}

	var lastErr error
	for attempt := 1; attempt <= opts.Attempts; attempt++ {
		delay := retryDelay(opts.Delays, attempt-1)
		if delay > 0 {
			opts.Sleep(delay)
		}
		if err := opts.Run(); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("%s failed after %d attempts: %s", opts.Label, opts.Attempts, sanitizeFailureDetails(lastErr.Error()))
}

func retryDelay(delays []time.Duration, index int) time.Duration {
	if len(delays) == 0 {
		return 0
	}
	if index < len(delays) {
		return delays[index]
	}
	return delays[len(delays)-1]
}
