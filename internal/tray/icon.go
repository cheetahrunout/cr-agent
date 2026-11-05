package tray

import (
	"math"

	"github.com/cheetahrunout/cr-agent/internal/status"
)

// iconSize is the edge of the generated icons; Windows scales them to the tray.
const iconSize = 32

type rgb struct{ r, g, b float64 }

var (
	green = rgb{0x2E, 0x9E, 0x62}
	amber = rgb{0xD9, 0x8A, 0x1E}
	grey  = rgb{0x8E, 0x93, 0x9B}
	white = rgb{0xFF, 0xFF, 0xFF}
)

// iconPixels draws a state's icon as premultiplied BGRA, top row first. The shapes differ, so the
// state is readable without colour (§3.5.6): a filled dot (available), a half-filled dot (in use),
// a hollow grey dot (paused, not paired), a grey dot with an exclamation mark (needs attention) and
// a broken ring (connecting).
func iconPixels(kind status.Icon) []byte {
	const n, ss = iconSize, 4
	px := make([]byte, n*n*4)
	c := float64(n) / 2
	rOut, rIn := c-2, c-7.5
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			var a, r, g, b float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					fx := float64(x) + (float64(sx)+0.5)/ss - c
					fy := float64(y) + (float64(sy)+0.5)/ss - c
					d := math.Hypot(fx, fy)
					col, on := rgb{}, false
					switch kind {
					case status.IconGreen:
						col, on = green, d <= rOut
					case status.IconAmber: // ring with the lower half filled
						col, on = amber, d <= rOut && (d >= rIn || fy >= 0)
					case status.IconGrey: // hollow
						col, on = grey, d <= rOut && d >= rIn
					case status.IconGreyBang:
						col, on = grey, d <= rOut
						if on && math.Abs(fx) <= 1.8 && ((fy >= -8 && fy <= 2.5) || (fy >= 5 && fy <= 8.5)) {
							col = white
						}
					case status.IconRing: // three quarters of a ring
						col, on = grey, d <= rOut && d >= rIn && !(fx > 0 && fy < 0)
					}
					if on {
						a++
						r += col.r
						g += col.g
						b += col.b
					}
				}
			}
			i := (y*n + x) * 4
			const samples = ss * ss
			px[i+0] = byte(b / samples) // premultiplied: the sum already carries the coverage
			px[i+1] = byte(g / samples)
			px[i+2] = byte(r / samples)
			px[i+3] = byte(a * 255 / samples)
		}
	}
	return px
}
