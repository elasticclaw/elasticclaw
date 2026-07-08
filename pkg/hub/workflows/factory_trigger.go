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

	// Check required fields (key must be present AND value must be non-empty)
	for _, in := range inputs {
		if !in.Required {
			continue
		}
		raw, ok := values[in.Name]
		if !ok {
			return nil, fmt.Errorf("missing required input %q", in.Name)
		}
		// Reject explicit empty strings, null, and false (for bools, false is valid)
		switch v := raw.(type) {
		case string:
			if v == "" {
				return nil, fmt.Errorf("required input %q cannot be empty", in.Name)
			}
		case nil:
			return nil, fmt.Errorf("required input %q cannot be null", in.Name)
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

	// Apply defaults for missing optional inputs, then validate them too
	for _, in := range inputs {
		if _, ok := result[in.Name]; ok {
			continue
		}
		if in.Default != "" {
			result[in.Name] = in.Default
		}
	}

	// Number range validation (min / max) — applied to both user values and defaults
	for _, in := range inputs {
		if in.Type != "number" || (in.Min == nil && in.Max == nil) {
			continue
		}
		str, ok := result[in.Name]
		if !ok || str == "" {
			continue
		}
		num, err := strconv.ParseFloat(str, 64)
		if err != nil {
			return nil, fmt.Errorf("input %q: expected number, got %q", in.Name, str)
		}
		if in.Min != nil && num < *in.Min {
			return nil, fmt.Errorf("input %q: %v is below minimum %v", in.Name, num, *in.Min)
		}
		if in.Max != nil && num > *in.Max {
			return nil, fmt.Errorf("input %q: %v is above maximum %v", in.Name, num, *in.Max)
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
		// unreachable — all branches above return
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
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	name := r.PathValue("name")
	if name == "" {
		jsonError(w, http.StatusBadRequest, "missing factory name")
		return
	}

	s.triggerFactoryByName(w, r, name)
}

func (s *Server) triggerFactoryByName(w http.ResponseWriter, r *http.Request, name string) {
	factory, err := s.resolveFactoryByName(name)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to load factory: "+err.Error())
		return
	}
	if factory == nil {
		jsonError(w, http.StatusNotFound, "factory not found")
		return
	}
	s.triggerFactoryConfig(w, r, factory)
}

func (s *Server) triggerFactoryConfig(w http.ResponseWriter, r *http.Request, factory *types.FactoryConfig) {
	// Verify factory supports manual triggers
	if !factory.EnableManualTrigger {
		jsonError(w, http.StatusForbidden, "factory does not support manual triggers")
		return
	}

	// Verify factory is enabled
	if factory.Enabled != nil && !*factory.Enabled {
		jsonError(w, http.StatusForbidden, "factory is disabled")
		return
	}

	// Parse request body
	var req FactoryTriggerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Validate inputs against factory schema
	validatedInputs, err := validateFactoryInputs(factory.Inputs, req.Inputs)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "validation error: "+err.Error())
		return
	}

	// Create claw from factory with manual trigger inputs
	clawID, _, err := s.createClawFromFactory(factory, "", validatedInputs, nil, "manual trigger")
	if err != nil {
		s.trackFactoryCreationFailure(factory.Name, "", err.Error())
		jsonError(w, http.StatusInternalServerError, "failed to create claw: "+err.Error())
		return
	}

	s.trackFactoryCreationSuccess(factory.Name, "", clawID)

	jsonOK(w, map[string]string{
		"claw_id": clawID,
		"status":  "created",
	})
}

func (s *Server) resolveFactoryByName(name string) (*types.FactoryConfig, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil
	}
	if factory, err := loadExternalFactory(name); err == nil {
		return factory, nil
	}
	for _, factory := range s.resolveFactories() {
		if factory != nil && strings.EqualFold(factory.Name, name) {
			return factory, nil
		}
	}
	return nil, nil
}
