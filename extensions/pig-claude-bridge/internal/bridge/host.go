// Package bridge is the port of pi-claude-bridge's src/index.ts core: per-session
// state, cwd tracking and the shared-session sync that decides how a Claude Code
// session file follows pi's history.
package bridge

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/ccproto"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/models"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/promptcapture"
)

// Host is what the bridge needs from PiG for one request. It is passed into every
// entry point per call and never stored on Bridge: PiG's sdk.Context is per
// request, so a stored adapter would go stale and race across concurrent calls.
type Host interface {
	Notify(message, level string)
	SessionID() (string, error)
	Cwd() string
	AgentDir() string
	ConfigDirName() string
	SystemPrompt() (string, error)
	// Mode is PiG's run mode: "tui", "rpc", "json" or "print".
	Mode() string
}

// Deps is everything a Bridge needs that is not per request.
type Deps struct {
	Config        config.Config
	AgentDir      string
	ConfigDirName string
	Spawn         func(context.Context, ccproto.Options, ccproto.Handlers) (ccproto.Client, error)
	Now           func() time.Time
	Debug         bool
}

// Bridge serves many pi sessions from one long-lived process, one goroutine per
// host request. mu guards every map below; values stored in them are copied in and
// out, never shared.
type Bridge struct {
	deps Deps

	mu sync.Mutex
	// sessions holds one CC-conversation mirror per pi session id (never "").
	sessions map[string]*SessionState
	// loaded are the pi session ids whose persisted state has been read once; memory
	// is authoritative after. known are the ids this process read or wrote state for.
	loaded, known map[string]bool
	// writers maps a CC session id to the exit of the child last writing it.
	writers map[string]<-chan struct{}
	// syncLocks serialise syncSharedSession per pi session id.
	syncLocks map[string]*sync.Mutex
	// historyRewritten holds the pi session ids pi rewrote (compact, tree) since their last query.
	historyRewritten map[string]struct{}
	// contexts are the live queries armStaleContexts marks.
	contexts map[*QueryContext]struct{}
	// cwds maps a pi session id to its working directory.
	cwds map[string]string
	// pendingSummary is the pi session whose compaction summary is about to run.
	pendingSummary string
	// pendingNotices are the startup notices not yet shown.
	pendingNotices []string
	// lastPromptOptions are the pi prompt options stashed at before_agent_start for the
	// later boundaries to reuse.
	lastPromptOptions *promptOptions
	// executable is the resolved Claude Code binary; "" until the first query resolves it.
	executable string
	// resolveMu serialises resolveExecutable; it is never taken under mu.
	resolveMu sync.Mutex

	// captures is the prompt-capture registry, guarded by its own mutex.
	captures    *promptcapture.Captures
	longContext models.LongContextSettings
	// fastMode adds fastMode to Claude Code's --settings; /claude-fast flips it mid-session.
	fastMode atomic.Bool
	// plan is provider.plan as configured or detected, "" when unknown; the footer shows it.
	plan string
	// lastUsage is the usage windows the footer last showed, guarded by mu.
	lastUsage string

	// queries tracks the goroutines a provider call starts, so a shutdown or a test
	// can wait for them.
	queries sync.WaitGroup
	// resultFallbackHook is a test seam run inside the result fallback's critical section.
	resultFallbackHook func()

	nextCLIDebugSeq atomic.Int64
}

// New returns a Bridge. A nil Deps.Now defaults to time.Now.
func New(d Deps) *Bridge {
	if d.Now == nil {
		d.Now = time.Now
	}
	b := &Bridge{
		deps:             d,
		sessions:         map[string]*SessionState{},
		loaded:           map[string]bool{},
		known:            map[string]bool{},
		writers:          map[string]<-chan struct{}{},
		syncLocks:        map[string]*sync.Mutex{},
		historyRewritten: map[string]struct{}{},
		contexts:         map[*QueryContext]struct{}{},
		cwds:             map[string]string{},
		longContext:      LongContextSettings(d.Config.Provider),
	}
	if p := d.Config.Provider; p != nil && p.FastMode != nil {
		b.fastMode.Store(*p.FastMode)
	}
	if p := d.Config.Provider; p != nil && p.Plan != nil {
		b.plan = *p.Plan
	}
	b.captures = promptcapture.NewCaptures(0, b.debugCaptureMiss)
	b.queueStartupNotices()
	for _, p := range d.Config.Problems {
		b.debugf("%s", p)
	}
	return b
}
