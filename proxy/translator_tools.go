package proxy

import (
	"encoding/json"
	"strconv"
	"strings"
)

const maxToolDescLen = 10237

func convertClaudeTools(tools []ClaudeTool) ([]KiroToolWrapper, map[string]string) {
	if len(tools) == 0 {
		return nil, nil
	}

	result := make([]KiroToolWrapper, 0, len(tools))
	nameMap := make(map[string]string)
	for _, tool := range tools {
		desc := tool.Description
		if len([]rune(desc)) > maxToolDescLen {
			desc = truncateRunes(desc, maxToolDescLen) + "..."
		}
		sanitized := shortenToolName(sanitizeToolName(tool.Name))
		if sanitized != tool.Name {
			nameMap[sanitized] = tool.Name
		}
		w := KiroToolWrapper{}
		w.ToolSpecification.Name = sanitized
		w.ToolSpecification.Description = normalizeToolDesc(desc, sanitized)
		w.ToolSpecification.InputSchema = InputSchema{JSON: ensureObjectSchema(tool.InputSchema)}
		result = append(result, w)
	}
	result = compressToolsIfNeeded(result)
	return result, nameMap
}

// ensureObjectSchema 确保工具 schema 顶层是 object，并清理 Kiro 不接受的字段。
func ensureObjectSchema(schema interface{}) interface{} {
	m, ok := schema.(map[string]interface{})
	if !ok {
		return map[string]interface{}{"type": "object"}
	}
	cleaned := cloneSchemaMap(m)
	cleanSchema(cleaned)
	if _, hasType := cleaned["type"]; !hasType {
		cleaned["type"] = "object"
	}
	return cleaned
}

func cloneSchemaMap(m map[string]interface{}) map[string]interface{} {
	cloned := make(map[string]interface{}, len(m))
	for k, v := range m {
		cloned[k] = cloneSchemaValue(v)
	}
	return cloned
}

func cloneSchemaValue(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		return cloneSchemaMap(val)
	case []interface{}:
		cloned := make([]interface{}, 0, len(val))
		for _, item := range val {
			cloned = append(cloned, cloneSchemaValue(item))
		}
		return cloned
	default:
		return v
	}
}

// cleanSchema 递归清理会导致 Kiro 400 的 schema 字段。
func cleanSchema(m map[string]interface{}) {
	delete(m, "additionalProperties")

	// required 必须是非空数组，否则 Kiro 会报 Improperly formed request。
	if req, exists := m["required"]; exists {
		switch arr := req.(type) {
		case nil:
			delete(m, "required")
		case []interface{}:
			if len(arr) == 0 {
				delete(m, "required")
			}
		case []string:
			if len(arr) == 0 {
				delete(m, "required")
			}
		default:
			delete(m, "required")
		}
	}

	for _, v := range m {
		switch val := v.(type) {
		case map[string]interface{}:
			cleanSchema(val)
		case []interface{}:
			for _, item := range val {
				if sub, ok := item.(map[string]interface{}); ok {
					cleanSchema(sub)
				}
			}
		}
	}
}

func normalizeToolDesc(desc, name string) string {
	if strings.TrimSpace(desc) != "" {
		return desc
	}
	return "Tool: " + name
}

func buildClaudeToolSchemaMap(tools []ClaudeTool) map[string]interface{} {
	if len(tools) == 0 {
		return nil
	}
	schemas := make(map[string]interface{}, len(tools))
	for _, tool := range tools {
		name := shortenToolName(sanitizeToolName(tool.Name))
		schemas[name] = ensureObjectSchema(tool.InputSchema)
	}
	return schemas
}

func buildOpenAIToolSchemaMap(tools []OpenAITool) map[string]interface{} {
	if len(tools) == 0 {
		return nil
	}
	schemas := make(map[string]interface{}, len(tools))
	for _, tool := range tools {
		if tool.Type != "function" {
			continue
		}
		name := shortenToolName(tool.Function.Name)
		schemas[name] = ensureObjectSchema(tool.Function.Parameters)
	}
	if len(schemas) == 0 {
		return nil
	}
	return schemas
}

func sanitizeToolInput(input map[string]interface{}, schema interface{}) map[string]interface{} {
	if input == nil {
		return nil
	}
	coerced, ok := sanitizeValueForSchema(cloneSchemaValue(input), schema).(map[string]interface{})
	if !ok {
		coerced = cloneSchemaValue(input).(map[string]interface{})
	}
	return normalizeCommonToolInputScalars(coerced)
}

func sanitizeValueForSchema(value interface{}, schema interface{}) interface{} {
	schemaMap, ok := schema.(map[string]interface{})
	if !ok {
		return value
	}

	schemaType := schemaTypeSet(schemaMap["type"])
	if coerced, changed := coerceStructuredStringForSchema(value, schemaType); changed {
		value = coerced
	}

	if coerced, changed := coerceScalarForSchema(value, schemaMap); changed {
		return coerced
	}

	if props, ok := schemaMap["properties"].(map[string]interface{}); ok && (len(schemaType) == 0 || schemaType["object"] || schemaType[""]) {
		if obj, ok := value.(map[string]interface{}); ok {
			for key, childSchema := range props {
				if childValue, exists := obj[key]; exists {
					obj[key] = sanitizeValueForSchema(childValue, childSchema)
				}
			}
		}
	}

	if items, ok := schemaMap["items"]; ok && schemaType["array"] {
		if arr, ok := value.([]interface{}); ok {
			for i, item := range arr {
				arr[i] = sanitizeValueForSchema(item, items)
			}
		}
	}

	return value
}

func normalizeCommonToolInputScalars(input map[string]interface{}) map[string]interface{} {
	if input == nil {
		return nil
	}
	for key, value := range input {
		if child, ok := value.(map[string]interface{}); ok {
			input[key] = normalizeCommonToolInputScalars(child)
			continue
		}
		if arr, ok := value.([]interface{}); ok {
			for i, item := range arr {
				if child, ok := item.(map[string]interface{}); ok {
					arr[i] = normalizeCommonToolInputScalars(child)
				}
			}
			input[key] = arr
			continue
		}
		if isCommonBooleanToolField(key) {
			if parsed, ok := coerceCommonBoolean(value); ok {
				input[key] = parsed
			}
			continue
		}
		if isCommonArrayToolField(key) {
			if parsed, ok := parseJSONArrayString(value); ok {
				input[key] = parsed
			}
			continue
		}
		if !isCommonNumericToolField(key) {
			continue
		}
		if parsed, ok := coerceCommonNumber(value); ok {
			input[key] = parsed
		}
	}
	return input
}

func isCommonNumericToolField(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "context", "offset", "limit", "line", "line_number", "lineno", "lineoffset", "head_limit", "duration", "start", "end", "timeout", "max_length", "count", "n", "pageno", "-c", "-a", "-b":
		return true
	default:
		return false
	}
}

func isCommonBooleanToolField(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "-i", "-n", "multiline", "replace_all", "compress", "preview", "stream", "dangerouslydisablesandbox", "run_in_background", "block", "enabled", "disabled":
		return true
	default:
		return false
	}
}

func isCommonArrayToolField(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "todos", "items", "files", "paths", "tool_uses", "questions", "options":
		return true
	default:
		return false
	}
}

func coerceCommonBoolean(value interface{}) (interface{}, bool) {
	coerced, changed := coerceBoolean(value)
	if changed {
		if b, ok := coerced.(bool); ok {
			return b, true
		}
	}
	return value, false
}

func coerceCommonNumber(value interface{}) (interface{}, bool) {
	switch v := value.(type) {
	case string:
		parsed, ok := parseNumberString(v)
		if !ok {
			return value, false
		}
		if parsed == float64(int(parsed)) {
			return int(parsed), true
		}
		return parsed, true
	case float64:
		if v == float64(int(v)) {
			return int(v), true
		}
	}
	return value, false
}

func coerceStructuredStringForSchema(value interface{}, types map[string]bool) (interface{}, bool) {
	if types["array"] {
		return parseJSONArrayString(value)
	}
	if types["object"] {
		return parseJSONObjectString(value)
	}
	return value, false
}

func parseJSONArrayString(value interface{}) ([]interface{}, bool) {
	s, ok := value.(string)
	if !ok {
		return nil, false
	}
	s = strings.TrimSpace(s)
	if s == "" || !strings.HasPrefix(s, "[") {
		return nil, false
	}
	var parsed []interface{}
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return nil, false
	}
	return parsed, true
}

func parseJSONObjectString(value interface{}) (map[string]interface{}, bool) {
	s, ok := value.(string)
	if !ok {
		return nil, false
	}
	s = strings.TrimSpace(s)
	if s == "" || !strings.HasPrefix(s, "{") {
		return nil, false
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return nil, false
	}
	return parsed, true
}

func coerceScalarForSchema(value interface{}, schema map[string]interface{}) (interface{}, bool) {
	types := schemaTypeSet(schema["type"])
	if len(types) == 0 {
		return value, false
	}
	if types["boolean"] {
		return coerceBoolean(value)
	}
	if types["integer"] {
		return coerceInteger(value)
	}
	if types["number"] {
		return coerceNumber(value)
	}
	if types["string"] {
		return coerceString(value)
	}
	return value, false
}

func schemaTypeSet(raw interface{}) map[string]bool {
	result := make(map[string]bool)
	switch v := raw.(type) {
	case string:
		result[v] = true
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				result[s] = true
			}
		}
	case []string:
		for _, item := range v {
			result[item] = true
		}
	}
	return result
}

func coerceBoolean(value interface{}) (interface{}, bool) {
	switch v := value.(type) {
	case bool:
		return value, false
	case string:
		normalized := strings.ToLower(strings.TrimSpace(v))
		switch normalized {
		case "", "false", "0", "no", "off", "disabled", "disable":
			return false, true
		case "true", "1", "yes", "on", "enabled", "enable":
			return true, true
		default:
			// Kiro often emits a string payload for CLI-style boolean flags
			// (for example "-n": "..."), while Claude Code requires a bool.
			// Treat any non-empty, non-false string as presence of the flag.
			return true, true
		}
	case float64:
		return v != 0, true
	case int:
		return v != 0, true
	case nil:
		return false, true
	default:
		return value, false
	}
}

func coerceInteger(value interface{}) (interface{}, bool) {
	switch v := value.(type) {
	case string:
		if parsed, ok := parseIntegerString(v); ok {
			return parsed, true
		}
	case float64:
		return int(v), true
	}
	return value, false
}

func coerceNumber(value interface{}) (interface{}, bool) {
	if s, ok := value.(string); ok {
		if parsed, ok := parseNumberString(s); ok {
			return parsed, true
		}
	}
	return value, false
}

func coerceString(value interface{}) (interface{}, bool) {
	switch v := value.(type) {
	case bool:
		if v {
			return "true", true
		}
		return "false", true
	case float64:
		return formatFloat(v), true
	}
	return value, false
}

func parseIntegerString(value string) (int, bool) {
	num, ok := parseNumberString(value)
	if !ok {
		return 0, false
	}
	return int(num), true
}

func parseNumberString(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	num, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return num, true
}

func formatFloat(value float64) string {
	b, _ := json.Marshal(value)
	return string(b)
}

// sanitizeToolName normalizes a tool name to characters the Kiro API accepts.
// Kiro tool names must be pure camelCase (no underscores or dashes).
// Separators (_, -, and multi-underscore namespace prefixes) are converted to camelCase boundaries.
func sanitizeToolName(name string) string {
	// Split on underscores and dashes, including multi-underscore namespace prefixes.
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '_' || r == '-'
	})
	if len(parts) == 0 {
		return "tool"
	}
	// Build camelCase: first part lowercase start, rest capitalize first letter
	var b strings.Builder
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == 0 {
			b.WriteString(strings.ToLower(part[:1]) + part[1:])
		} else {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	result := b.String()
	if result == "" {
		return "tool"
	}
	return result
}

func shortenToolName(name string) string {
	if len(name) <= 64 {
		return name
	}
	// MCP tools: mcp__server__tool -> mcp__tool
	if strings.HasPrefix(name, "mcp__") {
		lastIdx := strings.LastIndex(name, "__")
		if lastIdx > 5 {
			shortened := "mcp__" + name[lastIdx+2:]
			if len(shortened) <= 64 {
				return shortened
			}
		}
	}
	return truncateToByteLimitAtRune(name, 64)
}

// truncateToByteLimitAtRune truncates s to the largest prefix that fits within
// maxBytes bytes without splitting a multi-byte UTF-8 character. The 64-byte
// cap is an upstream hard limit, so we stay within it while avoiding invalid
// UTF-8 that plain byte slicing (s[:64]) could produce.
func truncateToByteLimitAtRune(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	b := 0
	for _, r := range s {
		size := len(string(r))
		if b+size > maxBytes {
			break
		}
		b += size
	}
	return s[:b]
}

func convertOpenAITools(tools []OpenAITool) []KiroToolWrapper {
	if len(tools) == 0 {
		return nil
	}

	result := make([]KiroToolWrapper, 0, len(tools))
	for _, tool := range tools {
		if tool.Type != "function" {
			continue
		}
		// Skip tool specs with no name. A nameless tool reaching the upstream
		// triggers HTTP 400 "Improperly formed request"; this can happen if a
		// client sends an unrecognized tool shape that parses to an empty name.
		if strings.TrimSpace(tool.Function.Name) == "" {
			continue
		}
		desc := tool.Function.Description
		if len([]rune(desc)) > maxToolDescLen {
			desc = truncateRunes(desc, maxToolDescLen) + "..."
		}
		wrapper := KiroToolWrapper{}
		wrapper.ToolSpecification.Name = shortenToolName(tool.Function.Name)
		wrapper.ToolSpecification.Description = normalizeToolDesc(desc, wrapper.ToolSpecification.Name)
		wrapper.ToolSpecification.InputSchema = InputSchema{JSON: ensureObjectSchema(tool.Function.Parameters)}
		result = append(result, wrapper)
	}
	return compressToolsIfNeeded(result)
}
