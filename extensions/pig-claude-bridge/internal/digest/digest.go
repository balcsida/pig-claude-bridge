// Package digest hashes a pi conversation message's conversion-visible content,
// ignoring metadata (pi-claude-bridge src/message-digests.ts). The digests are
// persisted in the bridge's session state, so the encoding is pinned to the
// TypeScript's byte for byte (tests/fixtures/message-digests.json).
package digest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
)

// writeString frames a string by its JavaScript length (UTF-16 code units) and
// writes it as UTF-16LE, as the TypeScript does.
func writeString(h hash.Hash, s string) {
	units := utf16.Encode([]rune(s))
	fmt.Fprintf(h, "s%d:", len(units))
	buf := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(buf[2*i:], u)
	}
	h.Write(buf)
}

// jsNumber renders a float as JavaScript's String(n): decimal when 1e-6 <= |n| < 1e21,
// exponent form otherwise, with the shortest digits that round-trip.
func jsNumber(f float64) string {
	if f == 0 {
		return "0" // -0 too
	}
	if abs := math.Abs(f); abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	mant, exp, _ := strings.Cut(s, "e")
	sign := exp[:1]
	exp = strings.TrimLeft(exp[1:], "0")
	return mant + "e" + sign + exp
}

// writeJSON canonicalises a JSON value: object keys sorted, numbers as JavaScript
// renders them. It errors on a value JSON cannot carry (the TypeScript throws).
func writeJSON(h hash.Hash, v any) error {
	switch x := v.(type) {
	case nil:
		h.Write([]byte("null;"))
	case string:
		writeString(h, x)
	case bool:
		if x {
			h.Write([]byte("b1"))
		} else {
			h.Write([]byte("b0"))
		}
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Errorf("tool arguments must contain finite JSON numbers")
		}
		n := jsNumber(x)
		fmt.Fprintf(h, "d%d:%s", len(n), n)
	case int:
		return writeJSON(h, float64(x))
	case int64:
		return writeJSON(h, float64(x))
	case []any:
		h.Write([]byte("a["))
		for _, item := range x {
			if err := writeJSON(h, item); err != nil {
				return err
			}
		}
		h.Write([]byte("]a"))
	case []map[string]any:
		items := make([]any, len(x))
		for i, m := range x {
			items[i] = m
		}
		return writeJSON(h, items)
	case map[string]any:
		h.Write([]byte("o{"))
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		for _, k := range keys {
			writeString(h, k)
			if err := writeJSON(h, x[k]); err != nil {
				return err
			}
		}
		h.Write([]byte("}o"))
	default:
		return fmt.Errorf("unsupported tool argument value: %T", v)
	}
	return nil
}

// utf16Less orders strings by UTF-16 code units, as JavaScript's default sort does.
func utf16Less(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

type part struct{ typ, text, data, mimeType string }

func parts(content any) ([]part, bool) {
	var raw []any
	switch c := content.(type) {
	case []any:
		raw = c
	case []map[string]any:
		for _, m := range c {
			raw = append(raw, m)
		}
	default:
		return nil, false
	}
	out := make([]part, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		out = append(out, part{typ: str(m["type"]), text: str(m["text"]), data: str(m["data"]), mimeType: str(m["mimeType"])})
	}
	return out, true
}

func str(v any) string { s, _ := v.(string); return s }

func writeUser(h hash.Hash, m map[string]any) {
	if s, ok := m["content"].(string); ok {
		h.Write([]byte("user:string:"))
		if s == "" {
			s = "[empty]"
		}
		writeString(h, s)
		return
	}
	content, isArray := parts(m["content"])
	if !isArray {
		h.Write([]byte("user:fallback:"))
		writeString(h, "[empty]")
		return
	}
	hasParts := false
	for _, b := range content {
		hasParts = hasParts || (b.typ == "text" && b.text != "") || (b.typ == "image" && b.data != "" && b.mimeType != "")
	}
	if !hasParts {
		h.Write([]byte("user:fallback:"))
		writeString(h, "[image]")
		return
	}
	h.Write([]byte("user:blocks["))
	for _, b := range content {
		if b.typ == "text" && b.text != "" {
			h.Write([]byte("text:"))
			writeString(h, b.text)
		} else if b.typ == "image" && b.data != "" && b.mimeType != "" {
			h.Write([]byte("image:"))
			writeString(h, b.mimeType)
			writeString(h, b.data)
		}
	}
	h.Write([]byte("]user"))
}

func writeToolResult(h hash.Hash, m map[string]any) {
	h.Write([]byte("toolResult:"))
	writeString(h, str(m["toolCallId"]))
	if isError, _ := m["isError"].(bool); isError {
		h.Write([]byte("b1"))
	} else {
		h.Write([]byte("b0"))
	}
	content, _ := parts(m["content"])
	hasImage := false
	for _, b := range content {
		hasImage = hasImage || (b.typ == "image" && b.data != "" && b.mimeType != "")
	}
	if !hasImage {
		h.Write([]byte("tool-content:text:"))
		writeString(h, convert.MessageContentToText(m["content"]))
		return
	}
	h.Write([]byte("tool-content:blocks["))
	for _, b := range content {
		switch {
		case b.typ == "text" && b.text != "":
			h.Write([]byte("text:"))
			writeString(h, b.text)
		case b.typ == "image" && b.data != "" && b.mimeType != "":
			h.Write([]byte("image:"))
			writeString(h, b.mimeType)
			writeString(h, b.data)
		case b.typ != "text" && b.typ != "image":
			h.Write([]byte("text:"))
			writeString(h, "["+b.typ+"]")
		}
	}
	h.Write([]byte("]tool-content"))
}

func replayableThinking(m, block map[string]any) bool {
	return str(block["type"]) == "thinking" && m["provider"] == convert.ProviderID && str(block["thinkingSignature"]) != ""
}

func writeAssistant(h hash.Hash, m map[string]any) error {
	var content []map[string]any
	switch c := m["content"].(type) {
	case []any:
		for _, r := range c {
			b, _ := r.(map[string]any)
			content = append(content, b)
		}
	case []map[string]any:
		content = c
	}
	if len(content) == 0 {
		h.Write([]byte("assistant:omitted"))
		return nil
	}
	converted := false
	for _, b := range content {
		converted = converted || (str(b["type"]) == "text" && str(b["text"]) != "") || str(b["type"]) == "toolCall" || replayableThinking(m, b)
	}
	if !converted {
		h.Write([]byte("assistant:placeholder"))
		return nil
	}
	h.Write([]byte("assistant:blocks["))
	for _, b := range content {
		switch {
		case str(b["type"]) == "text" && str(b["text"]) != "":
			h.Write([]byte("text:"))
			writeString(h, str(b["text"]))
		case replayableThinking(m, b):
			h.Write([]byte("thinking:"))
			writeString(h, str(b["thinking"]))
			writeString(h, str(b["thinkingSignature"]))
		case str(b["type"]) == "toolCall":
			h.Write([]byte("toolCall:"))
			writeString(h, str(b["id"]))
			writeString(h, str(b["name"]))
			args := b["arguments"]
			if args == nil {
				args = map[string]any{}
			}
			if err := writeJSON(h, args); err != nil {
				return err
			}
		}
	}
	h.Write([]byte("]assistant"))
	return nil
}

// Message hashes one non-system pi conversation message.
func Message(m map[string]any) (string, error) {
	h := sha256.New()
	switch m["role"] {
	case "user":
		writeUser(h, m)
	case "assistant":
		if err := writeAssistant(h, m); err != nil {
			return "", err
		}
	case "toolResult":
		writeToolResult(h, m)
	default:
		return "", fmt.Errorf("digest expects a non-system conversation message, got role %v", m["role"])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Messages returns one digest per message.
func Messages(messages []map[string]any) ([]string, error) {
	out := make([]string, len(messages))
	for i, m := range messages {
		d, err := Message(m)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		out[i] = d
	}
	return out, nil
}
