//go:build windows && !dev

package main

import (
	_ "embed"

	"github.com/bartek5186/pcm2www/internal/syncer"
	"github.com/lxn/walk"
)

//go:embed assets/status-running.ico
var runningStatusIcon []byte

//go:embed assets/status-starting.ico
var startingStatusIcon []byte

//go:embed assets/status-stopped.ico
var stoppedStatusIcon []byte

func statusAppearance(state syncer.StatusState) (walk.Color, []byte) {
	switch state {
	case syncer.StatusRunning:
		return walk.RGB(36, 145, 74), runningStatusIcon
	case syncer.StatusStarting:
		return walk.RGB(178, 116, 0), startingStatusIcon
	default:
		return walk.RGB(190, 48, 48), stoppedStatusIcon
	}
}
