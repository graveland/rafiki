package tools

import (
	"encoding/json"
	"testing"
)

func TestSchemaJSONRoundTrip(t *testing.T) {
	s := Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "path", Type: "string", Description: "The file path."},
			{Name: "limit", Type: "integer", Description: "Line limit."},
		},
		Required: []string{"path"},
	}

	raw := s.JSON()

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	typ, ok := got["type"].(string)
	if !ok || typ != "object" {
		t.Fatalf("type = %v, want object", got["type"])
	}

	props, ok := got["properties"].(map[string]any)
	if !ok {
		t.Fatal("no properties map")
	}
	path, ok := props["path"].(map[string]any)
	if !ok || path["type"] != "string" {
		t.Fatalf("path property = %v", props["path"])
	}
	limit, ok := props["limit"].(map[string]any)
	if !ok || limit["type"] != "integer" {
		t.Fatalf("limit property = %v", props["limit"])
	}

	req, ok := got["required"].([]any)
	if !ok || len(req) != 1 || req[0] != "path" {
		t.Fatalf("required = %v", got["required"])
	}
}

func TestSchemaNestedObject(t *testing.T) {
	s := Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{
				Name: "edits",
				Type: "array",
				Items: &Schema{
					Type: "object",
					Properties: []SchemaProperty{
						{Name: "old_string", Type: "string"},
						{Name: "new_string", Type: "string"},
					},
					Required: []string{"old_string", "new_string"},
				},
			},
		},
	}

	raw := s.JSON()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	props := got["properties"].(map[string]any)
	edits := props["edits"].(map[string]any)
	if edits["type"] != "array" {
		t.Fatalf("edits.type = %v", edits["type"])
	}
	items, ok := edits["items"].(map[string]any)
	if !ok {
		t.Fatal("no items")
	}
	itemProps, ok := items["properties"].(map[string]any)
	if !ok || len(itemProps) != 2 {
		t.Fatalf("items.properties = %v", itemProps)
	}
}

func TestSchemaEmptyJSON(t *testing.T) {
	s := Schema{}
	raw := s.JSON()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty schema should produce empty JSON object, got %v", got)
	}
}
