package trayicon

import (
	"encoding/binary"
	"testing"
)

func TestMenuIconHasOpaqueSystemBackground(t *testing.T) {
	for _, bg := range []uint32{0x00f0f0f0, 0x00202020} {
		icon := DotICO(0x004a9124, bg)
		if binary.LittleEndian.Uint32(icon[18:]) != 22 || binary.LittleEndian.Uint32(icon[22:]) != 40 {
			t.Fatal("not a DIB ICO")
		}
		pixels := icon[62 : 62+32*32*4]
		for i := 3; i < len(pixels); i += 4 {
			if pixels[i] != 255 {
				t.Fatal("transparent pixel would render black in systray")
			}
		}
		if pixels[0] != byte(bg>>16) || pixels[1] != byte(bg>>8) || pixels[2] != byte(bg) {
			t.Fatal("corner does not use menu background")
		}
		center := pixels[(16*32+16)*4:]
		if center[0] != 0x4a || center[1] != 0x91 || center[2] != 0x24 {
			t.Fatal("incorrect status dot colour")
		}
	}
}
