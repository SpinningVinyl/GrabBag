//go:build linux

package main

import (
	"image"
	"image/color"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	wl "github.com/SpinningVinyl/go-wayland/wayland/client"
	xdg "github.com/SpinningVinyl/go-wayland/wayland/stable/xdg-shell"
	toplevelicon "github.com/SpinningVinyl/go-wayland/wayland/staging/xdg-toplevel-icon-v1"
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

func TestAppIconRenders(t *testing.T) {
	for _, size := range []int{32, 128, 512} {
		frame, err := renderAppIcon(size)
		if err != nil {
			t.Fatal(err)
		}
		visible := 0
		for i := 3; i < len(frame.Pix); i += 4 {
			if frame.Pix[i] != 0 {
				visible++
			}
		}
		if visible < size*size/10 {
			t.Errorf("%dpx app icon rendered only %d visible pixels", size, visible)
		}
	}
}

func TestToplevelPropertiesRestoredAfterRemap(t *testing.T) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "wayland"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	display, err := wl.Connect(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer display.Context().Close()
	server, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	server.SetDeadline(time.Now().Add(3 * time.Second))
	ctx := display.Context()
	a := &application{
		top: xdg.NewToplevel(ctx), surface: wl.NewSurface(ctx), shm: wl.NewShm(ctx),
		iconManager: toplevelicon.NewToplevelIconManager(ctx), iconSizes: []int{16},
	}
	a.show("")
	a.hide()
	a.show("")
	if a.err != nil {
		t.Fatal(a.err)
	}
	var commits, titles, appIDs, minSizes, icons int
	for commits < 3 {
		header := make([]byte, 8)
		if _, err := io.ReadFull(server, header); err != nil {
			t.Fatal(err)
		}
		word := wl.Uint32(header[4:])
		body := make([]byte, int(word>>16)-8)
		if _, err := io.ReadFull(server, body); err != nil {
			t.Fatal(err)
		}
		id, opcode := wl.Uint32(header), word&0xffff
		switch {
		case id == a.top.ID() && opcode == 2:
			titles++
		case id == a.top.ID() && opcode == 3:
			appIDs++
		case id == a.top.ID() && opcode == 8:
			minSizes++
		case id == a.iconManager.ID() && opcode == 2:
			icons++
		case id == a.surface.ID() && opcode == 6:
			commits++
			want := 1
			if commits == 3 {
				want = 2
			}
			if titles != want || appIDs != want || minSizes != want || icons != want {
				t.Fatalf("commit %d: title=%d appID=%d minSize=%d icon=%d; want %d each", commits, titles, appIDs, minSizes, icons, want)
			}
		}
	}
}
