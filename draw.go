//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"
	"strings"

	wl "github.com/SpinningVinyl/go-wayland/wayland/client"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/sys/unix"
)

func (a *application) rows() int { return max(1, (a.height-164)/40) }

func (a *application) clampScroll() { a.scroll = max(0, min(a.scroll, len(a.items)-a.rows())) }

func (a *application) paint() error {
	w, h, scale := a.width, a.height, a.scale
	if w <= 0 || h <= 0 || scale <= 0 || int64(w)*int64(h)*int64(scale)*int64(scale) > 32<<20 {
		return fmt.Errorf("unsupported window dimensions %dx%d at scale %d", w, h, scale)
	}
	a.clampScroll()
	frame := image.NewRGBA(image.Rect(0, 0, w*scale, h*scale))
	rect := func(x, y, width, height int, c color.RGBA) {
		draw.Draw(frame, image.Rect(x*scale, y*scale, (x+width)*scale, (y+height)*scale), image.NewUniform(c), image.Point{}, draw.Src)
	}
	ttf, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return err
	}
	face, err := opentype.NewFace(ttf, &opentype.FaceOptions{Size: 14 * float64(scale), DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return err
	}
	defer face.Close()
	text := func(x, baseline, maxWidth int, s string, c color.RGBA) {
		s = strings.NewReplacer("\n", "\\n", "\r", "\\r", "\t", "\\t").Replace(s)
		limit := fixed.I(maxWidth * scale)
		if font.MeasureString(face, s) > limit {
			limit -= font.MeasureString(face, "...")
			var width fixed.Int26_6
			end := 0
			for i, r := range s {
				advance, _ := face.GlyphAdvance(r)
				if width+advance > limit {
					break
				}
				width += advance
				end = i + len(string(r))
			}
			s = s[:end] + "..."
		}
		d := font.Drawer{Dst: frame, Src: image.NewUniform(c), Face: face, Dot: fixed.P(x*scale, baseline*scale)}
		d.DrawString(s)
	}
	bg := color.RGBA{24, 28, 35, 255}
	panel := color.RGBA{40, 47, 58, 255}
	fg := color.RGBA{238, 242, 247, 255}
	muted := color.RGBA{177, 188, 203, 255}
	accent := color.RGBA{98, 195, 238, 255}
	rect(0, 0, w, h, bg)
	rect(0, 0, w, 30, panel)
	text(12, 21, w-60, "Grab Bag", fg)
	text(w-28, 21, 24, "x", fg)
	if a.focus == 3 {
		rect(w-38, 28, 36, 2, accent)
	}
	button := func(x, y, width, height, focus int, label string) {
		rect(x, y, width, height, panel)
		if a.focus == focus {
			rect(x, y+height-2, width, 2, accent)
		}
		text(x+10, y+height/2+5, width-20, label, fg)
	}
	selected := 0
	for _, item := range a.items {
		if item.selected {
			selected++
		}
	}
	selectLabel := "Select all"
	if len(a.items) > 0 && selected == len(a.items) {
		selectLabel = "Deselect all"
	}
	button(12, 38, 128, 32, 0, selectLabel)
	button(148, 38, 82, 32, 1, "Clear")
	text(242, 59, w-254, fmt.Sprintf("%d / %d", selected, len(a.items)), muted)
	for row := 0; row < a.rows() && a.scroll+row < len(a.items); row++ {
		i := a.scroll + row
		item := a.items[i]
		y := 80 + row*40
		if a.focus == i+4 {
			rect(8, y, w-20, 38, panel)
			rect(8, y, 2, 38, accent)
		}
		rect(16, y+10, 18, 18, muted)
		rect(18, y+12, 14, 14, bg)
		if item.selected {
			text(20, y+24, 16, "x", accent)
		}
		text(44, y+15, w-66, filepath.Base(item.path), fg)
		text(44, y+32, w-66, filepath.Dir(item.path), muted)
	}
	if len(a.items) == 0 {
		text(16, 106, w-32, "Add files: grbg file1 file2 ...", muted)
	}
	if len(a.items) > a.rows() {
		track := a.rows() * 40
		thumb := max(8, track*a.rows()/len(a.items))
		rect(w-8, 80+((track-thumb)*a.scroll/max(1, len(a.items)-a.rows())), 3, thumb, accent)
	}
	label := fmt.Sprintf("Drag %d selected file(s)", selected)
	button(12, h-76, w-24, 40, 2, label)
	status := a.status
	if status == "" {
		status = "Tab: focus   Space: select   Esc: hide"
	}
	text(12, h-14, w-34, status, muted)
	text(w-16, h-4, 16, "/", muted)
	buffer, err := a.upload(frame)
	if err != nil {
		return err
	}
	a.inFlight++
	buffer.SetReleaseHandler(func(wl.BufferReleaseEvent) { a.inFlight--; a.check(buffer.Destroy()) })
	a.check(a.surface.SetBufferScale(int32(scale)))
	a.check(a.surface.Attach(buffer, 0, 0))
	a.check(a.surface.Damage(0, 0, int32(w), int32(h)))
	a.check(a.surface.Commit())
	a.dirty = false
	return a.err
}

func (a *application) upload(frame *image.RGBA) (*wl.Buffer, error) {
	fd, err := unix.MemfdCreate("grbg-frame", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "grbg-frame")
	defer file.Close()
	// wl_shm ARGB8888 is a native-endian packed integer, unlike image.RGBA bytes.
	for i := 0; i < len(frame.Pix); i += 4 {
		r, g, b, alpha := frame.Pix[i], frame.Pix[i+1], frame.Pix[i+2], frame.Pix[i+3]
		binary.NativeEndian.PutUint32(frame.Pix[i:i+4], uint32(alpha)<<24|uint32(r)<<16|uint32(g)<<8|uint32(b))
	}
	if _, err := file.Write(frame.Pix); err != nil {
		return nil, err
	}
	pool, err := a.shm.CreatePool(fd, int32(len(frame.Pix)))
	if err != nil {
		return nil, err
	}
	defer func() { a.check(pool.Destroy()) }()
	buffer, err := pool.CreateBuffer(0, int32(frame.Rect.Dx()), int32(frame.Rect.Dy()), int32(frame.Stride), uint32(wl.ShmFormatArgb8888))
	return buffer, err
}

func (a *application) makeCursor() error {
	var err error
	a.cursor, err = a.compositor.CreateSurface()
	if err != nil {
		return err
	}
	frame := image.NewRGBA(image.Rect(0, 0, 20, 26))
	// for now we're jsut drawing a static cursor at 1x;
    // TODO: use cursor themes/scaled cursor buffers for polish.
	for y := 1; y < 22; y++ {
		for x := 1; x <= min(y/2+1, 10); x++ {
			c := color.RGBA{245, 245, 245, 255}
			if x == 1 || x == y/2+1 || y == 21 {
				c = color.RGBA{20, 20, 20, 255}
			}
			frame.SetRGBA(x, y, c)
		}
	}
	buffer, err := a.upload(frame)
	if err != nil {
		return err
	}
	buffer.SetReleaseHandler(func(wl.BufferReleaseEvent) { a.check(buffer.Destroy()) })
	a.check(a.cursor.Attach(buffer, 0, 0))
	a.check(a.cursor.Commit())
	return a.err
}
