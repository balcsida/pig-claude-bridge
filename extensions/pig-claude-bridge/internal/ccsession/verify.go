package ccsession

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf16"
)

// VerifyWrittenSession checks that a saved session file round-trips and
// returns warnings (none when healthy); callers decide how to surface them.
func VerifyWrittenSession(jsonlPath, expectedSessionID string, expectedRecordCount int) []string {
	st, err := os.Stat(jsonlPath)
	if err != nil {
		return []string{fmt.Sprintf("file missing after save — path=%s err=%v", jsonlPath, err)}
	}
	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		return []string{fmt.Sprintf("file unreadable — path=%s size=%d err=%v", jsonlPath, st.Size(), err)}
	}
	content := string(data)
	var lines []string
	for _, l := range strings.Split(content, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) != expectedRecordCount {
		return []string{fmt.Sprintf("record count mismatch — expected=%d actual=%d path=%s bytes=%d",
			expectedRecordCount, len(lines), jsonlPath, len(utf16.Encode([]rune(content))))}
	}
	if len(lines) == 0 { // the library parses lines[0] regardless and reports it as malformed
		return []string{fmt.Sprintf("malformed JSONL — path=%s err=no records", jsonlPath)}
	}
	first, err := recordSessionID(lines[0])
	var last any
	if err == nil {
		last, err = recordSessionID(lines[len(lines)-1])
	}
	if err != nil {
		return []string{fmt.Sprintf("malformed JSONL — path=%s err=%v", jsonlPath, err)}
	}
	if first != expectedSessionID || last != expectedSessionID {
		return []string{fmt.Sprintf("sessionId drift — expected=%s first=%v last=%v", expectedSessionID, first, last)}
	}
	return nil
}

// recordSessionID reads a record's sessionId as the TypeScript does: a null record
// is malformed (reading its property throws), any other non-object has none.
func recordSessionID(line string) (any, error) {
	var rec any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, errors.New("Cannot read properties of null (reading 'sessionId')")
	}
	fields, _ := rec.(map[string]any)
	return fields["sessionId"], nil
}
