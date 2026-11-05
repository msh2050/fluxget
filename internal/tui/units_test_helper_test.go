package tui

import (
	"github.com/msh2050/fluxget/internal/types"
	"github.com/msh2050/fluxget/internal/utils"
)

// Unit constants re-exported for use in test files within this package.
const (
	KB                    = utils.KiB
	MB                    = utils.MiB
	GB                    = utils.GiB
	ProgressChannelBuffer = types.ProgressChannelBuffer
)
