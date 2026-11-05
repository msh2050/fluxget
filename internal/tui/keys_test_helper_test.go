package tui

import "github.com/msh2050/fluxget/internal/config"

// Keys is a convenience alias for tests that need to reference keymap fields
// without constructing a full RootModel.
var Keys = *config.DefaultKeyMap()
