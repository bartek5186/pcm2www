package trayicon

import (
	"encoding/binary"
	"math"
)

// DotICO is an opaque 32-bit DIB icon. systray converts menu icons through
// DrawIconEx onto an uninitialised bitmap, so transparent pixels turn black.
// Compositing the dot onto the Windows menu colour avoids that path entirely.
// Colours use Windows COLORREF (0x00BBGGRR).
func DotICO(foreground, background uint32) []byte {
	const size = 32
	const pixels = size * size * 4
	const mask = size * size / 8
	data := make([]byte, 22+40+pixels+mask)
	binary.LittleEndian.PutUint16(data[2:], 1)
	binary.LittleEndian.PutUint16(data[4:], 1)
	data[6], data[7] = size, size
	binary.LittleEndian.PutUint16(data[10:], 1)
	binary.LittleEndian.PutUint16(data[12:], 32)
	binary.LittleEndian.PutUint32(data[14:], 40+pixels+mask)
	binary.LittleEndian.PutUint32(data[18:], 22)
	dib := data[22:]
	binary.LittleEndian.PutUint32(dib, 40)
	binary.LittleEndian.PutUint32(dib[4:], size)
	binary.LittleEndian.PutUint32(dib[8:], size*2)
	binary.LittleEndian.PutUint16(dib[12:], 1)
	binary.LittleEndian.PutUint16(dib[14:], 32)
	binary.LittleEndian.PutUint32(dib[20:], pixels+mask)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			distance := math.Hypot(float64(x)+0.5-size/2, float64(y)+0.5-size/2)
			coverage := math.Max(0, math.Min(1, 11.5-distance))
			offset := 40 + ((size-1-y)*size+x)*4
			for channel := 0; channel < 3; channel++ {
				shift := uint((2 - channel) * 8)
				fg, bg := float64(byte(foreground>>shift)), float64(byte(background>>shift))
				dib[offset+channel] = byte(math.Round(bg + (fg-bg)*coverage))
			}
			dib[offset+3] = 255
		}
	}
	return data
}
