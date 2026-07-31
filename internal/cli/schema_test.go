package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCommandResultConformsToDocumentedSchema(t *testing.T) {
	schemaData, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli-result.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(schemaData, &schema); err != nil {
		t.Fatal(err)
	}
	for _, result := range []commandResult{
		newCommandResult("cleanup"),
		{
			Version: 1, Command: "transcribe", Success: true, Provider: "local", Model: "large-v3", Language: "fr",
			TranscriptRaw: "brut", TranscriptFinal: "final", SegmentCount: 1,
			Segments: []commandSegment{{Index: 0, Provider: "local", Model: "large-v3", Language: "fr", Transcript: "brut"}},
			Warnings: []commandWarning{},
		},
		{
			Version: 1, Command: "dictate", Segments: []commandSegment{}, Warnings: []commandWarning{},
			Error: &commandError{Class: "audio_failed", Component: "audio/wav", Message: "format invalide"},
		},
	} {
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var document any
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		if err := validateSchemaValue(schema, document, schema); err != nil {
			t.Fatalf("%s: %v\n%s", result.Command, err, data)
		}
	}
}

// validateSchemaValue implements only the JSON Schema keywords used by the
// checked-in contract. Keeping it in tests avoids a runtime dependency.
func validateSchemaValue(schema map[string]any, value any, root map[string]any) error {
	if reference, ok := schema["$ref"].(string); ok {
		const prefix = "#/$defs/"
		if len(reference) <= len(prefix) || reference[:len(prefix)] != prefix {
			return fmt.Errorf("unsupported reference %q", reference)
		}
		definition, ok := root["$defs"].(map[string]any)[reference[len(prefix):]].(map[string]any)
		if !ok {
			return fmt.Errorf("missing definition %q", reference)
		}
		return validateSchemaValue(definition, value, root)
	}
	if variants, ok := schema["oneOf"].([]any); ok {
		matches := 0
		for _, variant := range variants {
			if validateSchemaValue(variant.(map[string]any), value, root) == nil {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("oneOf matched %d variants", matches)
		}
		return nil
	}
	if err := validateJSONType(schema["type"], value); err != nil {
		return err
	}
	if constant, ok := schema["const"]; ok && !reflect.DeepEqual(constant, value) {
		return fmt.Errorf("value %v differs from const %v", value, constant)
	}
	if enum, ok := schema["enum"].([]any); ok {
		matched := false
		for _, candidate := range enum {
			matched = matched || reflect.DeepEqual(candidate, value)
		}
		if !matched {
			return fmt.Errorf("value %v is outside enum", value)
		}
	}
	if minimum, ok := schema["minimum"].(float64); ok && value.(float64) < minimum {
		return fmt.Errorf("value %v is below minimum %v", value, minimum)
	}
	if object, ok := value.(map[string]any); ok {
		properties, _ := schema["properties"].(map[string]any)
		for _, required := range schemaList(schema["required"]) {
			if _, ok := object[required]; !ok {
				return fmt.Errorf("required property %q is missing", required)
			}
		}
		if schema["additionalProperties"] == false {
			for name := range object {
				if _, ok := properties[name]; !ok {
					return fmt.Errorf("unexpected property %q", name)
				}
			}
		}
		for name, child := range object {
			if childSchema, ok := properties[name].(map[string]any); ok {
				if err := validateSchemaValue(childSchema, child, root); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
			}
		}
	}
	if array, ok := value.([]any); ok {
		itemSchema := schema["items"].(map[string]any)
		for i, item := range array {
			if err := validateSchemaValue(itemSchema, item, root); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
	}
	return nil
}

func validateJSONType(schemaType any, value any) error {
	valid := false
	switch schemaType {
	case "object":
		_, valid = value.(map[string]any)
	case "array":
		_, valid = value.([]any)
	case "string":
		_, valid = value.(string)
	case "boolean":
		_, valid = value.(bool)
	case "integer":
		number, ok := value.(float64)
		valid = ok && number == float64(int64(number))
	case "null":
		valid = value == nil
	default:
		return fmt.Errorf("unsupported schema type %v", schemaType)
	}
	if !valid {
		return fmt.Errorf("value %v does not have type %v", value, schemaType)
	}
	return nil
}

func schemaList(value any) []string {
	items, _ := value.([]any)
	result := make([]string, len(items))
	for i, item := range items {
		result[i], _ = item.(string)
	}
	return result
}
