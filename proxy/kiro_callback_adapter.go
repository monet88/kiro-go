package proxy

import (
	"encoding/json"
	"kiro-go/logger"
	"strings"

	"github.com/google/uuid"
)

// kiroCallbackAdapter renders Kiro Semantic Events onto the legacy
// KiroStreamCallback surface so PR A preserves production handler behavior.
type kiroCallbackAdapter struct {
	callback     *KiroStreamCallback
	inputTokens  int
	outputTokens int
	totalCredits float64
	tool         *adapterToolState
}

type adapterToolState struct {
	toolUseID   string
	name        string
	inputBuffer strings.Builder
}

func newKiroCallbackAdapter(callback *KiroStreamCallback) *kiroCallbackAdapter {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}
	return &kiroCallbackAdapter{callback: callback}
}

func (a *kiroCallbackAdapter) handle(ev kiroSemanticEvent) {
	switch ev.kind {
	case kiroKindPlainText:
		if a.callback.OnText != nil && ev.text != "" {
			a.callback.OnText(ev.text, false)
		}
	case kiroKindReasoning:
		if a.callback.OnText != nil && ev.text != "" {
			a.callback.OnText(ev.text, true)
		}
	case kiroKindToolStart:
		a.tool = &adapterToolState{toolUseID: ev.toolID, name: ev.toolName}
	case kiroKindToolInput:
		if a.tool == nil {
			a.tool = &adapterToolState{toolUseID: ev.toolID, name: ev.toolName}
		} else {
			// Real ID may upgrade a generated one between start and stop.
			if ev.toolID != "" {
				a.tool.toolUseID = ev.toolID
			}
			if ev.toolName != "" {
				a.tool.name = ev.toolName
			}
		}
		if ev.replace {
			a.tool.inputBuffer.Reset()
		}
		a.tool.inputBuffer.WriteString(ev.toolInput)
	case kiroKindToolStop:
		if a.tool == nil {
			a.tool = &adapterToolState{toolUseID: ev.toolID, name: ev.toolName}
		} else {
			if ev.toolID != "" {
				a.tool.toolUseID = ev.toolID
			}
			if ev.toolName != "" {
				a.tool.name = ev.toolName
			}
		}
		a.finishTool()
	case kiroKindUsage:
		a.inputTokens = ev.inputTokens
		a.outputTokens = ev.outputTokens
	case kiroKindCredit:
		a.totalCredits += ev.credits
	case kiroKindContextUsage:
		if a.callback.OnContextUsage != nil {
			a.callback.OnContextUsage(ev.contextPct)
		}
	case kiroKindStopMeta:
		// Intentionally not exposed on the legacy callback surface.
	case kiroKindError:
		if a.callback.OnError != nil && ev.err != nil {
			a.callback.OnError(ev.err)
		}
	case kiroKindTerminal:
		// Safety net: open tool should already have been stopped by the extractor.
		if a.tool != nil {
			a.finishTool()
		}
		if a.callback.OnCredits != nil && a.totalCredits > 0 {
			a.callback.OnCredits(a.totalCredits)
		}
		if a.callback.OnComplete != nil {
			a.callback.OnComplete(a.inputTokens, a.outputTokens)
		}
	}
}

func (a *kiroCallbackAdapter) finishTool() {
	if a.tool == nil || a.tool.name == "" || a.callback.OnToolUse == nil {
		a.tool = nil
		return
	}
	id := a.tool.toolUseID
	if id == "" {
		id = "toolu_" + uuid.New().String()
	}
	var input map[string]interface{}
	if a.tool.inputBuffer.Len() > 0 {
		if err := json.Unmarshal([]byte(a.tool.inputBuffer.String()), &input); err != nil {
			logger.Warnf("[KiroAPI] tool %q input JSON parse failed (%d bytes): %v", a.tool.name, a.tool.inputBuffer.Len(), err)
		}
	}
	if input == nil {
		input = make(map[string]interface{})
	}
	a.callback.OnToolUse(KiroToolUse{
		ToolUseID: id,
		Name:      a.tool.name,
		Input:     input,
	})
	a.tool = nil
}
