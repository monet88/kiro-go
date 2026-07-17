package proxy

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// declaredTool is one client-declared tool after request translation has
// established the canonical (client-visible) and upstream (sanitized) names.
type declaredTool struct {
	// CanonicalName is the original client-facing tool name.
	CanonicalName string
	// UpstreamName is the name sent to Kiro (may equal CanonicalName).
	UpstreamName string
	// Schema is the raw client schema document (map or nil).
	Schema any
	// Compiled is the request-scoped compiled schema. Nil only when Schema is
	// treated as a permissive object schema without full compilation.
	Compiled *jsonschema.Schema
	// RequireObject forces the tool input to be a JSON object even when the
	// schema is {} or lacks a top-level type.
	RequireObject bool
}

// declaredToolSet is request-scoped. Compiled once after translation and reused
// across Account attempts; never mutated by attempt-local normalizer state.
type declaredToolSet struct {
	byCanonical map[string]*declaredTool
	byUpstream  map[string]*declaredTool
	order       []*declaredTool
}

// errInvalidDeclaredTool is returned when a client-declared schema cannot be
// compiled under the repository schema policy (external $ref, compile error).
type errInvalidDeclaredTool struct {
	Name   string
	Reason string
}

func (e *errInvalidDeclaredTool) Error() string {
	if e == nil {
		return ""
	}
	if e.Name == "" {
		return "invalid declared tool schema: " + e.Reason
	}
	return fmt.Sprintf("invalid declared tool %q schema: %s", e.Name, e.Reason)
}

// buildDeclaredToolSet compiles every Declared Tool for a translated Kiro
// payload. nameMap maps upstream(sanitized)->canonical; schemas are keyed by
// the names present on the payload (typically upstream/sanitized names).
func buildDeclaredToolSet(nameMap map[string]string, schemas map[string]interface{}) (*declaredToolSet, error) {
	set := &declaredToolSet{
		byCanonical: make(map[string]*declaredTool),
		byUpstream:  make(map[string]*declaredTool),
	}
	if len(schemas) == 0 && len(nameMap) == 0 {
		return set, nil
	}

	// Collect every known tool identity from either map.
	seenUpstream := make(map[string]struct{})
	for upstream, canonical := range nameMap {
		if upstream == "" {
			continue
		}
		if canonical == "" {
			canonical = upstream
		}
		if err := set.addTool(upstream, canonical, schemas[upstream]); err != nil {
			return nil, err
		}
		seenUpstream[upstream] = struct{}{}
	}
	for upstream, schema := range schemas {
		if _, ok := seenUpstream[upstream]; ok {
			continue
		}
		// Schema present without name map entry: treat upstream as canonical.
		if err := set.addTool(upstream, upstream, schema); err != nil {
			return nil, err
		}
	}
	return set, nil
}

func (s *declaredToolSet) addTool(upstream, canonical string, schema any) error {
	if upstream == "" {
		return &errInvalidDeclaredTool{Reason: "empty tool name"}
	}
	if canonical == "" {
		canonical = upstream
	}
	if existing := s.byUpstream[upstream]; existing != nil {
		return nil
	}

	dt := &declaredTool{
		CanonicalName: canonical,
		UpstreamName:  upstream,
		Schema:        schema,
		RequireObject: true, // always require an object for tool inputs
	}

	if err := rejectExternalSchemaRefs(schema, nil); err != nil {
		return &errInvalidDeclaredTool{Name: canonical, Reason: err.Error()}
	}

	compiled, err := compileDeclaredToolSchema(upstream, schema)
	if err != nil {
		return &errInvalidDeclaredTool{Name: canonical, Reason: err.Error()}
	}
	dt.Compiled = compiled
	s.byUpstream[upstream] = dt
	s.byCanonical[canonical] = dt
	s.order = append(s.order, dt)
	return nil
}

func (s *declaredToolSet) lookup(name string) *declaredTool {
	if s == nil || name == "" {
		return nil
	}
	if dt := s.byUpstream[name]; dt != nil {
		return dt
	}
	return s.byCanonical[name]
}

func (s *declaredToolSet) lookupCanonical(name string) *declaredTool {
	if s == nil || name == "" {
		return nil
	}
	return s.byCanonical[name]
}

// deniedURLLoader rejects every remote/file load so schema compilation cannot
// perform network or filesystem I/O for external $ref targets.
type deniedURLLoader struct{}

func (deniedURLLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema reference resolution is disabled: %s", url)
}

func compileDeclaredToolSchema(name string, schema any) (*jsonschema.Schema, error) {
	// Empty / missing schema: compile a pure object schema so validation still
	// requires a JSON object while preserving all properties.
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	if m, ok := schema.(map[string]interface{}); ok && len(m) == 0 {
		schema = map[string]any{"type": "object"}
	}
	if m, ok := schema.(map[string]any); ok && len(m) == 0 {
		schema = map[string]any{"type": "object"}
	}

	// If the schema is a map without top-level type, leave it as-is for
	// compilation (JSON Schema treats missing type as unrestricted) but the
	// normalizer still enforces RequireObject before validation.
	doc, err := normalizeSchemaDoc(schema)
	if err != nil {
		return nil, err
	}

	c := jsonschema.NewCompiler()
	c.UseLoader(deniedURLLoader{})
	// Prefer draft 2020-12 default; documents may set $schema themselves.
	resourceURL := "tool://" + url.PathEscape(name) + "/schema.json"
	if err := c.AddResource(resourceURL, doc); err != nil {
		return nil, err
	}
	return c.Compile(resourceURL)
}

func normalizeSchemaDoc(schema any) (any, error) {
	switch v := schema.(type) {
	case map[string]any:
		return v, nil
	case bool:
		// JSON Schema boolean schemas are valid.
		return v, nil
	default:
		return nil, fmt.Errorf("schema must be a JSON object, got %T", schema)
	}
}

// rejectExternalSchemaRefs walks a schema document and rejects $ref values that
// would require network or filesystem resolution. Internal fragment references
// (#/..., #definitions/...) are allowed.
func rejectExternalSchemaRefs(v any, path []string) error {
	switch t := v.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok {
			if isExternalSchemaRef(ref) {
				return fmt.Errorf("external $ref %q is not allowed", ref)
			}
		}
		for k, child := range t {
			if err := rejectExternalSchemaRefs(child, append(path, k)); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range t {
			if err := rejectExternalSchemaRefs(child, append(path, fmt.Sprintf("%d", i))); err != nil {
				return err
			}
		}
	}
	return nil
}

func isExternalSchemaRef(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	// Pure fragment / same-document reference.
	if strings.HasPrefix(ref, "#") {
		return false
	}
	// Relative JSON pointer within the same resource (no scheme, no path escape to file).
	// Treat absolute URIs and file paths as external.
	if strings.Contains(ref, "://") {
		return true
	}
	if strings.HasPrefix(ref, "file:") || strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "\\\\") {
		return true
	}
	// Windows drive path.
	if len(ref) >= 3 && ref[1] == ':' && (ref[2] == '\\' || ref[2] == '/') {
		return true
	}
	// Relative path refs (./foo.json, ../bar, other.json) would trigger loader I/O.
	if strings.Contains(ref, ".json") || strings.Contains(ref, "/") || strings.Contains(ref, "\\") {
		// Allow only same-document fragments already handled above.
		// Anything with a path component is external for our policy.
		if !strings.HasPrefix(ref, "#") {
			return true
		}
	}
	return false
}

// validateToolInput runs schema-directed coercion (no field-name heuristics)
// then full JSON Schema validation against the compiled Declared Tool.
func (dt *declaredTool) validateToolInput(input map[string]interface{}) (map[string]interface{}, error) {
	if dt == nil {
		return nil, fmt.Errorf("undeclared tool")
	}
	if input == nil {
		input = map[string]interface{}{}
	}
	if dt.RequireObject {
		// Already a map by type; keep invariant explicit for callers.
	}

	// Schema-directed coercion only (reuse existing sanitizeValueForSchema path
	// without field-name heuristics). sanitizeToolInput also applies
	// normalizeCommonToolInputScalars which is field-name heuristic — skip that.
	coerced := input
	if dt.Schema != nil {
		if v, ok := sanitizeValueForSchema(cloneSchemaValue(input), dt.Schema).(map[string]interface{}); ok {
			coerced = v
		}
	}

	if dt.Compiled != nil {
		if err := dt.Compiled.Validate(coerced); err != nil {
			// Do not include raw input in the error text.
			return nil, fmt.Errorf("tool input failed schema validation")
		}
	}
	return coerced, nil
}
