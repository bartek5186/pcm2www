//go:build windows && !dev

package main

import (
	"sync"

	"github.com/bartek5186/pcm2www/internal/syncer"
	"github.com/bartek5186/pcm2www/internal/trayicon"
	"github.com/lxn/walk"
	"github.com/lxn/win"
)

var statusIcons sync.Map

func statusAppearance(state syncer.StatusState) (walk.Color, []byte) {
	color := walk.RGB(190, 48, 48)
	switch state {
	case syncer.StatusRunning:
		color = walk.RGB(36, 145, 74)
	case syncer.StatusStarting:
		color = walk.RGB(178, 116, 0)
	}
	key := [2]uint32{uint32(color), statusMenuBackground()}
	icon, ok := statusIcons.Load(key)
	if !ok {
		icon, _ = statusIcons.LoadOrStore(key, trayicon.DotICO(key[0], key[1]))
	}
	return color, icon.([]byte)
}

func statusMenuBackground() uint32 { return win.GetSysColor(win.COLOR_MENU) }
