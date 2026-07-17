package proxy

// assistantCollector accumulates Assistant Events for non-stream responses.
type assistantCollector struct {
	Text       string
	Reasoning  string
	Tools      []normalizedToolCall
	InputTok   int
	OutputTok  int
	Credits    float64
	ContextPct float64
	HasContext bool
	StopReason string
	Err        error
	Completed  bool
}

func (c *assistantCollector) handle(ev assistantEvent) {
	switch ev.kind {
	case assistantKindPlainText:
		c.Text += ev.text
	case assistantKindReasoning:
		c.Reasoning += ev.text
	case assistantKindToolCall:
		c.Tools = append(c.Tools, ev.tool)
	case assistantKindTelemetry:
		if ev.hasCredits {
			c.Credits = ev.credits
		}
		if ev.hasContext {
			c.ContextPct = ev.contextPct
			c.HasContext = true
		}
	case assistantKindCompletion:
		c.Completed = true
		c.StopReason = ev.stopReason
		c.InputTok = ev.finalIn
		c.OutputTok = ev.finalOut
		if ev.finalCred > 0 {
			c.Credits = ev.finalCred
		}
	case assistantKindModelOutputError:
		c.Err = ev.err
	}
}
