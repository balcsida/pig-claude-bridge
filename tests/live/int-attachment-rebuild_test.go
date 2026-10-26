//go:build live

package live

import "testing"

// Port of tests/int-attachment-rebuild.mjs: an `@file` attachment survives a rebuild and reaches Claude, and
// the control rebuild without the carry loses it. The test body is in the extension module
// (internal/ccproto/live_attachment_test.go) because it calls the conversion and session code directly.
func TestIntAttachmentRebuild(t *testing.T) {
	runExtensionLiveTests(t, "TestAnAtFileAttachmentSurvivesARebuildAndReachesClaude", "TestControlWithoutTheCarryTheSameRebuildLosesTheAttachment")
}
