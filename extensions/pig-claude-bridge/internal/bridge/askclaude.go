package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/convert"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/models"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/promptcapture"
)

// AskRequest is one AskClaude call (index.ts promptAndWait's arguments).
type AskRequest struct {
	Prompt string
	// DisallowedTools are the Claude Code tools the mode blocks.
	DisallowedTools []string
	// SystemPrompt is the calling pi session's prompt, used to resolve the skills block.
	SystemPrompt string
	// AppendSkills false leaves the skills block out; nil and true include it.
	AppendSkills *bool
	// Model is a shortcut or id; "" means opus. Thinking is a pi level; "" and "off" send no effort.
	Model, Thinking string
	// Isolated runs without resume and without persisting a session file.
	Isolated bool
	// Context is the pi session's messages; a non-isolated call with a non-empty context
	// continues the remembered Claude Code session.
	Context     []map[string]any
	PiSessionID string
	// OnToolUse is told of every tool_use block with Claude Code's own tool name.
	OnToolUse func(id, name string, input any)
}

// PromptAndWait runs one AskClaude query on its own Claude Code process with the
// native tools the mode leaves enabled, and returns Claude's answer. Unlike
// IsolatedStream it resumes the calling pi session's remembered Claude Code session
// unless the call is isolated, and may take many turns. A panic becomes an error.
func (b *Bridge) PromptAndWait(ctx context.Context, host Host, r AskRequest) (text string, err error) {
	defer func() {
		if p := recover(); p != nil {
			b.debugf("askClaude: recovered from panic: %v\n%s", p, debug.Stack())
			text, err = "", fmt.Errorf("claude bridge internal error: %v", p)
		}
	}()
	if ctx.Err() != nil {
		return "", errors.New("Aborted")
	}
	cwd := b.sessionCwdFor(r.PiSessionID)
	requested := r.Model
	if requested == "" {
		requested = "opus"
	}
	modelID, cliModel := requested, requested
	if model, ok := models.ResolveModel(models.BuildModels(models.Catalog()), requested); ok {
		modelID = str(model["id"])
		cliModel = models.ClaudeCodeModelID(modelID, b.longContext)
	}

	// A non-isolated call continues the session the provider already keeps for this pi
	// session, or creates one from pi's context. It does not move the mirror's cursor,
	// so the next provider call sees the missed messages and rebuilds.
	resume := ""
	if !r.Isolated && len(r.Context) > 0 {
		if state := b.sessionStateFor(r.PiSessionID); state != nil {
			resume = state.SessionID
		} else {
			withPrompt := append(slices.Clone(r.Context), map[string]any{"role": "user", "content": r.Prompt, "timestamp": b.deps.Now().UnixMilli()})
			synced, err := b.syncSharedSession(host, r.PiSessionID, withPrompt, nil, cwd, cliModel)
			if err != nil {
				return "", err
			}
			resume = synced.SessionID
		}
	}

	// Resolved only when the answer would be used: a miss must not fail a call that has
	// skills switched off or no reader to open a skill file with.
	skillsBlock := ""
	if (r.AppendSkills == nil || *r.AppendSkills) && !slices.Contains(r.DisallowedTools, "Read") {
		capture, err := b.captures.ResolveOrDerive(r.SystemPrompt)
		if err != nil {
			return "", err
		}
		if capture != nil {
			skillsBlock, _ = convert.RenderSkillsBlock(promptcapture.CollectPromptSkills(capture), "native")
		}
	}

	effort := ""
	if r.Thinking != "off" {
		effort = reasoningToEffort[r.Thinking]
	}
	extraArgs := []ccproto.ExtraArg{{Key: "strict-mcp-config"}, {Key: "model", Value: &cliModel}}
	if effort != "" {
		summarized := "summarized"
		extraArgs = append(extraArgs, ccproto.ExtraArg{Key: "thinking-display", Value: &summarized})
	}
	exe, err := b.resolveExecutable(host)
	if err != nil {
		return "", err
	}
	b.debugf("askClaude: disallowed=%d model=%s cliModel=%s effort=%s isolated=%t resume=%s skills=%t promptLen=%d",
		len(r.DisallowedTools), modelID, cliModel, orDefault(effort), r.Isolated, orNone(short(resume)), skillsBlock != "", len([]rune(r.Prompt)))

	// Skills: [] suppresses Claude Code's own skill listing; pi's skills arrive through
	// the skills block, the only channel. The preset prompt is unconditional: without it
	// the child lacks the tool and permission guidance the bridge relies on.
	settings := config.ClaudeCodeSettings(b.deps.Config.Provider)
	settings["claudeMdExcludes"] = claudeMdExcludes
	debugFile, onStderr := b.cliDebugOptions("askclaude")
	opts := ccproto.Options{
		Executable: exe, Cwd: cwd, Effort: effort, Resume: resume,
		DisallowedTools: r.DisallowedTools, PermissionMode: "bypassPermissions",
		SettingSources: []string{"user", "project"}, SettingSourcesSet: true, Skills: []string{},
		Settings: settings, DebugFile: debugFile, AppendSystemPrompt: skillsBlock, ExtraArgs: extraArgs,
		NoSessionPersistence: r.Isolated,
	}
	// The process lives no longer than this call, so the call's abort also stops its start-up.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	client, err := b.deps.Spawn(runCtx, opts, ccproto.Handlers{OnStderr: onStderr})
	if err != nil {
		if ctx.Err() != nil {
			return "", errors.New("Aborted")
		}
		return "", fmt.Errorf("start Claude Code: %v", err)
	}
	var writer sync.WaitGroup
	writeErr := make(chan error, 1)
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		if err := client.Close(closeCtx); err != nil {
			b.debugf("askClaude: closing Claude Code: %v", err)
		}
		writer.Wait()
	}()
	writer.Add(1)
	go func() {
		defer writer.Done()
		defer b.recoverHandler("askClaude writer")
		if err := client.WriteUser(userMessage([]any{map[string]any{"type": "text", "text": r.Prompt}}, "")); err != nil {
			writeErr <- fmt.Errorf("write user message: %w", err)
			return
		}
		if err := client.EndInput(); err != nil {
			b.debugf("askClaude: closing Claude Code stdin: %v", err)
		}
	}()

	response := ""
	frames := client.Frames()
	for {
		var raw []byte
		select {
		case <-ctx.Done():
			b.killClient(client)
			return "", errors.New("Aborted")
		case err := <-writeErr:
			return "", err
		case f, open := <-frames:
			if !open {
				if err := b.awaitExit(ctx, client); err != nil {
					return "", err
				}
				if ctx.Err() != nil {
					return "", errors.New("Aborted")
				}
				b.debugf("askClaude: done responseLen=%d", len([]rune(response)))
				return response, nil
			}
			raw = f
		}
		var frame map[string]any
		if err := json.Unmarshal(raw, &frame); err != nil {
			b.debugf("askClaude: skipping malformed frame: %v", err)
			continue
		}
		switch str(frame["type"]) {
		case "assistant":
			for _, block := range list(obj(frame["message"])["content"]) {
				if bl := obj(block); str(bl["type"]) == "tool_use" && r.OnToolUse != nil {
					r.OnToolUse(str(bl["id"]), str(bl["name"]), bl["input"])
				}
			}
		case "result":
			// Claude Code reports an API failure as is_error on a result whose subtype is
			// still "success"; without this the error text would be returned as the answer.
			if failure, failed := resultErrorText(frame); failed {
				return "", errors.New(failure)
			}
			if result := str(frame["result"]); response == "" && frame["subtype"] == "success" && result != "" {
				response = result
			}
		}
	}
}
