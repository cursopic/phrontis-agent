// Package mcp is a minimal read-only Model Context Protocol server over newline-delimited JSON-RPC 2.0 (stdio).
// It speaks the 2026-07-28 revision (stateless: server/discover, per-request _meta, resultType) and, for clients
// that still open with a handshake, the legacy `initialize` flow (2025-11-25 and earlier). It knows nothing about Kubernetes.
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// Latest is the spec revision this server implements.
	Latest = "2026-07-28"
	legacy = "2025-11-25"
)

// supported lists every protocol version this server accepts; the first is the newest.
var supported = []string{Latest, "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// Tool is one callable tool. Call gets the raw "arguments" object (may be empty) and returns any JSON-encodable value.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Call        func(args json.RawMessage) (any, error)
}

// Server serves Tools. Name and Version identify it to clients.
type Server struct {
	Name, Version, Instructions string
	Tools                       []Tool
}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads one JSON-RPC message per line from r and writes responses to w until r ends. Requests are handled one at a time.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			if out := s.handle(line); out != nil {
				if _, werr := w.Write(append(out, '\n')); werr != nil {
					return werr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// handle returns the encoded response, or nil for a notification.
func (s *Server) handle(line []byte) []byte {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return encode(nil, nil, &rpcError{-32700, "parse error: " + err.Error(), nil})
	}
	if req.Method == "" {
		return encode(req.ID, nil, &rpcError{-32600, "invalid request: no method", nil})
	}
	if len(req.ID) == 0 { // notification (notifications/initialized, notifications/cancelled, ...): never answered
		return nil
	}
	res, rerr := s.dispatch(req)
	return encode(req.ID, res, rerr)
}

func encode(id json.RawMessage, res any, rerr *rpcError) []byte {
	if id == nil {
		id = json.RawMessage("null")
	}
	if rerr == nil && res == nil {
		res = map[string]any{}
	}
	b, err := json.Marshal(response{"2.0", id, res, rerr})
	if err != nil { // a tool returned something JSON cannot hold
		b, _ = json.Marshal(response{"2.0", id, nil, &rpcError{-32603, "internal error: " + err.Error(), nil}})
	}
	return b
}

func (s *Server) dispatch(req request) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Version   string          `json:"protocolVersion"` // initialize
		Meta      map[string]any  `json:"_meta"`
	}
	if len(req.Params) > 0 && string(req.Params) != "null" {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params: " + err.Error(), nil}
		}
	}
	// A modern client names its version on every request; an unknown one gets the list we support.
	if v, ok := p.Meta["io.modelcontextprotocol/protocolVersion"].(string); ok && req.Method != "initialize" && !isSupported(v) {
		return nil, &rpcError{-32022, "Unsupported protocol version", map[string]any{"supported": supported, "requested": v}}
	}
	info := map[string]any{"name": s.Name, "version": s.Version}
	switch req.Method {
	case "initialize":
		v := legacy
		if isSupported(p.Version) && p.Version != Latest {
			v = p.Version
		}
		res := map[string]any{"protocolVersion": v, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": info}
		if s.Instructions != "" {
			res["instructions"] = s.Instructions
		}
		return res, nil
	case "server/discover":
		res := map[string]any{
			"resultType": "complete", "supportedVersions": supported, "capabilities": map[string]any{"tools": map[string]any{}},
			"_meta": map[string]any{"io.modelcontextprotocol/serverInfo": info}, "ttlMs": 3600000, "cacheScope": "public",
		}
		if s.Instructions != "" {
			res["instructions"] = s.Instructions
		}
		return res, nil
	case "ping": // removed in 2026-07-28, still sent by legacy clients
		return map[string]any{}, nil
	case "tools/list":
		tools := make([]map[string]any, len(s.Tools))
		for i, t := range s.Tools {
			tools[i] = map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema,
				"annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "openWorldHint": false}}
		}
		return map[string]any{"resultType": "complete", "tools": tools, "ttlMs": 3600000, "cacheScope": "public"}, nil
	case "tools/call":
		for _, t := range s.Tools {
			if t.Name != p.Name {
				continue
			}
			text, isErr := "", false
			if v, err := t.Call(p.Arguments); err != nil {
				text, isErr = err.Error(), true
			} else if b, err := json.Marshal(v); err != nil {
				text, isErr = "encode result: "+err.Error(), true
			} else {
				text = string(b)
			}
			return map[string]any{"resultType": "complete", "content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}, nil
		}
		return nil, &rpcError{-32602, fmt.Sprintf("unknown tool: %q", p.Name), nil}
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method, nil}
}

func isSupported(v string) bool {
	for _, s := range supported {
		if s == v {
			return true
		}
	}
	return false
}
