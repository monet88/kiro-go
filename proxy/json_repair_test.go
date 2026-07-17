package proxy

import "testing"

func TestRepairStructuredToolJSONAllowedRepairs(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"newline in string", `{"q":"a
b"}`, `a\nb`},
		{"trailing comma object", `{"q":"x",}`, "x"},
		{"trailing comma array", `{"a":[1,],}`, ""},
		{"unterminated string", `{"q":"hello`, "hello"},
		{"unclosed object", `{"q":"x"`, "x"},
		{"unclosed array", `{"a":[1`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj, err := repairStructuredToolJSON(tc.raw)
			if err != nil {
				t.Fatalf("repair: %v", err)
			}
			if tc.want != "" {
				if got, _ := obj["q"].(string); got != tc.want && tc.name != "trailing comma array" && tc.name != "unclosed array" {
					// flexible checks below
				}
			}
			if tc.name == "newline in string" && obj["q"] != "a\nb" {
				t.Fatalf("got %#v", obj["q"])
			}
			if tc.name == "trailing comma object" && obj["q"] != "x" {
				t.Fatalf("got %#v", obj)
			}
			if tc.name == "unterminated string" && obj["q"] != "hello" {
				t.Fatalf("got %#v", obj)
			}
			if tc.name == "unclosed object" && obj["q"] != "x" {
				t.Fatalf("got %#v", obj)
			}
			if tc.name == "trailing comma array" {
				arr, _ := obj["a"].([]interface{})
				if len(arr) != 1 {
					t.Fatalf("got %#v", obj)
				}
			}
			if tc.name == "unclosed array" {
				arr, _ := obj["a"].([]interface{})
				if len(arr) != 1 {
					t.Fatalf("got %#v", obj)
				}
			}
		})
	}
}

func TestRepairStructuredToolJSONRejects(t *testing.T) {
	// Negative balance
	if _, err := repairStructuredToolJSON(`{"q":"x"}}`); err == nil {
		t.Fatal("expected negative balance rejection")
	}
	// Non-object
	if _, err := repairStructuredToolJSON(`[1,2]`); err == nil {
		t.Fatal("expected non-object rejection")
	}
	// Still invalid tokens
	if _, err := repairStructuredToolJSON(`{"q": hello}`); err == nil {
		t.Fatal("expected invalid token rejection")
	}
}
