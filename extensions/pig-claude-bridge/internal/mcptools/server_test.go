package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
)

var nestedSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"question": map[string]any{"type": "string", "description": "Question to ask"},
		"options": map[string]any{"type": "array", "items": map[string]any{
			"type":       "object",
			"properties": map[string]any{"label": map[string]any{"type": "string", "description": "Short label"}, "detail": map[string]any{"type": "string"}},
			"required":   []any{"label"},
		}},
		"config": map[string]any{"type": "object",
			"properties": map[string]any{"mode": map[string]any{"type": "string", "enum": []any{"single", "multi"}}, "retries": map[string]any{"type": "integer"}},
			"required":   []any{"mode"}},
		"either": map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "number"}}},
	},
	"required": []any{"question", "options"},
}

type client struct {
	t      *testing.T
	s      *Server
	nextID int
}

func newClient(t *testing.T, tools []ToolDef) *client {
	t.Helper()
	s, err := NewServer("custom-tools", tools)
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, s: s}
	c.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1.0.0"}})
	if _, has, err := s.Handle(context.Background(), json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil || has {
		t.Fatalf("initialized: has=%v err=%v", has, err)
	}
	return c
}

type reply struct {
	Result map[string]any `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *client) requestCtx(ctx context.Context, method string, params any) reply {
	c.t.Helper()
	c.nextID++
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params})
	out, has, err := c.s.Handle(ctx, raw)
	if err != nil || !has {
		c.t.Fatalf("%s: has=%v err=%v", method, has, err)
	}
	var r reply
	if err := json.Unmarshal(out, &r); err != nil {
		c.t.Fatal(err)
	}
	return r
}

func (c *client) request(method string, params any) reply {
	return c.requestCtx(context.Background(), method, params)
}

func (c *client) callTool(name, toolUseID string, args map[string]any) reply {
	p := map[string]any{"name": name, "arguments": args}
	if toolUseID != "" {
		p["_meta"] = map[string]any{"claudecode/toolUseId": toolUseID}
	}
	return c.request("tools/call", p)
}

func textHandler(text string, isErr bool) func(context.Context, string, map[string]any) (convert.McpResult, error) {
	return func(context.Context, string, map[string]any) (convert.McpResult, error) {
		return convert.McpResult{Content: convert.McpContent{{"type": "text", "text": text}}, IsError: isErr}, nil
	}
}

func listTools(t *testing.T, tools []ToolDef) []any {
	return newClient(t, tools).request("tools/list", map[string]any{}).Result["tools"].([]any)
}

func TestNewServerRejectsNonObjectSchema(t *testing.T) {
	_, err := NewServer("custom-tools", []ToolDef{{Name: "bad_tool", InputSchema: map[string]any{"type": "string"}}})
	if err == nil || err.Error() != `bad_tool: MCP tool parameters must be an object schema, got {"type":"string"}` {
		t.Fatalf("err %v", err)
	}
	_, err = NewServer("custom-tools", []ToolDef{{Name: "nil_tool"}})
	if err == nil || err.Error() != "nil_tool: MCP tool parameters must be an object schema, got undefined" {
		t.Fatalf("err %v", err)
	}
}

func TestSchemaAdvertisement(t *testing.T) {
	listed := listTools(t, []ToolDef{{Name: "ask_user_question", Description: "Ask the user a question", InputSchema: nestedSchema, Handler: textHandler("ok", false)}})[0].(map[string]any)["inputSchema"].(map[string]any)
	props := listed["properties"].(map[string]any)

	t.Run("preserves top-level properties and required", func(t *testing.T) {
		if !reflect.DeepEqual(keys(props), []string{"config", "either", "options", "question"}) || !reflect.DeepEqual(listed["required"], []any{"question", "options"}) {
			t.Fatalf("got %v", listed)
		}
		if props["question"].(map[string]any)["description"] != "Question to ask" {
			t.Fatal("description lost")
		}
	})
	t.Run("preserves object properties nested in an array", func(t *testing.T) {
		item := props["options"].(map[string]any)["items"].(map[string]any)
		if !reflect.DeepEqual(keys(item["properties"].(map[string]any)), []string{"detail", "label"}) || !reflect.DeepEqual(item["required"], []any{"label"}) {
			t.Fatalf("got %v", item)
		}
	})
	t.Run("preserves nested object properties, enums and required", func(t *testing.T) {
		cfg := props["config"].(map[string]any)
		if !reflect.DeepEqual(keys(cfg["properties"].(map[string]any)), []string{"mode", "retries"}) || !reflect.DeepEqual(cfg["required"], []any{"mode"}) {
			t.Fatalf("got %v", cfg)
		}
		if !reflect.DeepEqual(cfg["properties"].(map[string]any)["mode"].(map[string]any)["enum"], []any{"single", "multi"}) {
			t.Fatal("enum lost")
		}
	})
	t.Run("preserves anyOf branches", func(t *testing.T) {
		want := []any{map[string]any{"type": "string"}, map[string]any{"type": "number"}}
		if !reflect.DeepEqual(props["either"].(map[string]any)["anyOf"], want) {
			t.Fatalf("got %v", props["either"])
		}
	})
}

func TestCombinatorDescriptions(t *testing.T) {
	list := func(schema map[string]any) map[string]any {
		return listTools(t, []ToolDef{{Name: "union_tool", Description: "d", InputSchema: schema}})[0].(map[string]any)
	}
	t.Run("anyOf", func(t *testing.T) {
		tool := list(map[string]any{
			"type": "object", "required": []any{"operation"},
			"properties": map[string]any{"operation": map[string]any{}, "input": map[string]any{}},
			"anyOf": []any{
				map[string]any{"properties": map[string]any{"operation": map[string]any{}, "baseRef": map[string]any{}}},
				map[string]any{"properties": map[string]any{"operation": map[string]any{}}, "required": []any{"input"}},
			},
		})
		// Go maps lose key order, so a branch named by its keys lists them sorted (the TypeScript lists insertion order).
		want := "Input constraint: Provide parameters for at least one of: (baseRef, operation) or (input).\n\nd"
		if tool["description"] != want {
			t.Fatalf("got %q", tool["description"])
		}
	})
	t.Run("allOf", func(t *testing.T) {
		tool := list(map[string]any{"type": "object", "allOf": []any{map[string]any{"properties": map[string]any{"b": map[string]any{}}, "required": []any{"b"}}}})
		if !strings.HasPrefix(tool["description"].(string), "Input constraint: all listed parameters apply together") {
			t.Fatalf("got %q", tool["description"])
		}
	})
	t.Run("oneOf of $defs refs", func(t *testing.T) {
		tool := list(map[string]any{"type": "object",
			"$defs": map[string]any{
				"byId":   map[string]any{"properties": map[string]any{"id": map[string]any{}}, "required": []any{"id"}},
				"byName": map[string]any{"properties": map[string]any{"name": map[string]any{}}, "required": []any{"name"}},
			},
			"oneOf": []any{map[string]any{"$ref": "#/$defs/byId"}, map[string]any{"$ref": "#/$defs/byName"}}})
		if tool["description"] != "Input constraint: Provide parameters for exactly one of: (id) or (name).\n\nd" {
			t.Fatalf("got %q", tool["description"])
		}
	})
	t.Run("no combinator leaves the description alone", func(t *testing.T) {
		if list(map[string]any{"type": "object"})["description"] != "d" {
			t.Fatal("description changed")
		}
	})
}

func TestToolsListUsesFlattenedSchema(t *testing.T) {
	tool := listTools(t, []ToolDef{{Name: "u", Description: "d", InputSchema: map[string]any{
		"type": "object", "anyOf": []any{map[string]any{"properties": map[string]any{"a": map[string]any{}}}},
	}}})[0].(map[string]any)
	schema := tool["inputSchema"].(map[string]any)
	if _, ok := schema["anyOf"]; ok || tool["name"] != "u" || !reflect.DeepEqual(keys(schema["properties"].(map[string]any)), []string{"a"}) {
		t.Fatalf("got %v", tool)
	}
}

func TestToolInvocation(t *testing.T) {
	type call struct{ name, id string }
	var mu sync.Mutex
	var calls []call
	record := func(name, text string, isErr bool) func(context.Context, string, map[string]any) (convert.McpResult, error) {
		return func(_ context.Context, id string, _ map[string]any) (convert.McpResult, error) {
			mu.Lock()
			calls = append(calls, call{name, id})
			mu.Unlock()
			return textHandler(text, isErr)(nil, id, nil)
		}
	}
	tools := func() []ToolDef {
		calls = nil
		return []ToolDef{
			{Name: "alpha", Description: "a", InputSchema: map[string]any{"type": "object"}, Handler: record("alpha", "from alpha", false)},
			{Name: "beta", Description: "b", InputSchema: map[string]any{"type": "object"}, Handler: record("beta", "from beta", true)},
		}
	}

	t.Run("routes tools/call to the matching handler", func(t *testing.T) {
		c := newClient(t, tools())
		r := c.callTool("beta", "toolu_b", map[string]any{"x": "hi"})
		if !reflect.DeepEqual(calls, []call{{"beta", "toolu_b"}}) || r.Result["isError"] != true {
			t.Fatalf("calls %v result %v", calls, r.Result)
		}
		if r.Result["content"].([]any)[0].(map[string]any)["text"] != "from beta" {
			t.Fatalf("result %v", r.Result)
		}
	})
	t.Run("hands the handler the tool_use id, not one inferred from call order", func(t *testing.T) {
		c := newClient(t, tools())
		c.callTool("beta", "toolu_second", nil)
		c.callTool("alpha", "toolu_first", nil)
		if !reflect.DeepEqual(calls, []call{{"beta", "toolu_second"}, {"alpha", "toolu_first"}}) {
			t.Fatalf("calls %v", calls)
		}
	})
	t.Run("does not leak internal fields into the tool result", func(t *testing.T) {
		c := newClient(t, []ToolDef{{Name: "alpha", InputSchema: map[string]any{"type": "object"}, Handler: func(context.Context, string, map[string]any) (convert.McpResult, error) {
			return convert.McpResult{Content: convert.McpContent{{"type": "text", "text": "ok"}}, ToolCallID: "toolu_x"}, nil
		}}})
		r := c.callTool("alpha", "toolu_x", nil)
		// isError is omitted when false (the Go result cannot tell false from undefined).
		if !reflect.DeepEqual(keys(r.Result), []string{"content"}) {
			t.Fatalf("keys %v", keys(r.Result))
		}
	})
	t.Run("invokes the handler even when arguments do not match the schema", func(t *testing.T) {
		called := false
		c := newClient(t, []ToolDef{{Name: "strict", InputSchema: map[string]any{"type": "object", "required": []any{"count"}, "additionalProperties": false},
			Handler: func(_ context.Context, _ string, args map[string]any) (convert.McpResult, error) {
				called = args["count"] == "not-a-number"
				return textHandler("ran", false)(nil, "", nil)
			}}})
		r := c.callTool("strict", "toolu_y", map[string]any{"count": "not-a-number", "extra": 1})
		if !called || r.Result["content"].([]any)[0].(map[string]any)["text"] != "ran" {
			t.Fatalf("called=%v result=%v", called, r.Result)
		}
	})
}

func TestToolsCallMissingMetaErrors(t *testing.T) {
	ran := false
	c := newClient(t, []ToolDef{{Name: "alpha", InputSchema: map[string]any{"type": "object"}, Handler: func(context.Context, string, map[string]any) (convert.McpResult, error) {
		ran = true
		return convert.McpResult{}, nil
	}}})
	r := c.callTool("alpha", "", nil)
	want := `alpha: tools/call is missing _meta["claudecode/toolUseId"] — cannot pair the result with its tool call`
	if r.Error == nil || r.Error.Code != -32603 || r.Error.Message != want || ran {
		t.Fatalf("error %+v ran=%v", r.Error, ran)
	}
	if r := c.callTool("nope", "toolu_1", nil); r.Error == nil || r.Error.Message != "Unknown tool: nope" {
		t.Fatalf("error %+v", r.Error)
	}
}

func TestInitializeResultShape(t *testing.T) {
	s, _ := NewServer("custom-tools", nil)
	req := `{"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1.310"}},"jsonrpc":"2.0","id":0}`
	out, has, err := s.Handle(context.Background(), json.RawMessage(req))
	want := `{"id":0,"jsonrpc":"2.0","result":{"capabilities":{"tools":{}},"protocolVersion":"2025-11-25","serverInfo":{"name":"custom-tools","version":"1.0.0"}}}`
	if err != nil || !has || !jsonEqual(t, string(out), want) {
		t.Fatalf("has=%v err=%v out=%s", has, err, out)
	}
	for _, c := range []struct{ asked, want string }{{"2024-11-05", "2024-11-05"}, {"1999-01-01", "2025-11-25"}} {
		out, _, _ := s.Handle(context.Background(), json.RawMessage(`{"jsonrpc":"2.0","id":"s","method":"initialize","params":{"protocolVersion":"`+c.asked+`"}}`))
		var r struct {
			ID     string
			Result struct{ ProtocolVersion string }
		}
		json.Unmarshal(out, &r)
		if r.ID != "s" || r.Result.ProtocolVersion != c.want {
			t.Fatalf("asked %s got %s", c.asked, out)
		}
	}
}

func jsonEqual(t *testing.T, a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		t.Fatalf("bad json %s / %s", a, b)
	}
	return reflect.DeepEqual(x, y)
}

func TestOtherMessages(t *testing.T) {
	s, _ := NewServer("custom-tools", nil)
	ctx := context.Background()
	out, has, _ := s.Handle(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":7,"method":"ping"}`))
	if !has || !jsonEqual(t, string(out), `{"jsonrpc":"2.0","id":7,"result":{}}`) {
		t.Fatalf("ping %s", out)
	}
	out, has, _ = s.Handle(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":"x","method":"resources/list"}`))
	if !has || !jsonEqual(t, string(out), `{"jsonrpc":"2.0","id":"x","error":{"code":-32601,"message":"Method not found"}}`) {
		t.Fatalf("unknown %s", out)
	}
	if _, has, err := s.Handle(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`)); has || err != nil {
		t.Fatalf("notification has=%v err=%v", has, err)
	}
	if _, _, err := s.Handle(ctx, json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed JSON must error")
	}
}

func TestToolsCallHandlerErrorBecomesJSONRPCError(t *testing.T) {
	c := newClient(t, []ToolDef{{Name: "a", InputSchema: map[string]any{"type": "object"}, Handler: func(context.Context, string, map[string]any) (convert.McpResult, error) {
		return convert.McpResult{}, errors.New("boom")
	}}})
	if r := c.callTool("a", "t", nil); r.Error == nil || r.Error.Code != -32603 || r.Error.Message != "boom" {
		t.Fatalf("%+v", r.Error)
	}
}

func TestToolsCallBlocksUntilHandlerReturns(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	c := newClient(t, []ToolDef{{Name: "slow", InputSchema: map[string]any{"type": "object"}, Handler: func(ctx context.Context, id string, _ map[string]any) (convert.McpResult, error) {
		started <- id
		select {
		case <-release:
		case <-ctx.Done():
			return convert.McpResult{}, ctx.Err()
		}
		return textHandler("done "+id, false)(ctx, id, nil)
	}}})

	// Two parallel calls both park inside their handlers; Handle holds no lock meanwhile.
	var wg sync.WaitGroup
	texts := make(chan string, 2)
	for _, id := range []string{"toolu_1", "toolu_2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": "slow", "_meta": map[string]any{"claudecode/toolUseId": id}}})
			out, _, _ := c.s.Handle(context.Background(), raw)
			texts <- string(out)
		}()
	}
	<-started
	<-started
	select {
	case got := <-texts:
		t.Fatalf("replied before handler returned: %s", got)
	case <-time.After(50 * time.Millisecond):
	}
	if r := c.request("ping", nil); r.Result == nil {
		t.Fatal("ping blocked behind parked calls")
	}
	close(release)
	wg.Wait()
	close(texts)
	n := 0
	for got := range texts {
		n++
		if !strings.Contains(got, `"id":"toolu_`) || !strings.Contains(got, "done toolu_") {
			t.Fatalf("reply %s", got)
		}
	}
	if n != 2 {
		t.Fatalf("replies %d", n)
	}
}

func TestToolsCallCancellationReachesHandlerAndSuppressesReply(t *testing.T) {
	entered := make(chan struct{})
	c := newClient(t, []ToolDef{{Name: "slow", InputSchema: map[string]any{"type": "object"}, Handler: func(ctx context.Context, _ string, _ map[string]any) (convert.McpResult, error) {
		close(entered)
		<-ctx.Done()
		return convert.McpResult{}, ctx.Err()
	}}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() {
		_, has, _ := c.s.Handle(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow","_meta":{"claudecode/toolUseId":"t"}}}`))
		done <- has
	}()
	<-entered
	cancel()
	if has := <-done; has {
		t.Fatal("a cancelled request must not be answered")
	}
}
