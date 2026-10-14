package readhttp

import "strings"

// StreamRedactor holds only a possible credential prefix between model chunks.
// A credential split across arbitrary token boundaries never reaches consumers.
type StreamRedactor struct {
	Secrets []string
	pending string
	emitted int
}

func (s *StreamRedactor) Write(text string) string {
	s.pending += text
	return s.drain(false)
}

func (s *StreamRedactor) drain(flush bool) string {
	var out strings.Builder
	for len(s.pending) > 0 {
		matched := 0
		partial := false
		for _, secret := range s.Secrets {
			if secret == "" {
				continue
			}
			if strings.HasPrefix(s.pending, secret) && len(secret) > matched {
				matched = len(secret)
			}
			if len(s.pending) < len(secret) && strings.HasPrefix(secret, s.pending) {
				partial = true
			}
		}
		if partial && !flush {
			break
		}
		if matched > 0 {
			if matched > s.emitted {
				out.WriteString("[redacted]")
			}
		} else {
			if s.emitted == 0 {
				out.WriteByte(s.pending[0])
			}
			matched = 1
		}
		s.pending = s.pending[matched:]
		s.emitted = max(0, s.emitted-matched)
	}
	return out.String()
}
func (s *StreamRedactor) Flush() string {
	// Keep the released prefix as lookbehind in case a later response completes
	// a credential. Already emitted bytes must not be emitted again.
	pending := s.pending
	remaining := s.drain(true)
	s.pending, s.emitted = pending, len(pending)
	return remaining
}
