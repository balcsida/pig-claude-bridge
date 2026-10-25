package claudebridge

import "testing"

func TestExtensionFactoryReturnsNamedExtension(t *testing.T) {
	a, b := Extension(), Extension()
	if a == nil || b == nil {
		t.Fatal("Extension() returned nil")
	}
	if a == b {
		t.Fatal("Extension() returned the same value twice")
	}
}
