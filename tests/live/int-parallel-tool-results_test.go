//go:build live

package live

import "testing"

// Port of tests/int-parallel-tool-results.mjs: a rebuilt parallel tool call keeps every result on disk, and CC
// resumes it with Claude able to read all three results back. The test body is in the extension module
// (internal/ccproto/live_parallel_results_test.go) because it calls the conversion and session code directly.
func TestIntParallelToolResults(t *testing.T) {
	runExtensionLiveTests(t, "^(TestARebuiltParallelToolCallKeepsEveryResultOnDisk|TestCCResumesItAndClaudeCanReadAllThreeResultsBack)$")
}
