package claudebridge

import (
	"reflect"
	"regexp"
	"testing"

	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/bridge"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/config"
	"github.com/balcsida/pig-claude-bridge/extensions/pig-claude-bridge/internal/models"
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
