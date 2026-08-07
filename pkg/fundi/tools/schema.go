package tools

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Schema is a JSON Schema builder for tool input schemas. A zero-value Schema
// produces an empty JSON object ({}), which the Anthropic API accepts as a
// valid tool input schema meaning "no constraints".
//
// The builder covers the subset of JSON Schema draft 2020-12 that the Anthropic
// API accepts: top-level "type", "properties", "required", and per-property
// "type", "description", "items", and nested "properties"/"required".
type Schema struct {
	Type        string
	Description string
	Properties  []SchemaProperty
	Required    []string
}

// SchemaProperty describes one property in a Schema's "properties" map.
// For array types, set Items to describe the element type.
// For object types, set Properties/Required to describe the nested shape.
type SchemaProperty struct {
	Name        string
	Type        string // "string", "integer", "boolean", "array", "object", "number"
	Description string
	Items       *Schema
	Properties  []SchemaProperty
	Required    []string
}

// JSON serialises this Schema to a JSON Schema object ready for the Anthropic
// API's ToolInputSchemaParam field. The returned json.RawMessage is always
// valid JSON — Schema validates its own structure and panics on programmer
// errors (missing "type" on a top-level schema, for example).
func (s Schema) JSON() json.RawMessage {
	b, err := json.Marshal(jsonSchemaValue(s))
	if err != nil {
		panic(fmt.Sprintf("tools: schema.JSON failed to marshal: %v", err))
	}
	return json.RawMessage(b)
}

// jsonSchemaValue converts a Schema to its JSON representation. A Schema
// always produces a map.
func jsonSchemaValue(s Schema) map[string]any {
	out := map[string]any{}
	if s.Type != "" {
		out["type"] = s.Type
	}
	if s.Description != "" {
		out["description"] = s.Description
	}
	if len(s.Properties) > 0 {
		props := map[string]any{}
		for _, p := range s.Properties {
			props[p.Name] = jsonSchemaProperty(p)
		}
		out["properties"] = props
	}
	if len(s.Required) > 0 {
		out["required"] = s.Required
	}
	return out
}

func jsonSchemaProperty(p SchemaProperty) map[string]any {
	out := map[string]any{}
	if p.Type != "" {
		out["type"] = p.Type
	}
	if p.Description != "" {
		out["description"] = strings.Join(strings.Fields(p.Description), " ")
	}
	if p.Items != nil {
		out["items"] = jsonSchemaValue(*p.Items)
	}
	if len(p.Properties) > 0 || len(p.Required) > 0 {
		nested := jsonSchemaValue(Schema{Properties: p.Properties, Required: p.Required})
		for k, v := range nested {
			out[k] = v
		}
	}
	return out
}
