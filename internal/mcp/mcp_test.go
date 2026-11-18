package mcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func fake() *Server {
	return &Server{Name: "phrontis", Version: "test", Tools: []Tool{
		{Name: "echo", Description: "d", InputSchema: map[string]any{"type": "object"}, Call: func(a json.RawMessage) (any, error) {
			return map[string]any{"args": string(a)}, nil
		}},
		{Name: "boom", InputSchema: map[string]any{"type": "object"}, Call: func(json.RawMessage) (any, error) { return nil, errors.New("no cluster") }},
	}}
}

// run feeds lines to the server and returns the decoded response lines.
func run(t *testing.T, in ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := fake().Serve(strings.NewReader(strings.Join(in, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var res []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad output %q: %v", l, err)
		}
		res = append(res, m)
	}
	return res
}

func result(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	r, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", m)
	}
	return r
}

func errCode(t *testing.T, m map[string]any) float64 {
	t.Helper()
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error: %v", m)
	}
	return e["code"].(float64)
}

func TestInitializeLegacy(t *testing.T) {
	got := run(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if len(got) != 2 { // the notification gets no reply
		t.Fatalf("want 2 responses, got %d: %v", len(got), got)
	}
	r := result(t, got[0])
	if r["protocolVersion"] != "2025-06-18" || r["serverInfo"].(map[string]any)["name"] != "phrontis" {
		t.Fatalf("initialize: %v", r)
	}
	if _, ok := r["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("no tools capability: %v", r)
	}
	result(t, got[1])
}

func TestDiscoverAndVersion(t *testing.T) {
	got := run(t,
		`{"jsonrpc":"2.0","id":"d","method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1900-01-01"}}}`)
	r := result(t, got[0])
	if r["resultType"] != "complete" || r["supportedVersions"].([]any)[0] != Latest || got[0]["id"] != "d" {
		t.Fatalf("discover: %v", got[0])
	}
	if errCode(t, got[1]) != -32022 {
		t.Fatalf("want -32022: %v", got[1])
	}
}

func TestToolsList(t *testing.T) {
	r := result(t, run(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)[0])
	tools := r["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("tools: %v", tools)
	}
	if tools[0].(map[string]any)["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Fatal("tool not marked read-only")
	}
}

func TestToolsCall(t *testing.T) {
	got := run(t,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"a":1}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"boom"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nope"}}`)
	c := result(t, got[0])["content"].([]any)[0].(map[string]any)
	if c["type"] != "text" || c["text"] != `{"args":"{\"a\":1}"}` || result(t, got[0])["isError"] != false {
		t.Fatalf("call: %v", got[0])
	}
	r := result(t, got[1])
	if r["isError"] != true || r["content"].([]any)[0].(map[string]any)["text"] != "no cluster" {
		t.Fatalf("tool error: %v", got[1])
	}
	if errCode(t, got[2]) != -32602 {
		t.Fatalf("unknown tool: %v", got[2])
	}
}

func TestUnknownMethodAndMalformed(t *testing.T) {
	got := run(t,
		`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`,
		`{not json`,
		`{"jsonrpc":"2.0","id":8,"method":"ping"}`)
	if errCode(t, got[0]) != -32601 || got[0]["id"] != float64(7) {
		t.Fatalf("unknown method: %v", got[0])
	}
	if errCode(t, got[1]) != -32700 || got[1]["id"] != nil {
		t.Fatalf("malformed: %v", got[1])
	}
	result(t, got[2]) // the server keeps going after bad input
}
