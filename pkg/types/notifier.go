package types

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// NotifierConfig defines an outbound messaging transport and its
// provider-specific settings. In YAML and JSON the settings are inline next
// to the type, so provider settings stay open-ended:
//
//	notifiers:
//	  eng-agents:
//	    type: slack
//	    token_secret: slack_bot_token
//	    channel: C0123ABCD
type NotifierConfig struct {
	Type     string         `yaml:"type"`
	Settings map[string]any `yaml:",inline"`
}

func (n *NotifierConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("notifier: expected a mapping")
	}
	var flattened map[string]any
	if err := value.Decode(&flattened); err != nil {
		return err
	}
	return n.fromFlattened(flattened)
}

func (n NotifierConfig) MarshalYAML() (interface{}, error) {
	return n.flattened(), nil
}

func (n *NotifierConfig) UnmarshalJSON(data []byte) error {
	var flattened map[string]any
	if err := json.Unmarshal(data, &flattened); err != nil {
		return err
	}
	if flattened == nil {
		return fmt.Errorf("notifier: expected an object")
	}
	return n.fromFlattened(flattened)
}

func (n NotifierConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(n.flattened())
}

func (n *NotifierConfig) fromFlattened(flattened map[string]any) error {
	n.Type = ""
	n.Settings = make(map[string]any, len(flattened))
	for key, value := range flattened {
		if key == "type" {
			var ok bool
			n.Type, ok = value.(string)
			if !ok {
				return fmt.Errorf("notifier type must be a string")
			}
			continue
		}
		n.Settings[key] = value
	}
	return nil
}

func (n NotifierConfig) flattened() map[string]any {
	flattened := make(map[string]any, len(n.Settings)+1)
	for key, value := range n.Settings {
		if key != "type" {
			flattened[key] = value
		}
	}
	flattened["type"] = n.Type
	return flattened
}
