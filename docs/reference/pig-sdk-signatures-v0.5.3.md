# PiG Go extension SDK signature sheet (v0.5.3)

Quoted from the PiG clone at upstream main `ee5fdb16d` (pigversion 0.5.3, SDK `go 1.26`). Paths are relative to the PiG repo root. Produced 2026-10-25 by a read-only recon pass.

---

# PiG Go SDK signature sheet (extensions/sdk, git-tracked sources only)

Module: `github.com/MichaelKinsy/PiG/extensions/sdk` (extensions/sdk/go.mod: `go 1.26`). Import as `sdk "github.com/MichaelKinsy/PiG/extensions/sdk"`.
Everything below is quoted from source; paths are relative to repo root. Anything unverified is marked NOT FOUND / UNVERIFIED.

## 1. Entry (extension.go)
```go
extension.go:120  type Factory func() *Extension
extension.go:686  func New(name string) *Extension          // "name must match the identity of the selected Package, Piglet, or exact path"
extension.go:717  func (e *Extension) Tool(name, description string, schema Schema, handler ToolFunc)   // nil schema panics
extension.go:784  func (e *Extension) Command(name, description string, handler CommandFunc)
extension.go:828  func (e *Extension) RegisterProvider(name string, config ProviderConfig)   // panics if host refuses (after Run); queued before Run
extension.go:928  func (e *Extension) UnregisterProvider(name string)
extension.go:1004 func (e *Extension) OnSessionStart(handler EventFunc) func()
extension.go:1009 func (e *Extension) OnSessionShutdown(handler EventFunc) func()
extension.go:1014 func (e *Extension) OnToolResult(handler EventFunc) func()
extension.go:1020 func (e *Extension) OnProjectTrust(handler ProjectTrustFunc) func()
extension.go:1027 func (e *Extension) OnEvent(eventName string, handler EventFunc) func()   // returns idempotent unsubscribe
extension.go:1071 func (e *Extension) Run() error            // needs env PIG_EXT_SOCKET; blocks until shutdown/disconnect
extension.go:1081 func (e *Extension) RunWithSocket(sockPath string) error
extension.go:1098 func (e *Extension) RunWithConn(nc net.Conn) error
extension.go:79   type ProviderConfig map[string]any
extension.go:26   type Schema = map[string]any
extension.go:34   type ToolFunc func(ctx Context, params map[string]any) (any, error)
extension.go:37   type CommandFunc func(ctx Context, args string) error
extension.go:43   type EventFunc func(ctx Context, data map[string]any) (any, error)
provider.go:187   func (e *Extension) RegisterNativeProvider(provider *Provider) error
tool_definition.go:92  func (e *Extension) RegisterTool(def ToolDefinition)   // replaces same-name in place; invalid schema/host failure panic
mcp_servers.go:96   func (e *Extension) RegisterMcpServer(name string, config McpServerConfig) error
mcp_servers.go:108  func (c Context) RegisterMcpServer(name string, config McpServerConfig) error
events.go:  func (e *Extension) OnProviderStreamEvent(handler EventFunc) func()
events.go:  func (e *Extension) OnMcpServersChange(handler EventFunc) func()
```
No `RegisterCommand` exists: the command API is `Command`. Other helpers: `ToolPromptSnippet`, `ToolWithPrepareArguments`, `ToolWithGuidelines`, `ToolWithSource`, `ToolWithConstrainedSampling`, `Shortcut(key, description, ShortcutFunc)`, `Flag(name, FlagOptions)`.
Factory convention (extension.go:113-118): package exposes `func Extension() *sdk.Extension`; Pig discovers it by exact signature and generates the runner (examples README).

## 2. Provider (provider.go, credential.go, context.go, provider_stream.go)
Two registration routes:
1. **Legacy config route**: `ext.RegisterProvider(name, sdk.ProviderConfig{... "streamSimple": sdk.ProviderStreamSimpleFunc(...) ...})`. extension.go `takeProviderDef`: `raw.(ProviderStreamSimpleFunc)` else `panic("provider streamSimple has an invalid Go callback signature")` — value MUST be exactly that func type (alias, so a plain func literal of that signature works). Other keys (`baseUrl`, `apiKey`, `api`, `models`, `headers`, `authHeader`, `oauth` = `*OAuthProvider`, images/classifiers funcs) go through `json.MarshalJS`.
2. **Native Provider object**: `ext.RegisterNativeProvider(&sdk.Provider{...})`.
```go
provider_stream.go:9  type ProviderStreamSimpleFunc = func(Context, map[string]any, map[string]any, map[string]any) (*ModelEventStream, error)
//   doc: "Context owns cancellation; model, request and options have Pi's wire shapes."  args = (ctx, model, context, options)

provider.go:12 type Provider struct {
	handle    string
	ID        string
	Name      string
	BaseURL   *string
	Headers   map[string]string
	Auth      ProviderAuth
	GetModels func() ([]map[string]any, error)
	GetAllModels func() ([]map[string]any, error)        // models of every type; without it GetModels does
	FilterModels func([]map[string]any, *Credential) ([]map[string]any, error)
	FilterAllModels func([]map[string]any, *Credential) ([]map[string]any, error)
	RefreshModels   func(RefreshModelsContext) error
	Stream          ProviderStreamFunc
	StreamSimple    ProviderStreamFunc
	FetchDeferred   ProviderStreamFunc
	CancelDeferred  func(map[string]any, map[string]any, ProviderStreamOptions) error
	GenerateImages ProviderImagesFunc
	Classify       ProviderClassifyFunc
}
provider.go:38 type ProviderStreamFunc func(model, transcriptOrHandle map[string]any, options ProviderStreamOptions) (*ModelEventStream, error)
provider.go:41 type ProviderStreamOptions struct {
	Signal     context.Context
	Values     map[string]any                              // serializable request options
	OnPayload  func(any, map[string]any) (any, error)      // nil = caller passed none
	OnResponse func(map[string]any, map[string]any) error
	OnProviderStreamEvent func(any, map[string]any) error   // observer of parsed provider event before normalization
}
provider.go:51 type ProviderAuth struct { APIKey *APIKeyAuth; OAuth *OAuthAuth }
provider.go:55 type APIKeyAuth struct {
	Name    string
	Check   func(APIKeyAuthInput) (*AuthCheck, error)
	Resolve func(APIKeyAuthInput) (*AuthResult, error)     // REQUIRED if APIKey set ("API key auth requires resolve")
	Login func(AuthInteraction) (*Credential, error)
}
provider.go:62 type OAuthAuth struct {
	Name string; IsSubscription *bool; LoginLabel *string
	Login   func(AuthInteraction, LoginOptions) (*Credential, error)
	Refresh func(*Credential, context.Context) (*Credential, error)
	ToAuth  func(*Credential) (map[string]any, error)      // Login/Refresh/ToAuth all REQUIRED
}
provider.go:  type APIKeyAuthInput struct { Ctx AuthContext; Credential *Credential; Signal context.Context }
provider.go:  type AuthResult struct { Auth map[string]any `json:"auth"`; Env map[string]string `json:"env,omitempty"`; Source *string `json:"source,omitempty"` }
provider.go:  type AuthCheck struct { Type string `json:"type"`; Source *string `json:"source,omitempty"` }
provider.go:  type AuthContext struct { Env func(string) (*string, error); FileExists func(string) (bool, error) }
provider.go:  type AuthInteraction struct { Signal context.Context; Prompt func(map[string]any) (string, error); Notify func(map[string]any) error }
provider.go:  type RefreshModelsContext struct { Credential *Credential; Stored map[string]any; AllowNetwork bool; Force *bool; Signal context.Context; Publish func(ModelsPublication) (bool, error); BaseURL string }
```
`providerDeclaration` (provider.go) requires: non-blank `ID`, and `GetModels`, `Stream`, `StreamSimple` all non-nil ("Provider requires getModels, stream and streamSimple").

Credential (credential.go): `type Credential struct{ object OrderedObject }`; `func NewCredential(pairs ...any) *Credential`; methods `Get(key) (any,bool)`, `String(key) string`, `Keys() []string` (+ more; nil receiver safe). Order-preserving (auth.json).

### ModelEventStream (context.go:1330; ctor/Push in provider.go:212-219)
```go
func CreateAssistantMessageEventStream() *ModelEventStream            // provider.go:212; timed: final message gets durationMs
func (s *ModelEventStream) Push(event map[string]any)                 // provider.go:219; terminal event "done"/"error" completes Result and closes Events; Push after terminal is silently dropped (context.go push)
func (s *ModelEventStream) End(result map[string]any)                 // context.go:1386; completes iteration with result; no-op if already terminal
func (s *ModelEventStream) Events(ctx context.Context) <-chan map[string]any   // context.go:1437; ordered until terminal or ctx cancel
func (s *ModelEventStream) Result() map[string]any                    // context.go:1487; BLOCKS on s.done, returns terminal message
```
There is NO `Close`, no `Err`, no `Fail`/`PushError` method. Error reporting = push an `{"type":"error","error":{assistant message...}}` event (terminal), or return `(nil, err)` from the stream func. Only other exported stream constructor: none (`newModelEventStream` is unexported; grep `func (Create|New).*Stream` found only `CreateAssistantMessageEventStream`).
Terminal handling (context.go:1408-1430): on type=="done" result=event["message"]; on "error" result=event["error"].

## 3. Dispatch
### Legacy `streamSimple` (provider_stream.go dispatchProviderStream)
```go
var args struct {
	Model   map[string]any `json:"model"`
	Context map[string]any `json:"context"`
	Options map[string]any `json:"options"`
}
...
stream, err := handler(ctx, args.Model, args.Context, args.Options)
// err -> conn.respond(id,nil,err); nil stream -> error "provider returned nil stream"
for event := range stream.Events(ctx.ctx) {
	e.conn.notify("provider_stream_event", map[string]any{"request_id": id, "result": event})  // failure -> respond(err), return
}
select {
case <-ctx.Done():       _ = e.conn.respond(id, nil, ctx.ctx.Err())
case <-stream.done:      _ = e.conn.respond(id, stream.Result(), nil)
}
```
Cancellation: the request `Context` (`ctx.Done()`, `ctx.Err()`, `ctx.CancellationReason()`, context.go:268-307). Events loop stops when ctx cancelled; response is ctx error.

### Native `Provider` (provider_dispatch.go)
Params struct (provider_dispatch.go:13):
```go
type providerObjectParams struct {
	Models []map[string]any `json:"models"`; Credential *Credential `json:"credential"`
	Model map[string]any `json:"model"`; Context map[string]any `json:"context"`; Handle map[string]any `json:"handle"`
	Options map[string]any `json:"options"`; Callbacks []string `json:"callbacks"`; Aborted bool `json:"aborted"`
	Stored map[string]any `json:"stored"`; AllowNetwork bool `json:"allowNetwork"`; Force *bool `json:"force"`
	BaseURL string `json:"baseUrl"`; Token string `json:"token"`
}
```
Stream branch (provider_dispatch.go ~195-246):
```go
case "stream", "streamSimple", "fetchDeferred", "cancelDeferred":
	options := ProviderStreamOptions{Values: args.Options, Signal: ctx.ctx}
	if args.Aborted { signal, cancel := context.WithCancel(ctx.ctx); cancel(); options.Signal = signal }
	// Callbacks onPayload/onResponse/onProviderStreamEvent wired only if named in args.Callbacks (host round trip "provider.callback")
	transcript := args.Context            // fetchDeferred uses args.Handle
	stream, err := method(args.Model, transcript, options)
	if stream == nil { return nil, errors.New("Provider returned no stream") }
	e.conn.notify("tool_update", {"request_id": ctx.requestID, "result": {"type": "provider_started"}})
	for event := range stream.Events(e.runCtx) {
		if err := e.conn.notify("tool_update", {"request_id": ctx.requestID, "result": event}); err != nil { return nil, err }
	}
	return nil, nil
```
Note: native loop uses `e.runCtx` (extension-wide), NOT the request ctx, to drain events; `options.Signal` is the request ctx (`ctx.ctx`). Host-disconnect behaviour of an in-flight stream: UNVERIFIED beyond this (notify error returns; `Events(e.runCtx)` ends when runCtx cancels). The host-side proxy (provider_proxy.go:321 `providerStream`) is the *consumer* side (extension calling another provider via host) and ends with error event "Provider stream ended without a terminal event" if the host call returns without terminal event.

Key maps built by the host (SDK reads only): `model` = Pi model JSON object (keys incl. `id`, `provider`, `api`, `name`, `baseUrl`, `reasoning`, `input`, `cost`, `contextWindow`, `maxTokens`; see model_select payload in event_payload_test.go); `context`/`request` = Pi Context `{"messages":[...], ...}` (test uses `{"messages": []any{}}`); `options` = serializable StreamOptions via host `providerOptionsWire` (coding/extension/host/subprocess/native_provider.go:257-262; `apiKey` deleted when empty). Exact key list of options: NOT FOUND in SDK (built host-side by `providerOptionsWire`).

Run signal (run_signal.go): `func (c Context) Signal() context.Context` = cancellation of the run in progress, nil when no run is active; host frame `run_signal` `{run, active, aborted}`.

### Event ordering & accepted types
- provider_stream_order_test.go:13 `TestProviderStreamResultDoesNotOvertakeNotifications` (methods stream/streamSimple/fetchDeferred × stop/error/aborted): a call_result cannot overtake earlier creation or terminal notifications; creation doesn't wait for termination.
- Host decoder (coding/extension/host/subprocess/native_provider.go:272-304) accepts `type` ∈ `start, text_start, text_delta, text_end, thinking_start, thinking_delta, thinking_end, toolcall_start, toolcall_delta, toolcall_end, done, error`; anything else → `unknown native provider event "<type>"`. Decoded into `ai.<X>Event` structs (ai package; field list NOT quoted here).
- Wire sample (test/extension-conformance/event_payload_test.go message_update): `{"type":"text_delta","contentIndex":0,"delta":"x","partial":<assistant message>}`. SDK itself does not validate event fields; it only inspects `type` for `done`/`error` (and `message` / `error` keys). Pinning tests for exact field sets: coding/extension/host/subprocess/runtime_node_model_event_stream_test.go, provider_stream_shutdown_node_test.go, packed_go_test.go (not read in detail — UNVERIFIED which fields each requires).
- Conformance rows: `TestProviderProducersAcrossSDKs`, `TestExtensionProviderCommandStreamsAcrossProcesses`, scenario `22-extension-provider-stream` (docs/extension-api-parity.md:194).

## 4. Registry (context.go:1525, model_registry.go, model_types.go)
```go
context.go:1525 func (c Context) ModelRegistry() ModelRegistry          // type ModelRegistry struct{ context Context }
context.go:1528 func (r ModelRegistry) Find(providerID, modelID string) (map[string]any, error)   // nil,nil when absent
context.go:1533 func (r ModelRegistry) GetApiKeyAndHeaders(model map[string]any) (map[string]any, error)
context.go:1558 func (r ModelRegistry) Stream(model, request, options map[string]any) *ModelEventStream
context.go:1616 func (r ModelRegistry) StreamSimple(model, request, options map[string]any) *ModelEventStream
context.go:1620 func (r ModelRegistry) Complete(model, request, options map[string]any) map[string]any
model_registry.go:41  func (r ModelRegistry) GetAll() ([]map[string]any, error)         // chat models only
model_registry.go:45  func (r ModelRegistry) GetAvailable() ([]map[string]any, error)   // provider Configured && (AvailableModelIDs nil or contains id)
model_registry.go:61  func (r ModelRegistry) GetError() (*string, error)
model_registry.go:65  func (r ModelRegistry) HasConfiguredAuth(model map[string]any) (bool, error)   // keyed by model["provider"] string
model_registry.go:70  func (r ModelRegistry) IsUsingOAuth(model map[string]any) (bool, error)
model_registry.go:75  func (r ModelRegistry) GetProviderAuthStatus(provider string) (ProviderAuthStatus, error)
model_registry.go:79  func (r ModelRegistry) GetProviderDisplayName(provider string) (string, error)
model_registry.go:86  func (r ModelRegistry) GetProviderAuth(provider string) (map[string]any, error)
model_registry.go:91  func (r ModelRegistry) GetApiKeyForProvider(provider string) *string
model_registry.go:103 func (r ModelRegistry) GetRegisteredProviderIDs() ([]string, error)
model_registry.go:114 func (r ModelRegistry) GetRegisteredProviderConfig(provider string) (ProviderConfig, error)
model_registry.go:128 func (r ModelRegistry) RegisterProvider(name string, config ProviderConfig) error
model_registry.go:167 func (r ModelRegistry) UnregisterProvider(name string) error
model_registry.go:194 func (r ModelRegistry) Refresh(options ModelsRefreshOptions) (ModelsRefreshResult, error)
model_types.go:191 GetModelsOfType(modelType ModelType, provider ...string); :212 GetModelOfType(modelType, provider, modelID); :226 FindOfType; :231 GetAvailableOfType; :243 Classify; :282 GenerateImages; :305 RegisterVirtualModel(VirtualModel) error; :310 UnregisterVirtualModel(provider, id string)
model_types.go: type ModelType string; const ModelTypeChat="chat", ModelTypeImage="image", ModelTypeClassifier="classifier"
```
Model maps are untyped `map[string]any` (no model struct). Keys read by SDK: `provider` (string, or object with `id`), `id` / `modelId`. No exported model-field constants found (NOT FOUND). Registry state shape (model_registry.go:15-35): `models`, `typedModels`, `providers{name,baseUrl,authStatus,configured,usingOAuth,availableModelIds}`, `error`, `registered[{name,config,native}]`.
Late registration (late_provider_test.go): callbacks (`streamSimple`, images, classifiers, oauth) stay in extension; a later `RegisterProvider` merges only kinds it defines (…KeepsOperationsAfterTheFactory / MergesOperationsLikeTheFactory); `UnregisterProvider` drops held callbacks only once host removed it; invalid callback → error, provider stays usable; host-refused registration keeps previously held callbacks; `Extension.RegisterProvider` after Run sends host call and PANICS on refusal; before Run it queues (`TestExtensionRegisterProviderBeforeRunQueues`).

## 5. Events (events.go, verbatim constants)
```go
EventProjectTrust="project_trust" EventResourcesDiscover="resources_discover" EventSessionStart="session_start" EventSessionInfoChanged="session_info_changed"
EventSessionBeforeSwitch="session_before_switch" EventSessionBeforeFork="session_before_fork" EventSessionBeforeCompact="session_before_compact"
EventSessionCompact="session_compact" EventSessionCompactFailed="session_compact_failed" EventSessionShutdown="session_shutdown"
EventSessionBeforeTree="session_before_tree" EventSessionTree="session_tree"
EventContext="context" EventContextWithSystem="context_with_system" EventCacheWarmingDecision="cache_warming_decision"
EventBeforeProviderRequest="before_provider_request" EventBeforeProviderHeaders="before_provider_headers" EventAfterProviderResponse="after_provider_response"
EventProviderStreamEvent="provider_stream_event" EventMcpServersChange="mcp_servers_change"
EventBeforeAgentStart="before_agent_start" EventAgentStart="agent_start" EventAgentEnd="agent_end" EventAgentBeforeSettle="agent_before_settle" EventAgentSettled="agent_settled"
EventUIPromptStart="ui_prompt_start" EventUIPromptEnd="ui_prompt_end" EventTurnStart="turn_start" EventTurnEnd="turn_end"
EventMessageStart="message_start" EventMessageUpdate="message_update" EventMessageEnd="message_end"
EventToolExecutionStart="tool_execution_start" EventToolExecutionUpdate="tool_execution_update" EventToolExecutionEnd="tool_execution_end"
EventModelSelect="model_select" EventThinkingLevelSelect="thinking_level_select" EventToolCall="tool_call" EventToolResult="tool_result" EventUserBash="user_bash" EventInput="input"
```
Handler: `type EventFunc func(ctx Context, data map[string]any) (any, error)` (data = decoded JSON payload; return `(nil,nil)` to acknowledge; otherwise a result value). Typed accessors: `MessageRole(data map[string]any) string`, `MessageText(data map[string]any) string` (event_message.go:16,28); `SystemPromptOptions` / `SystemPromptSections.Set(name, value)/.Delete(name)` (context.go:928, prompt_sections.go:22,31); `BeforeAgentStartEventResult` mentioned in EventFunc doc but its type definition NOT FOUND via grep.
Payload keys (from test/extension-conformance/event_payload_test.go, wire JSON):
| event | keys |
|---|---|
| session_start | type, reason, previousSessionFile |
| session_shutdown | type, reason, targetSessionFile |
| session_compact | type, compactionEntry, fromExtension, reason, willRetry |
| session_before_compact | type, preparation, branchEntries, customInstructions, reason, willRetry |
| session_tree | type, newLeafId, oldLeafId, summaryEntry, fromExtension |
| session_before_tree | type, preparation{targetId, oldLeafId, commonAncestorId, entriesToSummarize, userWantsSummary, customInstructions, replaceInstructions, label} |
| before_agent_start | type, messageType, prompt, images, systemPrompt, systemPromptOptions{appendSystemPrompt, contextFiles, customPrompt, cwd, hiddenTools, promptGuidelines, sections, selectedTools, skills, toolGuidelines, toolSnippets} |
| agent_start | type |
| turn_start | type, turnIndex, timestamp |
| tool_result | type, toolCallId, parentToolCallId, input, content, isError, toolName |
| tool_call | type, toolCallId, parentToolCallId, toolName, input |
| context | type, messages |
| before_provider_request | type, payload |
| after_provider_response | type, status, headers |
| provider_stream_event | type, provider, api, model, data |
| message_update | type, message, assistantMessageEvent |
Host gaps: events.go comments mark none as gaps. Whether any listed event is not delivered in a given mode: NOT FOUND in my read (docs/extension-api-parity.md has per-event rows, e.g. line 144 `before_provider_request` "wired in the Session for every mode").

## 6. Tools
```go
tool_definition.go:34 type ToolDefinition struct {
	Name, Label, Description, PromptSnippet string
	PromptGuidelines []string
	Parameters Schema
	ConstrainedSampling ToolConstrainedSampling
	RenderShell ToolRenderShell
	PrepareArguments ToolPrepareArgumentsFunc
	ExecutionMode string            // "sequential" | "parallel" | ""
	Execute ToolFunc                // cancellation = ctx.Done; partials via ctx.OnUpdate
	OutputSchema Schema; Exposure ToolExposure; Namespace *ToolNamespace; Annotations *ToolAnnotations
	DefaultActive *bool; PrepareLoadout ToolPrepareLoadoutFunc; ReachesTools ToolReachesToolsFunc
	RenderCall ToolRenderCallFunc; RenderResult ToolRenderResultFunc
	RenderCallView ToolRenderCallViewFunc; RenderResultView ToolRenderResultViewFunc
}
extension.go:34   type ToolFunc func(ctx Context, params map[string]any) (any, error)
extension.go:29   type ToolPrepareArgumentsFunc func(params map[string]any) (map[string]any, error)
tool_renderers.go type ToolRenderCallFunc func(ctx Context, args map[string]any, render ToolRenderContext, width int) ([]string, error)
tool_renderers.go type ToolRenderResultFunc func(ctx Context, result ToolRenderResult, options ToolRenderResultOptions, render ToolRenderContext, width int) ([]string, error)
tool_renderers.go type ToolRenderContext struct { Args map[string]any; ToolCallID, Cwd string; ExecutionStarted, ArgsComplete, IsPartial, Expanded, ShowImages, IsError bool; OutputPad int; DurationMs *int64; State map[string]any; Invalidate func() }
tool_renderers.go type ToolRenderResult struct { Content []map[string]any; Details any }
tool_renderers.go type ToolRenderResultOptions struct { Expanded bool; IsPartial bool }
extension.go:2069 type ToolError struct{ Content string }   ; :2075 func NewToolError(content string) *ToolError
extension.go:2100 type ToolResult struct { Content string; Images []ImageContent; Preview string; Details any; StructuredContent any ... }
tool_orchestration.go:211 type AgentToolResult struct { Content []ToolResultContent; Details any; StructuredContent any; Usage map[string]any; IsError bool; Terminate bool ... }
```
Handler may return `map[string]any{"content": "..."}` (example) or `ToolResult`/`*ToolError`; returned `error` = error result.

## 7. Context (context.go, session_manager.go, host_info.go, settings*.go) — `type Context` is passed by value
UI: `Notify(message, level string)`; `SetStatus(key, text string)`; `ClearStatus(key)`; `SetWidget(key string, content any, options ...WidgetOptions) error`; `SetFooter(lines []string) error`; `SetHeader(lines []string) error`; `SetTitle(string)`; `Custom(factory any, options any) (any, error)`; `Select(title string, options []string) (any, bool, error)`; `Confirm(title, message string) (any, error)`; `Input(title, placeholder string) (any, bool, error)`; `Editor(title, prefill string) (any, bool, error)`; `SelectWithOptions/ConfirmWithOptions/InputWithOptions(..., DialogOptions)`; `SetWorkingMessage/ClearWorkingMessage/SetWorkingVisible/SetWorkingIndicator`; `SetEditorText/GetEditorText/PasteToEditor`; `SetTheme/GetTheme/GetAllThemes`; `OnTerminalInput`; `OnWidthChange`.
Session (`func (c Context) SessionManager() ReadonlySessionManager`, session_manager.go:12): `GetCwd() (string, error)`, `GetSessionDir()`, `GetSessionID() (string, error)`, `GetSessionFile() (*string, error)`, `GetSessionName() (*string, error)`, `GetLeafID() (*string, error)`, `GetHeader() (map[string]any, error)`, `GetLeafEntry()`, `GetEntry(id string)`, `GetLabel(id string) (*string, error)`, `GetEntries() ([]map[string]any, error)`, `GetBranch(fromID *string) ([]map[string]any, error)`, `GetChildren(parentID *string)`, `GetTree() ([]SessionTreeNode, error)`, `BuildContextEntries()`, `BuildSessionProjection()`, `BuildSessionContext()`, `IsPersisted() (bool, error)`. No AppendEntry on SessionManager (readonly); append is `Context.AppendEntry(customType string, data any) error` (context.go:676). Also Context-level `GetEntries() ([]json.RawMessage, error)` (:1287), `GetBranch() ([]BranchEntry, error)` (:1253), `GetSessionID/GetSessionFile/GetLeafID` (:1298-1308).
Messages/session control: `SendMessage(customType, content string, display bool, opts SendMessageOptions) error`; `SendUserMessage(content any, deliverAs string) error`; `SetSessionName/SessionName`; `SetLabel`; `NewSession/Fork/NavigateTree/SwitchSession(…) (CancelledResult, error)`; `Reload() error`; `Compact(map)`/`CompactWithOptions`; `WaitForIdle() error`; `Abort()`; `Shutdown()`.
Model: `Model() string`; `ModelProvider() string`; `ModelQualified() string`; `SetModel(model string) (bool, error)`; `GetModelInfo() (*ModelInfo, error)`; `ModelRegistry() ModelRegistry`; `GetModelAuth(providerID, modelID string) (json.RawMessage, error)`; `GetSystemPrompt() (string, error)`; `GetSystemPromptOptions() (SystemPromptOptions, error)`; `GetThinkingLevel()/SetThinkingLevel(level string)`; `Complete(model, request, auth map[string]any) (json.RawMessage, error)`; `GetContextUsage() (*ContextUsage, error)`.
Tools: `RegisterTool(ToolDefinition)`; `GetActiveTools() ([]string, error)`; `GetAllTools() ([]ToolInfo, error)`; `SetActiveTools([]string)`; `RefreshTools()`; `GetCommands() ([]CommandInfo, error)`; `ToolCallID() string`; `OnUpdate(partial any) error`.
Dirs/settings: `Cwd() string`; `ConfigHome() (string, error)`; package-level `AgentDir() (string, error)`, `ConfigDirName() (string, error)`, `Version()`, `PackageDir()` (host_info.go:45-77, read env-provided hostInfo; `ErrHostInfoUnavailable` otherwise); `(c Context) GetSettings() (Settings, error)` (`type Settings = map[string]any`, settings.go:20); `func SettingsManagerCreate(c Context, cwd string, agentDir *string, options *SettingsManagerCreateOptions) (SettingsManager, error)` (settings_manager.go:27); `GetFlag(name string) (any, error)`. `ConfigDirs`, `Settings()` method: NOT FOUND.
Process: `Exec(command string, args []string) (*ExecResult, error)`; `ExecWithOptions(command string, args []string, opts ExecOptions) (*ExecResult, error)`; `Signal() context.Context` (run_signal.go); `Done() <-chan struct{}`; `Err() error`; `CancellationReason() string`; `IsIdle() (bool, error)`; `HasUI() bool`; `Mode() string`; `IsProjectTrusted() (bool, error)`; `HasPendingMessages() (bool, error)`; `Width()/Height() int`.

## 8. Packaging
examples/extensions/go-factory/go.mod (verbatim):
```
module example.com/exgo

go 1.26

require github.com/MichaelKinsy/PiG/extensions/sdk v0.5.3

replace github.com/MichaelKinsy/PiG/extensions/sdk => ../../../extensions/sdk
```
examples/extensions/go-factory/extension.go: `package exgo`, `func Extension() *sdk.Extension { ext := sdk.New("go-factory"); ext.Tool("hello", ..., sdk.Schema{"type":"object"}, func(ctx sdk.Context, params map[string]any) (any, error) { return map[string]any{"content": "hello from go"}, nil }) ... return ext }` (imports `sdk "github.com/MichaelKinsy/PiG/extensions/sdk"`). No package.json in that example (manifest-free).
Run: `pig install --validate-only --json <dir>`; `pig -e <dir>`.
Rules (docs/extension-authoring.md:346-354):
- "`pig extension init` writes versioned SDK dependencies without machine-local paths. During a source build, Pig resolves an un-replaced Go requirement on `github.com/MichaelKinsy/PiG/extensions/sdk` ... to the SDK staged by the running binary under the active config root. The build uses a temporary Go modfile ... never edits the authored `go.mod`. An explicit valid author replacement or path wins. If staged material is missing, run `pig reload`." So: a `replace` is optional; plain `require` suffices.
- Go floor: SDK `go 1.26`; project policy: modules declare 1.26 floor (AGENTS.md).
- Dir is a Package only if it has a `pi` block in package.json or `extensions/skills/prompts/themes` dirs; then root `go.mod` beside such dirs loads nothing. Manifest shape (docs/site/docs/packages.md:42-50): `{"name":"@example/base-coding","version":"1.0.0","pi":{"extensions":["extensions/*"],"skills":["skills/*"],"prompts":["prompts/*.md"],"themes":["themes/*.json"]}}`; list the Go extension subdirectory in `pi.extensions`.
- Legacy import `github.com/mainstai/pig/extensions/sdk` still loads but is never fused.
- Build cache/rebuild rules: NOT FOUND in the three docs (grep for cache/rebuild found only: "A Go build that fails only on changed SDK symbols reports `written for an older SDK`", `pig extension upgrade`, docs/extension-authoring.md:358-360; packages.md:232 "The cache makes later matching loads fast, but the cache path is not a package version or reproducibility identity.").
- Exact standalone (single Go package with its own main) is always isolated; factory form can be isolated/packed/fused (AGENTS.md).

## 9. Provider-related limits
| capability | status | note |
|---|---|---|
| `config.streamSimple` producer (Go `ProviderStreamSimpleFunc`) | implemented (docs/extension-api-parity.md:194) | host calls `provider_stream_simple`, ordered `provider_stream_event`; tests `TestProviderProducersAcrossSDKs`, scenario 22 |
| `provider.streamSimple` in docs/extension-sdk-surface.md:1332 | listed "missing [exception]" for all SDKs | STALE vs api-parity row; reason in test/parity/sdk-surface-exceptions.toml:39-41 |
| `provider.refreshModels` (config route) | surface doc: missing (toml:44-46) | native `Provider.RefreshModels` exists in Go (provider.go) — conflicting; treat config route as unsupported |
| `provider.oauth.usesCallbackServer` | missing, deprecated upstream | inert metadata not forwarded |
| `provider.oauth.modifyModels` | missing | no host callback |
| `provider.oauth` login/refreshToken/getApiKey/isSubscription/name | implemented | Go `OAuthProvider` (config route) |
| name, baseUrl, apiKey, api, headers, authHeader, models (+ model type/api/reasoning/thinkingLevelMap/promptCache/contextWindow/maxTokens/samplingParams/compat/id/name/baseUrl/input/inputLimits/cost/headers/output) | implemented | via `RegisterProvider` |
| images / classifiers | implemented | `ProviderImagesFunc` / `ProviderClassifyFunc` |
| registerProvider/unregisterProvider after load | implemented | snapshot republished before return; late callbacks kept (api-parity :198-205) |
| registered-config read | implemented | `GetRegisteredProviderConfig` returns single effective root |

### Residual unknowns
- Exact `options` key set and per-event required fields for streams (host `providerOptionsWire`, `ai.*Event` structs) not read.
- In-flight stream behaviour on host disconnect only inferred from code above.
- Build-cache/rebuild rules not documented in the three requested docs.
