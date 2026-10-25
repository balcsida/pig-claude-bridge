package claudebridge

import (
	"reflect"
	"regexp"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/bridge"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/models"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/promptcapture"
)

func TestExtensionFactoryReturnsNamedExtension(t *testing.T) {
	a, b := Extension(), Extension()
	if a == nil || b == nil {
		t.Fatal("Extension() returned nil")
	}
	if a == b {
		t.Fatal("Extension() returned the same value twice")
	}
}

var _ bridge.Host = (*sdkHost)(nil)

func TestSdkHostImplementsBridgeHost(t *testing.T) {}

func TestRegisteredModelsComeFromCatalog(t *testing.T) {
	plan := "max"
	list := providerModels(&config.ProviderConfig{Plan: &plan})
	if len(list) == 0 {
		t.Fatal("no models")
	}
	dated := regexp.MustCompile(`-\d{8}$`)
	for _, m := range list {
		if m["provider"] != "claude-bridge" {
			t.Errorf("%v: provider = %v", m["id"], m["provider"])
		}
		if dated.MatchString(m["id"].(string)) {
			t.Errorf("dated alias %v", m["id"])
		}
	}
	want := models.ApplyLongContext(models.BuildModels(models.Catalog()), models.LongContextSettings{Plan: "max"})
	if !reflect.DeepEqual(list, want) {
		t.Error("models do not reflect ApplyLongContext for plan max")
	}
}

func TestBeforeAgentStartInputReadsPromptAndOrderedSections(t *testing.T) {
	sections := &sdk.SystemPromptSections{}
	sections.Set("zeta", "z")
	sections.Set("alpha", "a")
	data := map[string]any{
		"systemPrompt":        "base prompt",
		"systemPromptOptions": map[string]any{"cwd": "/p", "sections": sections},
	}
	prompt, options := beforeAgentStartInput(data)
	if prompt != "base prompt" {
		t.Errorf("prompt = %q", prompt)
	}
	want := []promptcapture.Section{{Name: "zeta", Text: "z"}, {Name: "alpha", Text: "a"}}
	if !reflect.DeepEqual(options["sections"], want) {
		t.Errorf("sections = %v, want %v", options["sections"], want)
	}
	if options["cwd"] != "/p" {
		t.Errorf("cwd = %v", options["cwd"])
	}
}

// A usable *sdk.SystemPrompt cannot be built outside the SDK (unexported fields, rendered through the host), so
// a nil one only proves the typed case is taken without a panic and yields no text.
func TestBeforeAgentStartInputAcceptsTypedSystemPrompt(t *testing.T) {
	if prompt, _ := beforeAgentStartInput(map[string]any{"systemPrompt": (*sdk.SystemPrompt)(nil)}); prompt != "" {
		t.Errorf("prompt = %q", prompt)
	}
}
