package readhttp

import "strings"

// StreamRedactor holds only a possible credential prefix between model chunks.
// A credential split across arbitrary token boundaries never reaches consumers.
type StreamRedactor struct {
	Secrets []string
	pending string
}

func (s *StreamRedactor) Write(text string) string {
	s.pending += text
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
		if partial {
			break
		}
		if matched > 0 {
			out.WriteString("[redacted]")
			s.pending = s.pending[matched:]
		} else {
			out.WriteByte(s.pending[0])
			s.pending = s.pending[1:]
		}
	}
	return out.String()
}
func (s *StreamRedactor) Flush() string {
	remaining := Redact(s.pending, s.Secrets)
	s.pending = ""
	return remaining
}
