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
	// sessions holds one CC-conversation mirror per pi session, keyed by sessionKey.
	sessions map[string]SessionState
	// syncLocks serialise syncSharedSession per session key.
	syncLocks map[string]*sync.Mutex
	// historyRewritten holds real pi session ids only, never the "(none)" key.
	historyRewritten map[string]struct{}
	// contexts are the live queries armStaleContexts marks.
	contexts map[*QueryContext]struct{}
	// cwds maps a pi session id to its working directory.
	cwds map[string]string
	// pendingSummary is the pi session whose compaction summary is about to run.
	pendingSummary string

	nextCLIDebugSeq atomic.Int64
}

// New returns a Bridge. A nil Deps.Now defaults to time.Now.
func New(d Deps) *Bridge {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Bridge{
		deps:             d,
		sessions:         map[string]SessionState{},
		syncLocks:        map[string]*sync.Mutex{},
		historyRewritten: map[string]struct{}{},
		contexts:         map[*QueryContext]struct{}{},
		cwds:             map[string]string{},
	}
}
