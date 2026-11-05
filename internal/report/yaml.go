package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	yaml "sigs.k8s.io/yaml/goyaml.v3"

	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

// ToYAML renders the report for humans (the ConfigMap) and for the receiver.
// Every string is double-quoted — a value that *is* quotes, spaces or '#' must
// read unambiguously, since YAML retyping is the very bug this agent exists
// for — null stays bare, numbers and booleans stay plain, and keys are plain
// unless YAML would retype them (an index "0" is quoted so it stays a string).
// Field order is the struct order: the JSON encoding is walked as a token
// stream, so no map reordering happens on the way.
func ToYAML(r *schema.Report) ([]byte, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	node, err := nodeFromJSON(dec)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func nodeFromJSON(dec *json.Decoder) (*yaml.Node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := keyTok.(string)
				val, err := nodeFromJSON(dec)
				if err != nil {
					return nil, err
				}
				n.Content = append(n.Content, keyNode(key), val)
			}
			_, err := dec.Token() // '}'
			return n, err
		case '[':
			n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			for dec.More() {
				val, err := nodeFromJSON(dec)
				if err != nil {
					return nil, err
				}
				n.Content = append(n.Content, val)
			}
			_, err := dec.Token() // ']'
			return n, err
		}
		return nil, fmt.Errorf("unexpected delimiter %v", v)
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}, nil
	case json.Number:
		tag := "!!int"
		if _, err := strconv.ParseInt(v.String(), 10, 64); err != nil {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v.String()}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(v)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	return nil, fmt.Errorf("unexpected token %T", tok)
}

// keyNode keeps keys plain unless a YAML reader would retype them.
func keyNode(k string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}
	if k == "" || looksTyped(k) {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

func looksTyped(s string) bool {
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	switch s {
	case "true", "false", "null", "~", "yes", "no", "on", "off", "y", "n", "True", "False", "Null", "Yes", "No", "On", "Off":
		return true
	}
	return false
}

// WriteYAML writes ToYAML to w.
func WriteYAML(w io.Writer, r *schema.Report) error {
	b, err := ToYAML(r)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
