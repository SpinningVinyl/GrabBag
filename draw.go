//go:build linux

package main

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	wl "github.com/SpinningVinyl/go-wayland/wayland/client"
	"github.com/SpinningVinyl/go-wayland/wayland/cursor"
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
	icon := func(name string, x, y, size int, tint color.RGBA) {
		a.check(a.drawIcon(frame, name, x, y, size, scale, tint))
	}
	rect(0, 0, w, h, bg)
	rect(0, 0, w, 30, panel)
	text(12, 21, w-60, "Grab Bag", fg)
	icon("x", w-32, 3, 24, fg)
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
		if item.selected {
			icon("square-check", 13, y+7, 24, accent)
		} else {
			icon("square", 13, y+7, 24, muted)
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
	rect(12, h-76, w-24, 40, panel)
	if a.focus == 2 {
		rect(12, h-38, w-24, 2, accent)
	}
	icon("grip-vertical", 20, h-68, 24, fg)
	text(52, h-51, w-76, label, fg)
	status := a.status
	if status == "" {
		status = "Tab: focus   Space: select   Esc: hide"
	}
	text(12, h-14, w-34, status, muted)
	icon("dots-diagonal", w-24, h-24, 24, muted)
	if a.err != nil {
		return a.err
	}
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
	name, size := cursorSettings()
	theme, err := cursor.LoadTheme(name, size, a.shm)
	if err == nil {
		arrow := theme.GetCursor(cursor.LeftPtr)
		if arrow == nil {
			arrow = theme.GetCursor("default")
		}
		if arrow != nil && len(arrow.Images) > 0 {
			img := &arrow.Images[0]
			buffer, err := img.GetBuffer()
			if err != nil {
				_ = theme.Destroy()
				return err
			}
			a.cursorTheme = theme
			a.cursorHotspotX, a.cursorHotspotY = int32(img.HotspotX), int32(img.HotspotY)
			a.check(a.cursor.Attach(buffer, 0, 0))
			a.check(a.cursor.Commit())
			return a.err
		}
		_ = theme.Destroy()
	}
	frame := image.NewRGBA(image.Rect(0, 0, 20, 26))
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
	a.cursorHotspotX, a.cursorHotspotY = 1, 1
	a.check(a.cursor.Attach(buffer, 0, 0))
	a.check(a.cursor.Commit())
	return a.err
}

func cursorSettings() (string, int) {
	name := os.Getenv("XCURSOR_THEME")
	size := cursorSize(os.Getenv("XCURSOR_SIZE"))
	if strings.Contains(os.Getenv("XDG_CURRENT_DESKTOP"), "KDE") || os.Getenv("KDE_FULL_SESSION") == "true" {
		if name == "" {
			name = kdeCursorSetting("cursorTheme")
			if name == "" {
				name = "breeze_cursors"
			}
		}
		if size == 0 {
			size = cursorSize(kdeCursorSetting("cursorSize"))
		}
	} else if strings.Contains(os.Getenv("XDG_CURRENT_DESKTOP"), "GNOME") {
		if name == "" {
			name = strings.Trim(gnomeCursorSetting("cursor-theme"), "'")
		}
		if size == 0 {
			size = cursorSize(gnomeCursorSetting("cursor-size"))
		}
	}
	if size == 0 {
		size = 24
	}
	return name, size
}

func gnomeCursorSetting(key string) string {
	value, err := exec.Command("gsettings", "get", "org.gnome.desktop.interface", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(value))
}

func cursorSize(value string) int {
	size, err := strconv.Atoi(value)
	if err != nil || size < 1 || size > 256 {
		return 0
	}
	return size
}

func kdeCursorSetting(key string) string {
	value, err := exec.Command("kreadconfig6", "--file", "kcminputrc", "--group", "Mouse", "--key", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(value))
}
