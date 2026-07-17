package proxy

import (
	"strings"
	"testing"
)

func TestBuildDeclaredToolSetCompilesInternalRef(t *testing.T) {
	schemas := map[string]interface{}{
		"lookup_tool": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"q": map[string]interface{}{"$ref": "#/$defs/q"},
			},
			"required": []interface{}{"q"},
			"$defs": map[string]interface{}{
				"q": map[string]interface{}{"type": "string"},
			},
		},
	}
	nameMap := map[string]string{"lookup_tool": "lookup"}
	set, err := buildDeclaredToolSet(nameMap, schemas)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	dt := set.lookup("lookup_tool")
	if dt == nil || dt.CanonicalName != "lookup" {
		t.Fatalf("lookup by upstream failed: %#v", dt)
	}
	if set.lookupCanonical("lookup") == nil {
		t.Fatal("lookup by canonical failed")
	}
	got, err := dt.validateToolInput(map[string]interface{}{"q": "hi"})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got["q"] != "hi" {
		t.Fatalf("unexpected input: %#v", got)
	}
	if _, err := dt.validateToolInput(map[string]interface{}{}); err == nil {
		t.Fatal("expected required-field failure")
	}
}

func TestBuildDeclaredToolSetRejectsExternalRef(t *testing.T) {
	schemas := map[string]interface{}{
		"lookup": map[string]interface{}{
			"$ref": "https://example.com/schema.json",
		},
	}
	_, err := buildDeclaredToolSet(nil, schemas)
	if err == nil {
		t.Fatal("expected external $ref rejection")
	}
	if !strings.Contains(err.Error(), "external") && !strings.Contains(err.Error(), "$ref") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildDeclaredToolSetRejectsFileRef(t *testing.T) {
	schemas := map[string]interface{}{
		"lookup": map[string]interface{}{
			"$ref": "file:///tmp/schema.json",
		},
	}
	if _, err := buildDeclaredToolSet(nil, schemas); err == nil {
		t.Fatal("expected file $ref rejection")
	}
}

func TestBuildDeclaredToolSetEmptySchemaRequiresObject(t *testing.T) {
	schemas := map[string]interface{}{
		"lookup": map[string]interface{}{},
	}
	set, err := buildDeclaredToolSet(map[string]string{"lookup": "Lookup"}, schemas)
	if err != nil {
		t.Fatalf("compile empty schema: %v", err)
	}
	dt := set.lookup("lookup")
	got, err := dt.validateToolInput(map[string]interface{}{"a": 1})
	if err != nil {
		t.Fatalf("object should pass: %v", err)
	}
	if got["a"] != 1 {
		t.Fatalf("values must be preserved: %#v", got)
	}
}

func TestBuildDeclaredToolSetSchemaDirectedCoercionNotFieldHeuristic(t *testing.T) {
	// Schema says offset is integer; string "10" should coerce.
	// Field-name heuristic alone would also coerce "offset", so also check a
	// non-heuristic field name "customCount".
	schemas := map[string]interface{}{
		"t": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"customCount": map[string]interface{}{"type": "integer"},
			},
		},
	}
	set, err := buildDeclaredToolSet(nil, schemas)
	if err != nil {
		t.Fatal(err)
	}
	dt := set.lookup("t")
	got, err := dt.validateToolInput(map[string]interface{}{"customCount": "7"})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// After schema-directed coercion, value should be numeric.
	switch v := got["customCount"].(type) {
	case int:
		if v != 7 {
			t.Fatalf("got %v", v)
		}
	case float64:
		if v != 7 {
			t.Fatalf("got %v", v)
		}
	default:
		t.Fatalf("expected numeric coercion, got %T %v", v, v)
	}
}

func TestBuildDeclaredToolSetSurvivesReuse(t *testing.T) {
	schemas := map[string]interface{}{
		"t": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"q": map[string]interface{}{"type": "string"},
			},
			"required": []interface{}{"q"},
		},
	}
	set, err := buildDeclaredToolSet(nil, schemas)
	if err != nil {
		t.Fatal(err)
	}
	dt := set.lookup("t")
	// Simulate two Account attempts reusing the same compiled schema.
	for i := 0; i < 2; i++ {
		if _, err := dt.validateToolInput(map[string]interface{}{"q": "x"}); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
}

func TestDeniedURLLoaderBlocksNetwork(t *testing.T) {
	var loader deniedURLLoader
	if _, err := loader.Load("https://example.com/schema.json"); err == nil {
		t.Fatal("expected load denial")
	}
}
