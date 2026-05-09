package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// FactoryTriggerRequest is the payload for POST /api/factories/{name}/trigger.
type FactoryTriggerRequest struct {
	Inputs map[string]interface{} `json:"inputs"`
}

// regexCache caches compiled regexes to avoid recompiling on every request.
// Keyed by pattern string.
var regexCache sync.Map // map[string]*regexp.Regexp

func getCachedRegex(pattern string) (*regexp.Regexp, error) {
	if cached, ok := regexCache.Load(pattern); ok {
		return cached.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	regexCache.Store(pattern, re)
	return re, nil
}

// validateFactoryInputs validates user-provided inputs against a factory's
// input schema. Returns a map[string]string of coerced values for template
// rendering. All values are returned as strings so they can be used in
// text/template pipelines uniformly.
func validateFactoryInputs(inputs []types.FactoryInput, values map[string]interface{}) (map[string]string, error) {
	result := make(map[string]string)

	// Build lookup of defined inputs
	defined := make(map[string]types.FactoryInput)
	for _, in := range inputs {
		defined[in.Name] = in
	}

	// Check required fields
	for _, in := range inputs {
		if !in.Required {
			continue
		}
		if _, ok := values[in.Name]; !ok {
			return nil, fmt.Errorf("missing required input %q", in.Name)
		}
	}

	// Validate and coerce each provided value
	for name, raw := range values {
		def, ok := defined[name]
		if !ok {
			return nil, fmt.Errorf("unknown input %q", name)
		}

		str, err := coerceInput(def.Type, raw)
		if err != nil {
			return nil, fmt.Errorf("input %q: %w", name, err)
		}

		// Enum check
		if def.Type == "enum" && len(def.Options) > 0 {
			found := false
			for _, opt := range def.Options {
				if str == opt {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("input %q: %q is not a valid option (%v)", name, str, def.Options)
			}
		}

		// Regex validation (cached)
		if def.Validation != "" {
			re, err := getCachedRegex(def.Validation)
			if err != nil {
				return nil, fmt.Errorf("input %q: invalid validation regex: %w", name, err)
			}
			if !re.MatchString(str) {
				return nil, fmt.Errorf("input %q: %q does not match validation pattern", name, str)
			}
		}

		result[name] = str
	}

	// Apply defaults for missing optional inputs
	for _, in := range inputs {
		if _, ok := result[in.Name]; ok {
			continue
		}
		if in.Default != "" {
			result[in.Name] = in.Default
		}
	}

	return result, nil
}

// coerceInput converts a raw JSON value to a string based on the expected type.
func coerceInput(typ string, raw interface{}) (string, error) {
	switch typ {
	case "string":
		switch v := raw.(type) {
		case string:
			return v, nil
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64), nil
		case bool:
			return strconv.FormatBool(v), nil
		default:
			return fmt.Sprintf("%v", v), nil
		}
	case "number":
		switch v := raw.(type) {
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64), nil
		case int:
			return strconv.Itoa(v), nil
		case string:
			// Validate it's a valid number
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				return "", fmt.Errorf("expected number, got %q", v)
			}
			return v, nil
		default:
			return "", fmt.Errorf("expected number, got %T", raw)
		}
	case "bool":
		switch v := raw.(type) {
		case bool:
			return strconv.FormatBool(v), nil
		case string:
			v = strings.ToLower(v)
			if v == "true" || v == "1" || v == "yes" || v == "on" {
				return "true", nil
			}
			if v == "false" || v == "0" || v == "no" || v == "off" {
				return "false", nil
			}
			return "", fmt.Errorf("expected bool, got %q", v)
		default:
			return "", fmt.Errorf("expected bool, got %T", raw)
		}
	case "enum":
		switch v := raw.(type) {
		case string:
			return v, nil
		default:
			return "", fmt.Errorf("expected enum value (string), got %T", raw)
		}
	default:
		// Unknown type — accept as string
		return fmt.Sprintf("%v", raw), nil
	}
}

// handleFactoryTrigger handles POST /api/factories/{name}/trigger.
func (s *Server) handleFactoryTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing factory name", http.StatusBadRequest)
		return
	}

	// Load factory
	factory, err := loadExternalFactory(name)
	if err != nil {
		// Fall back to in-memory
		s.mu.RLock()
		for _, f := range s.hubCfg.Factories {
			if f != nil && strings.EqualFold(f.Name, name) {
				factory = f
				break
			}
		}
		s.mu.RUnlock()
	}
	if factory == nil {
		http.Error(w, "factory not found", http.StatusNotFound)
		return
	}

	// Parse request body
	var req FactoryTriggerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Validate inputs against factory schema
	validatedInputs, err := validateFactoryInputs(factory.Inputs, req.Inputs)
	if err != nil {
		http.Error(w, "validation error: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Create claw from factory with manual trigger inputs
	clawID, err := s.createClawFromFactory(factory, "", validatedInputs, nil)
	if err != nil {
		http.Error(w, "failed to create claw: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]string{
		"claw_id": clawID,
		"status":  "created",
	})
}
