package proxy

// declaredToolsFromPayload builds the request-scoped Declared Tool set after
// translation has populated ToolNameMap / ToolSchemas on the Kiro payload.
func declaredToolsFromPayload(payload *KiroPayload) (*declaredToolSet, error) {
	if payload == nil {
		return buildDeclaredToolSet(nil, nil)
	}
	return buildDeclaredToolSet(payload.ToolNameMap, payload.ToolSchemas)
}
