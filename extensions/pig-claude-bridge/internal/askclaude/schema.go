// Package askclaude is the deprecated, opt-in AskClaude tool: PiG's delegation of
// one question or task to Claude Code (port of askclaude-schema.ts, askclaude-ui.ts
// and the AskClaude parts of index.ts).
package askclaude

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
)

type Mode string

const (
	ModeFull Mode = "full"
	ModeRead Mode = "read"
	ModeNone Mode = "none"
)

type Defaults struct {
	Mode         Mode
	Isolated     bool
	AllowFull    bool
	AppendSkills bool
}

const (
	packageDefaultMode     = ModeRead
	packageDefaultIsolated = false
)

// ResolveDefaults applies the configured defaults. allowFullMode and appendSkills default to true.
func ResolveDefaults(c *config.AskClaudeConfig) Defaults {
	d := Defaults{Mode: packageDefaultMode, Isolated: packageDefaultIsolated, AllowFull: true, AppendSkills: true}
	if c == nil {
		return d
	}
	if c.AllowFullMode != nil {
		d.AllowFull = *c.AllowFullMode
	}
	if c.AppendSkills != nil {
		d.AppendSkills = *c.AppendSkills
	}
	if c.DefaultIsolated != nil {
		d.Isolated = *c.DefaultIsolated
	}
	if c.DefaultMode != nil {
		switch m := Mode(*c.DefaultMode); m {
		case ModeFull, ModeRead, ModeNone:
			d.Mode = m
		default:
			// Unrecognized value: the most restrictive mode, not the package default.
			d.Mode = ModeNone
		}
	}
	if !d.AllowFull && d.Mode == ModeFull {
		d.Mode = packageDefaultMode
	}
	return d
}

type unset struct{}

// Unset is the ResolveMode argument for a call that omitted mode: the TypeScript's
// undefined, as opposed to nil (null), which is invalid.
var Unset any = unset{}

// ResolveMode is the mode of a call: the default when mode is Unset, an error for anything
// but "read", "none" or an allowed "full".
func ResolveMode(mode any, d Defaults) (Mode, error) {
	if _, omitted := mode.(unset); omitted {
		return d.Mode, nil
	}
	m, _ := mode.(string)
	switch Mode(m) {
	case ModeFull, ModeRead, ModeNone:
	default:
		if mode == nil {
			mode = "null"
		}
		return "", fmt.Errorf("Invalid AskClaude mode: %v", mode)
	}
	if Mode(m) == ModeFull && !d.AllowFull {
		return "", errors.New("AskClaude full mode is disabled.")
	}
	return Mode(m), nil
}

func modeDescription(d Defaults) string {
	mark := func(m Mode) string {
		if m == d.Mode {
			return " (default)"
		}
		return ""
	}
	parts := []string{
		`"read"` + mark(ModeRead) + `: questions about the codebase — review, analysis, explain.`,
		`"none"` + mark(ModeNone) + `: general knowledge only (no file access).`,
	}
	if d.AllowFull {
		parts = append(parts, `"full"`+mark(ModeFull)+`: allows writing and bash execution (careful: runs without feedback to pi).`)
	}
	return strings.Join(parts, " ")
}

// BuildParams is the tool's parameter schema as JSON Schema.
func BuildParams(d Defaults) sdk.Schema {
	visibility := "By default Claude sees the full conversation history."
	trueDefault, falseDefault := "", " (default)"
	if d.Isolated {
		visibility = "By default Claude sees only this prompt (isolated session)."
		trueDefault, falseDefault = " (default)", ""
	}
	modeValues := []string{"read", "none"}
	if d.AllowFull {
		modeValues = []string{"read", "full", "none"}
	}
	return sdk.Schema{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{"type": "string", "description": "The question or task for Claude Code. " + visibility + " Don't research up front, let Claude explore."},
			"mode":   map[string]any{"type": "string", "enum": modeValues, "description": modeDescription(d)},
			"model":  map[string]any{"type": "string", "description": `Claude model (e.g. "opus", "sonnet", "haiku", or full ID). Defaults to "opus".`},
			"thinking": map[string]any{
				"type": "string", "enum": []string{"off", "minimal", "low", "medium", "high", "xhigh"},
				"description": "Thinking effort level. Omit to use Claude Code's default.",
			},
			"isolated": map[string]any{
				"type":        "boolean",
				"description": "When true" + trueDefault + ", Claude sees only this prompt (clean session). When false" + falseDefault + ", Claude sees the full conversation history.",
			},
		},
		"required": []string{"prompt"},
	}
}

// ToolDescription is override when set, else the generated text for the defaults.
func ToolDescription(d Defaults, override *string) string {
	if override != nil {
		return *override
	}
	const suffix = " Prefer to handle straightforward tasks yourself."
	if !d.AllowFull {
		middle := "Read-only — Claude Code can explore the codebase but not make changes."
		if d.Mode == ModeNone {
			middle = `Defaults to no file access — pass mode "read" to let Claude Code explore the codebase; it can never make changes.`
		}
		return "Delegate to Claude Code for a second opinion or analysis (code review, architecture questions, debugging theories). " + middle + suffix
	}
	middle := "Defaults to read-only mode — use full mode when the user wants to delegate a task that requires changes."
	switch d.Mode {
	case ModeFull:
		middle = `Defaults to full mode — Claude Code writes files and runs bash without feedback to pi; pass mode "read" to keep it to exploration.`
	case ModeNone:
		middle = `Defaults to no file access — pass mode "read" to let Claude Code explore the codebase, or "full" for a task that requires changes.`
	}
	return "Delegate to Claude Code for a second opinion or analysis (code review, architecture questions, debugging theories), or to autonomously handle a task. " + middle + suffix
}

// CallArgs are the optional arguments of one call; zero values mean omitted.
type CallArgs struct {
	Mode            Mode
	Model, Thinking string
	Isolated        *bool
}

// CallTags are the status-line tags for the non-default options of a call.
func CallTags(a CallArgs, d Defaults) []string {
	tags := []string{}
	mode := a.Mode
	if mode == "" {
		mode = d.Mode
	}
	if mode != packageDefaultMode || (a.Mode != "" && a.Mode != d.Mode) {
		tags = append(tags, "mode="+string(mode))
	}
	if a.Model != "" {
		tags = append(tags, "model="+a.Model)
	}
	if a.Thinking != "" {
		tags = append(tags, "thinking="+a.Thinking)
	}
	isolated := d.Isolated
	if a.Isolated != nil {
		isolated = *a.Isolated
	}
	if isolated != packageDefaultIsolated {
		tags = append(tags, "isolated")
	}
	return tags
}

// Claude Code tools no mode may use: they need a pi TUI or harness that a delegated run lacks.
// Other tools (Agent, SendMessage, RemoteTrigger, Tasks, ...) are intentionally not blocked.
var alwaysBlocked = []string{"AskUserQuestion", "EnterPlanMode", "ExitPlanMode", "ToolSearch", "ScheduleWakeup"}

// DisallowedTools are the Claude Code tools blocked in mode.
func DisallowedTools(mode Mode) []string {
	switch mode {
	case ModeFull:
		return slices.Clone(alwaysBlocked)
	case ModeRead:
		return append(slices.Clone(alwaysBlocked),
			"Write", "Edit", "Bash", "NotebookEdit",
			"EnterWorktree", "ExitWorktree", "CronCreate", "CronDelete", "TeamCreate", "TeamDelete")
	default:
		return append(slices.Clone(alwaysBlocked),
			"Read", "Write", "Edit", "Glob", "Grep", "Bash", "Agent",
			"NotebookEdit", "EnterWorktree", "ExitWorktree",
			"CronCreate", "CronDelete", "TeamCreate", "TeamDelete",
			"WebFetch", "WebSearch")
	}
}
