package claudebridge

import sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

func Extension() *sdk.Extension {
	ext := sdk.New("pig-claude-bridge")
	return ext
}
