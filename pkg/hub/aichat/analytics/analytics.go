// Package analytics provides read-only analytics queries.
package analytics

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

// Source exposes only tools that read analytics data.
type Source interface{ Tools() []tools.Tool }

func result(provider string, value any, count int) (tools.Result, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return tools.Result{}, fmt.Errorf("Invalid provider response.")
	}
	return tools.Result{Summary: fmt.Sprintf("Read %d rows from %s", count, provider), Data: string(data), RowCount: count}, nil
}

func stringArg(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
func intArg(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}
func rowLimit(n int) (int, error) {
	if n < 0 {
		return 0, tools.ArgError("Limit must be positive.")
	}
	if n == 0 {
		return 100, nil
	}
	if n > 500 {
		n = 500
	}
	return n, nil
}

var readStatement = regexp.MustCompile(`(?i)^\s*(SELECT|WITH)\b`)

// Reject additional statements and SQL write clauses even inside a WITH query.
// String literals, quoted identifiers and property names do not contribute keywords.
func readQuery(query string) bool {
	if len(query) == 0 || len(query) > 32000 || !readStatement.MatchString(query) {
		return false
	}
	var tokens []string
	var word strings.Builder
	property := false
	flush := func() {
		if word.Len() > 0 {
			if !property {
				tokens = append(tokens, strings.ToUpper(word.String()))
			}
			property = false
			word.Reset()
		}
	}
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c == '\'' || c == '"' || c == '`' {
			flush()
			property = false
			quote := c
			closed := false
			for i++; i < len(query); i++ {
				if query[i] == '\\' {
					i++
					continue
				}
				if query[i] == quote {
					if i+1 < len(query) && query[i+1] == quote {
						i++
						continue
					}
					closed = true
					break
				}
			}
			if !closed {
				return false
			}
			continue
		}
		if c == ';' || (c == '-' && i+1 < len(query) && query[i+1] == '-') || (c == '/' && i+1 < len(query) && query[i+1] == '*') || c == 0 {
			return false
		}
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || (c >= '0' && c <= '9' && word.Len() > 0) {
			word.WriteByte(c)
		} else {
			flush()
			if c == '.' {
				property = true
			} else if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
				property = false
			}
		}
	}
	flush()
	if len(tokens) == 0 || (tokens[0] != "SELECT" && tokens[0] != "WITH") {
		return false
	}
	selectSeen := false
	for _, t := range tokens {
		switch t {
		case "SELECT":
			selectSeen = true
		case "INSERT", "UPDATE", "DELETE", "ALTER", "DROP", "CREATE", "TRUNCATE", "GRANT", "REVOKE", "ATTACH", "DETACH", "INTO", "OUTFILE", "FORMAT", "SETTINGS", "SYSTEM", "OPTIMIZE", "KILL", "CALL", "EXECUTE", "COPY", "MERGE":
			return false
		}
	}
	return selectSeen
}
