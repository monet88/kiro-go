package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// callbackTrace records the ordered KiroStreamCallback surface used by production
// handlers. These fixtures are the PR A parity baseline: binary AWS Event Stream
// bytes in, ordered callback-visible effects out.
type callbackTrace struct {
	Steps   []string
	Tools   []KiroToolUse
	Credits float64
	InTok   int
	OutTok  int
	Done    bool
	Err     error
}

func (t *callbackTrace) callback() *KiroStreamCallback {
	return &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			kind := "text"
			if isThinking {
				kind = "reasoning"
			}
			t.Steps = append(t.Steps, fmt.Sprintf("%s:%s", kind, text))
		},
		OnToolUse: func(toolUse KiroToolUse) {
			// Deep-copy input so later mutations cannot rewrite the trace.
			cp := KiroToolUse{
				ToolUseID: toolUse.ToolUseID,
				Name:      toolUse.Name,
				Input:     cloneMap(toolUse.Input),
			}
			t.Tools = append(t.Tools, cp)
			raw, _ := json.Marshal(cp.Input)
			t.Steps = append(t.Steps, fmt.Sprintf("tool:%s:%s:%s", cp.ToolUseID, cp.Name, string(raw)))
		},
		OnCredits: func(credits float64) {
			t.Credits = credits
			t.Steps = append(t.Steps, fmt.Sprintf("credits:%.4f", credits))
		},
		OnContextUsage: func(percentage float64) {
			t.Steps = append(t.Steps, fmt.Sprintf("context:%.4f", percentage))
		},
		OnComplete: func(inputTokens, outputTokens int) {
			t.InTok = inputTokens
			t.OutTok = outputTokens
			t.Done = true
			t.Steps = append(t.Steps, fmt.Sprintf("complete:%d:%d", inputTokens, outputTokens))
		},
		OnError: func(err error) {
			t.Err = err
			t.Steps = append(t.Steps, fmt.Sprintf("error:%v", err))
		},
	}
}

func cloneMap(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return map[string]interface{}{}
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func joinFrames(frames ...[]byte) []byte {
	return bytes.Join(frames, nil)
}

func TestKiroCallbackParityFixtures(t *testing.T) {
	type wantTool struct {
		idExact   string
		idPrefix  string
		name      string
		inputJSON string
	}
	type want struct {
		steps       []string
		stepPrefix  []string // matched by HasPrefix when non-empty; empty entry uses exact steps[i]
		tools       []wantTool
		credits     float64
		inTok       int
		outTok      int
		done        bool
		errContains string
		errIs       error
	}

	cases := []struct {
		name string
		body func(t *testing.T) io.Reader
		ctx  func(t *testing.T) (context.Context, context.CancelFunc)
		want want
	}{
		{
			name: "cumulative overlapping duplicate and true-delta assistant content",
			body: func(t *testing.T) io.Reader {
				return bytes.NewReader(joinFrames(
					// true first chunk
					awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "Hel"}),
					// cumulative full rewrite
					awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "Hello"}),
					// exact duplicate of longest snapshot
					awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "Hello"}),
					// overlapping rewrite that shares a suffix
					awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "lo world"}),
					// true delta after longest path was not cumulative; previous becomes "lo world"
					// next cumulative extension is not applicable; emit independent chunk
					awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "lo world!"}),
				))
			},
			want: want{
				steps: []string{
					"text:Hel",
					"text:lo",
					// duplicate ignored
					// overlap from "Hello" -> "lo world" yields " world" via suffix overlap on "lo"
					// Wait: normalizeChunk("lo world", prev="Hello"):
					//  not equal, not prefix either way.
					//  maxOverlap: suffix of prev matching prefix of chunk.
					//  "Hello" suffixes vs "lo world" prefixes: "lo" matches -> overlap 2 -> " world"
					"text: world",
					// prev becomes "lo world"; next "lo world!" has prefix prev -> "!"
					"text:!",
					"complete:0:0",
				},
				done: true,
			},
		},
		{
			name: "explicit reasoning with cumulative chunks",
			body: func(t *testing.T) io.Reader {
				return bytes.NewReader(joinFrames(
					awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "think"}),
					awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "thinking"}),
					awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "answer"}),
				))
			},
			want: want{
				steps: []string{
					"reasoning:think",
					"reasoning:ing",
					"text:answer",
					"complete:0:0",
				},
				done: true,
			},
		},
		{
			name: "multipart tool input with initial id",
			body: func(t *testing.T) io.Reader {
				return bytes.NewReader(joinFrames(
					awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
						"toolUseId": "toolu_1",
						"name":      "lookup",
						"input":     `{"q":"he`,
					}),
					awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
						"toolUseId": "toolu_1",
						"name":      "lookup",
						"input":     `llo"}`,
						"stop":      true,
					}),
				))
			},
			want: want{
				steps: []string{
					`tool:toolu_1:lookup:{"q":"hello"}`,
					"complete:0:0",
				},
				tools: []wantTool{{
					idExact:   "toolu_1",
					name:      "lookup",
					inputJSON: `{"q":"hello"}`,
				}},
				done: true,
			},
		},
		{
			name: "multipart tool input without initial id upgrades to real id",
			body: func(t *testing.T) io.Reader {
				return bytes.NewReader(joinFrames(
					awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
						"name":  "lookup",
						"input": `{"q":"a`,
					}),
					awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
						"toolUseId": "toolu_real",
						"name":      "lookup",
						"input":     `"}`,
						"stop":      true,
					}),
				))
			},
			want: want{
				// generated id is replaced before stop, so the emitted tool carries the real id.
				steps: []string{
					`tool:toolu_real:lookup:{"q":"a"}`,
					"complete:0:0",
				},
				tools: []wantTool{{
					idExact:   "toolu_real",
					name:      "lookup",
					inputJSON: `{"q":"a"}`,
				}},
				done: true,
			},
		},
		{
			name: "generated tool id when never provided",
			body: func(t *testing.T) io.Reader {
				return bytes.NewReader(joinFrames(
					awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
						"name":  "lookup",
						"input": `{"q":"x"}`,
						"stop":  true,
					}),
				))
			},
			want: want{
				stepPrefix: []string{
					"tool:toolu_", // exact suffix checked via tools
					"complete:0:0",
				},
				tools: []wantTool{{
					idPrefix:  "toolu_",
					name:      "lookup",
					inputJSON: `{"q":"x"}`,
				}},
				done: true,
			},
		},
		{
			name: "usage credit context and late usage after content",
			body: func(t *testing.T) io.Reader {
				return bytes.NewReader(joinFrames(
					awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hi"}),
					awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.5}),
					awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 12.5}),
					// early usage snapshot
					awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
						"content": "",
						"usage": map[string]interface{}{
							"inputTokens":  10,
							"outputTokens": 2,
						},
					}),
					// late usage supersedes earlier snapshot
					awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{
						"usage": 0.25,
						"tokenUsage": map[string]interface{}{
							"inputTokens":  15,
							"outputTokens": 7,
						},
					}),
					awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
						"stopReason": "END_TURN",
					}),
				))
			},
			want: want{
				// empty assistant content is ignored; metadata is currently no-op for callbacks
				steps: []string{
					"text:hi",
					"context:12.5000",
					"credits:1.7500",
					"complete:15:7",
				},
				credits: 1.75,
				inTok:   15,
				outTok:  7,
				done:    true,
			},
		},
		{
			name: "unknown event type is ignored",
			body: func(t *testing.T) io.Reader {
				return bytes.NewReader(joinFrames(
					awsEventStreamFrame(t, "totallyUnknownEvent", map[string]interface{}{"foo": "bar"}),
					awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "ok"}),
				))
			},
			want: want{
				steps: []string{"text:ok", "complete:0:0"},
				done:  true,
			},
		},
		{
			name: "eof finalizes open tool without explicit stop",
			body: func(t *testing.T) io.Reader {
				return bytes.NewReader(joinFrames(
					awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
						"toolUseId": "toolu_open",
						"name":      "lookup",
						"input":     `{"server":"ida"}`,
					}),
				))
			},
			want: want{
				steps: []string{
					`tool:toolu_open:lookup:{"server":"ida"}`,
					"complete:0:0",
				},
				tools: []wantTool{{
					idExact:   "toolu_open",
					name:      "lookup",
					inputJSON: `{"server":"ida"}`,
				}},
				done: true,
			},
		},
		{
			name: "context cancellation returns ctx error before completion",
			body: func(t *testing.T) io.Reader {
				// Provide a valid frame so the loop has work if cancellation is late;
				// the cancelled context must abort before any complete callback.
				return bytes.NewReader(joinFrames(
					awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "should-not-matter"}),
					awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "more"}),
				))
			},
			ctx: func(t *testing.T) (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			want: want{
				done:  false,
				errIs: context.Canceled,
			},
		},
		{
			name: "truncated frame returns read error",
			body: func(t *testing.T) io.Reader {
				// Valid prelude advertising more bytes than are present.
				frame := awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "x"})
				// Drop the last few payload/crc bytes to force a short read on the body.
				if len(frame) < 20 {
					t.Fatalf("fixture frame too small: %d", len(frame))
				}
				return bytes.NewReader(frame[:len(frame)-6])
			},
			want: want{
				done:        false,
				errContains: "EOF",
			},
		},
		{
			name: "object-shaped tool input replaces buffer",
			body: func(t *testing.T) io.Reader {
				return bytes.NewReader(joinFrames(
					awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
						"toolUseId": "toolu_obj",
						"name":      "lookup",
						"input":     `{"q":"stale"}`,
					}),
					awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
						"toolUseId": "toolu_obj",
						"name":      "lookup",
						"input":     map[string]interface{}{"q": "fresh"},
						"stop":      true,
					}),
				))
			},
			want: want{
				steps: []string{
					`tool:toolu_obj:lookup:{"q":"fresh"}`,
					"complete:0:0",
				},
				tools: []wantTool{{
					idExact:   "toolu_obj",
					name:      "lookup",
					inputJSON: `{"q":"fresh"}`,
				}},
				done: true,
			},
		},
		{
			name: "unparseable tool input becomes empty object",
			body: func(t *testing.T) io.Reader {
				return bytes.NewReader(joinFrames(
					awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
						"toolUseId": "toolu_bad",
						"name":      "lookup",
						"input":     `{"q":`,
						"stop":      true,
					}),
				))
			},
			want: want{
				steps: []string{
					`tool:toolu_bad:lookup:{}`,
					"complete:0:0",
				},
				tools: []wantTool{{
					idExact:   "toolu_bad",
					name:      "lookup",
					inputJSON: `{}`,
				}},
				done: true,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var cancel context.CancelFunc
			if tc.ctx != nil {
				ctx, cancel = tc.ctx(t)
			}
			if cancel != nil {
				defer cancel()
			}

			trace := &callbackTrace{}
			err := parseEventStream(ctx, tc.body(t), trace.callback())
			if tc.want.errIs != nil {
				if !errors.Is(err, tc.want.errIs) {
					t.Fatalf("error: got %v want %v", err, tc.want.errIs)
				}
			} else if tc.want.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want.errContains) {
					t.Fatalf("error: got %v want substring %q", err, tc.want.errContains)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.want.done != trace.Done {
				t.Fatalf("done: got %v want %v; steps=%v", trace.Done, tc.want.done, trace.Steps)
			}
			if tc.want.done {
				if trace.InTok != tc.want.inTok || trace.OutTok != tc.want.outTok {
					t.Fatalf("tokens: got %d/%d want %d/%d", trace.InTok, trace.OutTok, tc.want.inTok, tc.want.outTok)
				}
				if trace.Credits != tc.want.credits {
					t.Fatalf("credits: got %v want %v", trace.Credits, tc.want.credits)
				}
			}

			if len(tc.want.stepPrefix) > 0 {
				if len(trace.Steps) != len(tc.want.stepPrefix) {
					t.Fatalf("steps len: got %v want prefixes %v", trace.Steps, tc.want.stepPrefix)
				}
				for i, p := range tc.want.stepPrefix {
					if !strings.HasPrefix(trace.Steps[i], p) {
						t.Fatalf("step[%d]: got %q want prefix %q", i, trace.Steps[i], p)
					}
				}
			} else if tc.want.steps != nil {
				if len(trace.Steps) != len(tc.want.steps) {
					t.Fatalf("steps:\n got %v\nwant %v", trace.Steps, tc.want.steps)
				}
				for i := range tc.want.steps {
					if trace.Steps[i] != tc.want.steps[i] {
						t.Fatalf("step[%d]: got %q want %q\n full got %v\n full want %v", i, trace.Steps[i], tc.want.steps[i], trace.Steps, tc.want.steps)
					}
				}
			}

			if len(tc.want.tools) != len(trace.Tools) {
				t.Fatalf("tools count: got %d want %d (%v)", len(trace.Tools), len(tc.want.tools), trace.Tools)
			}
			for i, wt := range tc.want.tools {
				got := trace.Tools[i]
				if wt.idExact != "" && got.ToolUseID != wt.idExact {
					t.Fatalf("tool[%d] id: got %q want %q", i, got.ToolUseID, wt.idExact)
				}
				if wt.idPrefix != "" && !strings.HasPrefix(got.ToolUseID, wt.idPrefix) {
					t.Fatalf("tool[%d] id: got %q want prefix %q", i, got.ToolUseID, wt.idPrefix)
				}
				if got.Name != wt.name {
					t.Fatalf("tool[%d] name: got %q want %q", i, got.Name, wt.name)
				}
				raw, err := json.Marshal(got.Input)
				if err != nil {
					t.Fatalf("marshal tool input: %v", err)
				}
				if string(raw) != wt.inputJSON {
					t.Fatalf("tool[%d] input: got %s want %s", i, raw, wt.inputJSON)
				}
			}
		})
	}
}
