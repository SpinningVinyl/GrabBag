//go:build linux

package main

import (
	"image"
	"image/color"
	"testing"
)

func TestIconsRenderAtOutputScale(t *testing.T) {
	a := &application{}
	for _, name := range []string{"x", "square", "square-check", "grip-vertical", "dots-diagonal"} {
		frame := image.NewRGBA(image.Rect(0, 0, 48, 48))
		if err := a.drawIcon(frame, name, 0, 0, 24, 2, color.RGBA{255, 255, 255, 255}); err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		visible := 0
		for i := 3; i < len(frame.Pix); i += 4 {
			if frame.Pix[i] != 0 {
				visible++
			}
		}
		if visible < 10 {
			t.Errorf("%s rendered only %d visible pixels", name, visible)
		}
	}
}
