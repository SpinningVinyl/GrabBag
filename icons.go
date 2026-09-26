//go:build linux

package main

import (
	"bytes"
	"embed"
	"image"
	"image/color"
	"image/draw"

	wl "github.com/SpinningVinyl/go-wayland/wayland/client"
	toplevelicon "github.com/SpinningVinyl/go-wayland/wayland/staging/xdg-toplevel-icon-v1"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

//go:embed assets/*.svg
var iconAssets embed.FS

//go:embed app-icon.svg
var appIconSVG []byte

type iconKey struct {
	name string
	size int
}

func (a *application) drawIcon(dst *image.RGBA, name string, x, y, size, scale int, tint color.RGBA) error {
	if a.iconCache == nil {
		a.iconCache = make(map[iconKey]*image.RGBA)
	}
	key := iconKey{name, size * scale}
	mask := a.iconCache[key]
	if mask == nil {
		data, err := iconAssets.ReadFile("assets/" + name + ".svg")
		if err != nil {
			return err
		}
		data = bytes.ReplaceAll(data, []byte("currentColor"), []byte("#ffffff"))
		icon, err := oksvg.ReadIconStream(bytes.NewReader(data))
		if err != nil {
			return err
		}
		mask = image.NewRGBA(image.Rect(0, 0, key.size, key.size))
		icon.SetTarget(0, 0, float64(key.size), float64(key.size))
		icon.Draw(rasterx.NewDasher(key.size, key.size, rasterx.NewScannerGV(key.size, key.size, mask, mask.Bounds())), 1)
		a.iconCache[key] = mask
	}
	point := image.Pt(x*scale, y*scale)
	draw.DrawMask(dst, image.Rectangle{Min: point, Max: point.Add(mask.Bounds().Size())}, image.NewUniform(tint), image.Point{}, mask, image.Point{}, draw.Over)
	return nil
}

func renderAppIcon(size int) (*image.RGBA, error) {
	icon, err := oksvg.ReadIconStream(bytes.NewReader(appIconSVG))
	if err != nil {
		return nil, err
	}
	frame := image.NewRGBA(image.Rect(0, 0, size, size))
	icon.SetTarget(0, 0, float64(size), float64(size))
	icon.Draw(rasterx.NewDasher(size, size, rasterx.NewScannerGV(size, size, frame, frame.Bounds())), 1)
	return frame, nil
}

func (a *application) setAppIcon(manager *toplevelicon.ToplevelIconManager, sizes []int) (err error) {
	icon, err := manager.CreateIcon()
	if err != nil {
		return err
	}
	var buffers []*wl.Buffer
	defer func() {
		if e := icon.Destroy(); err == nil {
			err = e
		}
		for _, buffer := range buffers {
			if e := buffer.Destroy(); err == nil {
				err = e
			}
		}
	}()
	if len(sizes) == 0 {
		sizes = []int{32, 64, 128}
	}
	seen := make(map[[2]int]bool)
	for _, size := range sizes {
		for scale := 1; scale <= 2; scale++ {
			pixels := size * scale
			key := [2]int{size, scale}
			if pixels > 512 || seen[key] {
				continue
			}
			seen[key] = true
			frame, e := renderAppIcon(pixels)
			if e != nil {
				return e
			}
			buffer, e := a.upload(frame)
			if e != nil {
				return e
			}
			buffers = append(buffers, buffer)
			if e := icon.AddBuffer(buffer, int32(scale)); e != nil {
				return e
			}
		}
	}
	return manager.SetIcon(a.top, icon)
}
