//go:build linux

package main

import (
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	wl "github.com/SpinningVinyl/go-wayland/wayland/client"
	"github.com/SpinningVinyl/go-wayland/wayland/cursor"
	xdg "github.com/SpinningVinyl/go-wayland/wayland/stable/xdg-shell"
	activation "github.com/SpinningVinyl/go-wayland/wayland/staging/xdg-activation-v1"
	toplevelicon "github.com/SpinningVinyl/go-wayland/wayland/staging/xdg-toplevel-icon-v1"
	"golang.org/x/sys/unix"
)

type application struct {
	display                                    *wl.Display
	compositor                                 *wl.Compositor
	shm                                        *wl.Shm
	wm                                         *xdg.WmBase
	seat                                       *wl.Seat
	pointer                                    *wl.Pointer
	keyboard                                   *wl.Keyboard
	manager                                    *wl.DataDeviceManager
	device                                     *wl.DataDevice
	activation                                 *activation.Activation
	iconManager                                *toplevelicon.ToplevelIconManager
	iconSizes                                  []int
	surface, cursor                            *wl.Surface
	cursorTheme                                *cursor.Theme
	cursorHotspotX, cursorHotspotY             int32
	xsurface                                   *xdg.Surface
	top                                        *xdg.Toplevel
	outputs                                    map[uint32]int
	entered                                    map[uint32]bool
	items                                      []entry
	iconCache                                  map[iconKey]*image.RGBA
	width, height, scale, scroll, focus        int
	x, y, pressX, pressY                       float64
	pressSerial                                uint32
	visible, configured, dirty, dragging, quit bool
	inFlight                                   int
	status                                     string
	err                                        error
}

func (a *application) check(err error) {
	if a.err == nil && err != nil {
		a.err = err
	}
}

func serveApp() error { return serveAppWith(nil, nil) }

func serveAppWith(initial *command, ready func()) error {
	dir, err := runtimeDir()
	if err != nil {
		return err
	}
	a := &application{width: 360, height: 360, scale: 1, outputs: map[uint32]int{}, entered: map[uint32]bool{}}
	if err := a.connect(); err != nil {
		return err
	}
	defer a.display.Context().Close()
	defer func() {
		if a.cursor != nil {
			_ = a.cursor.Destroy()
		}
		if a.cursorTheme != nil {
			_ = a.cursorTheme.Destroy()
		}
		if a.iconManager != nil {
			_ = a.iconManager.Destroy()
		}
	}()
	socket := filepath.Join(dir, "socket")
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if ready != nil {
		ready()
	}
	if initial != nil {
		if err := a.apply(*initial); err != nil {
			return err
		}
	}
	commands := make(chan invocation)
	done := make(chan struct{})
	defer close(done)
	go listenCommands(listener, commands, done)
	type event struct {
		id, opcode uint32
		fds        []int
		data       []byte
		err        error
	}
	events := make(chan event)
	// Only ReadMsg runs off-thread. All protocol objects and UI state stay on this loop.
	go func() {
		for {
			id, opcode, fds, data, err := a.display.Context().ReadMsg()
			select {
			case events <- event{id, opcode, fds, data, err}:
			case <-done:
				for _, fd := range fds {
					_ = unix.Close(fd)
				}
				return
			}
			if err != nil {
				return
			}
		}
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	for !a.quit && a.err == nil {
		select {
		case <-signals:
			a.quit = true
		case call := <-commands:
			call.result <- a.apply(call.cmd)
			if a.quit {
				<-call.answered
			} // Flush the --stop response before process exit.
		case e := <-events:
			if e.err != nil {
				return e.err
			}
			err := a.display.Context().DispatchMessage(e.id, e.opcode, e.fds, e.data)
			// A destroyed incoming offer may still have already-queued events.
			if !errors.Is(err, wl.ErrDispatchSenderNotFound) {
				a.check(err)
			}
		}
		if a.dirty && a.visible && a.configured && a.inFlight < 2 {
			a.check(a.paint())
		}
	}
	return a.err
}

func (a *application) connect() error {
	address := os.Getenv("WAYLAND_DISPLAY")
	if address == "" {
		address = "wayland-0"
	}
	if !filepath.IsAbs(address) {
		address = filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), address)
	}
	display, err := wl.Connect(address)
	if err != nil {
		return fmt.Errorf("connect to Wayland: %w", err)
	}
	a.display = display
	ok := false
	defer func() {
		if !ok {
			_ = display.Context().Close()
		}
	}()
	display.SetErrorHandler(func(e wl.DisplayErrorEvent) { a.check(fmt.Errorf("Wayland: %s (code %d)", e.Message, e.Code)) })
	registry, err := display.GetRegistry()
	if err != nil {
		return err
	}
	ctx := display.Context()
	registry.SetGlobalHandler(func(e wl.RegistryGlobalEvent) {
		var proxy wl.Proxy
		version := uint32(1)
		switch e.Interface {
		case "wl_compositor":
			a.compositor = wl.NewCompositor(ctx)
			proxy = a.compositor
			version = 4
		case "wl_shm":
			a.shm = wl.NewShm(ctx)
			proxy = a.shm
		case "xdg_wm_base":
			a.wm = xdg.NewWmBase(ctx)
			proxy = a.wm
			a.wm.SetPingHandler(func(e xdg.WmBasePingEvent) { a.check(a.wm.Pong(e.Serial)) })
		case "wl_seat":
			if a.seat != nil {
				return
			} // ponytail: one seat; add per-seat input state for multi-seat desktops.
			a.seat = wl.NewSeat(ctx)
			proxy = a.seat
			version = 5
			a.seat.SetCapabilitiesHandler(a.capabilities)
		case "wl_data_device_manager":
			a.manager = wl.NewDataDeviceManager(ctx)
			proxy = a.manager
			version = 3
		case "xdg_activation_v1":
			a.activation = activation.NewActivation(ctx)
			proxy = a.activation
		case "xdg_toplevel_icon_manager_v1":
			a.iconManager = toplevelicon.NewToplevelIconManager(ctx)
			proxy = a.iconManager
			a.iconManager.SetIconSizeHandler(func(e toplevelicon.ToplevelIconManagerIconSizeEvent) {
				if e.Size > 0 && e.Size <= 512 {
					a.iconSizes = append(a.iconSizes, int(e.Size))
				}
			})
		case "wl_output":
			output := wl.NewOutput(ctx)
			proxy = output
			version = 2
			a.outputs[output.ID()] = 1
			output.SetScaleHandler(func(e wl.OutputScaleEvent) {
				a.outputs[output.ID()] = max(1, int(e.Factor))
				a.updateScale()
			})
		default:
			return
		}
		if e.Version < version {
			a.check(fmt.Errorf("%s version %d required (compositor provides %d)", e.Interface, version, e.Version))
			return
		}
		a.check(registry.Bind(e.Name, e.Interface, version, proxy))
	})
	for range 2 {
		callback, err := display.Sync()
		if err != nil {
			return err
		}
		done := false
		callback.SetDoneHandler(func(wl.CallbackDoneEvent) { done = true })
		for !done && a.err == nil {
			a.check(ctx.Dispatch())
		}
		a.check(callback.Destroy())
		if a.err != nil {
			return a.err
		}
	}
	if a.compositor == nil || a.shm == nil || a.wm == nil || a.seat == nil || a.manager == nil {
		return errors.New("compositor lacks a required interface: wl_compositor, wl_shm, xdg_wm_base, wl_seat, or wl_data_device_manager")
	}
	a.device, err = a.manager.GetDataDevice(a.seat)
	if err != nil {
		return err
	}
	a.device.SetDataOfferHandler(func(e wl.DataDeviceDataOfferEvent) { a.check(e.Id.Destroy()) })
	a.surface, err = a.compositor.CreateSurface()
	if err != nil {
		return err
	}
	a.surface.SetEnterHandler(func(e wl.SurfaceEnterEvent) { a.entered[e.Output.ID()] = true; a.updateScale() })
	a.surface.SetLeaveHandler(func(e wl.SurfaceLeaveEvent) { delete(a.entered, e.Output.ID()); a.updateScale() })
	a.xsurface, err = a.wm.GetXdgSurface(a.surface)
	if err != nil {
		return err
	}
	a.xsurface.SetConfigureHandler(func(e xdg.SurfaceConfigureEvent) {
		a.check(a.xsurface.AckConfigure(e.Serial))
		a.configured = true
		a.dirty = true
	})
	a.top, err = a.xsurface.GetToplevel()
	if err != nil {
		return err
	}
	a.top.SetConfigureHandler(func(e xdg.ToplevelConfigureEvent) {
		if e.Width > 0 {
			a.width = int(e.Width)
		}
		if e.Height > 0 {
			a.height = int(e.Height)
		}
		a.dirty = true
	})
	a.top.SetCloseHandler(func(xdg.ToplevelCloseEvent) { a.hide() })
	if err := a.makeCursor(); err != nil {
		return err
	}
	ok = a.err == nil
	return a.err
}

func (a *application) apply(cmd command) error {
	if cmd.Stop {
		a.quit = true
		return nil
	}
	if cmd.Toggle {
		if a.visible {
			a.hide()
		} else {
			a.show(cmd.Token)
		}
		return a.err
	}
	paths := make([]string, 0, len(cmd.Paths))
	for _, path := range cmd.Paths {
		if !filepath.IsAbs(string(path)) {
			return errors.New("IPC paths must be absolute")
		}
		paths = append(paths, string(path))
	}
	paths, err := validatePaths(paths)
	if err != nil {
		return err
	}
	existing := make(map[string]bool, len(a.items))
	if cmd.Replace {
		a.items = nil
		a.scroll = 0
		a.focus = 0
	} else {
		for _, item := range a.items {
			existing[item.path] = true
		}
	}
	for _, path := range paths {
		if existing[path] {
			continue
		}
		a.items = append(a.items, entry{path, true})
	}
	a.status = ""
	a.dirty = true
	a.show(cmd.Token)
	return a.err
}

func (a *application) show(token string) {
	if !a.visible {
		a.check(a.top.SetTitle("Grab Bag"))
		a.check(a.top.SetAppId("grbg"))
		a.check(a.top.SetMinSize(320, 220))
		if a.iconManager != nil {
			a.check(a.setAppIcon(a.iconManager, a.iconSizes))
		}
		a.visible = true
		a.configured = false
		a.dirty = true
		a.check(a.surface.Commit()) // Empty initial commit; wait for xdg_surface.configure.
	}
	if token != "" && a.activation != nil {
		a.check(a.activation.Activate(token, a.surface))
	}
}

func (a *application) hide() {
	if !a.visible {
		return
	}
	a.visible = false
	a.configured = false
	a.pressSerial = 0
	a.check(a.surface.Attach(nil, 0, 0))
	a.check(a.surface.Commit())
}

func (a *application) updateScale() {
	scale := 1
	for id := range a.entered {
		scale = max(scale, a.outputs[id])
	}
	if a.scale != scale {
		a.scale = scale
		a.dirty = true
	}
}

func (a *application) capabilities(e wl.SeatCapabilitiesEvent) {
	var err error
	if e.Capabilities&uint32(wl.SeatCapabilityPointer) != 0 && a.pointer == nil {
		a.pointer, err = a.seat.GetPointer()
		a.check(err)
		if err != nil {
			return
		}
		a.pointer.SetEnterHandler(func(e wl.PointerEnterEvent) {
			a.x, a.y = e.SurfaceX, e.SurfaceY
			if a.cursor != nil {
				a.check(a.pointer.SetCursor(e.Serial, a.cursor, a.cursorHotspotX, a.cursorHotspotY))
			}
		})
		a.pointer.SetLeaveHandler(func(wl.PointerLeaveEvent) { a.pressSerial = 0 })
		a.pointer.SetMotionHandler(func(e wl.PointerMotionEvent) {
			a.x, a.y = e.SurfaceX, e.SurfaceY
			dx, dy := a.x-a.pressX, a.y-a.pressY
			if a.pressSerial != 0 && dx*dx+dy*dy >= 16 {
				a.startDrag(a.pressSerial)
				a.pressSerial = 0
			}
		})
		a.pointer.SetButtonHandler(a.button)
		a.pointer.SetAxisHandler(func(e wl.PointerAxisEvent) {
			if e.Axis == uint32(wl.PointerAxisVerticalScroll) {
				if e.Value > 0 {
					a.scroll++
				} else if e.Value < 0 {
					a.scroll--
				}
				a.clampScroll()
				a.dirty = true
			}
		})
	} else if e.Capabilities&uint32(wl.SeatCapabilityPointer) == 0 && a.pointer != nil {
		a.check(a.pointer.Release())
		a.pointer = nil
		a.pressSerial = 0
	}
	if e.Capabilities&uint32(wl.SeatCapabilityKeyboard) != 0 && a.keyboard == nil {
		a.keyboard, err = a.seat.GetKeyboard()
		a.check(err)
		if err != nil {
			return
		}
		// Navigation uses only layout-independent keys, not text input or letter shortcuts.
		a.keyboard.SetKeymapHandler(func(e wl.KeyboardKeymapEvent) { _ = unix.Close(e.Fd) })
		a.keyboard.SetKeyHandler(func(e wl.KeyboardKeyEvent) {
			if e.State != uint32(wl.KeyboardKeyStatePressed) {
				return
			}
			switch e.Key {
			case 1:
				a.hide()
			case 15:
				a.focus = (a.focus + 1) % (len(a.items) + 4)
			case 103:
				a.focus = max(4, a.focus-1)
				a.focus = min(a.focus, len(a.items)+3)
			case 108:
				a.focus = min(max(4, a.focus+1), len(a.items)+3)
			case 28, 57:
				a.activate(a.focus)
			}
			if a.focus >= 4 {
				row := a.focus - 4
				if row < a.scroll {
					a.scroll = row
				}
				if row >= a.scroll+a.rows() {
					a.scroll = row - a.rows() + 1
				}
			}
			a.dirty = true
		})
	} else if e.Capabilities&uint32(wl.SeatCapabilityKeyboard) == 0 && a.keyboard != nil {
		a.check(a.keyboard.Release())
		a.keyboard = nil
	}
}

func (a *application) button(e wl.PointerButtonEvent) {
	if e.Button != 0x110 {
		return
	}
	if e.State != uint32(wl.PointerButtonStatePressed) {
		a.pressSerial = 0
		return
	}
	x, y := int(a.x), int(a.y)
	switch {
	case y < 30:
		if x >= a.width-40 {
			a.focus = 3
			a.hide()
		} else {
			a.check(a.top.Move(a.seat, e.Serial))
		}
	case y >= a.height-16 && x >= a.width-16:
		a.check(a.top.Resize(a.seat, e.Serial, uint32(xdg.ToplevelResizeEdgeBottomRight)))
	case y >= 38 && y < 70:
		if x >= 12 && x < 140 {
			a.focus = 0
			a.activate(0)
		}
		if x >= 148 && x < 230 {
			a.focus = 1
			a.activate(1)
		}
	case y >= a.height-76 && y < a.height-36:
		a.focus = 2
		a.pressSerial = e.Serial
		a.pressX, a.pressY = a.x, a.y
	case y >= 80 && y < a.height-84:
		row := a.scroll + (y-80)/40
		if row < len(a.items) {
			a.focus = row + 4
			a.activate(a.focus)
		}
	}
	a.dirty = true
}

func (a *application) activate(focus int) {
	switch focus {
	case 0:
		all := len(a.items) > 0
		for _, item := range a.items {
			all = all && item.selected
		}
		for i := range a.items {
			a.items[i].selected = !all
		}
	case 1:
		a.items = nil
		a.scroll = 0
		a.focus = 1
	case 2:
		a.status = "Drag this handle with the pointer."
	case 3:
		a.hide()
	default:
		if i := focus - 4; i >= 0 && i < len(a.items) {
			a.items[i].selected = !a.items[i].selected
		}
	}
	a.dirty = true
}

func (a *application) startDrag(serial uint32) {
	if a.dragging {
		return
	}
	payload, err := fileURIs(a.items)
	if err != nil {
		a.status = err.Error()
		fmt.Fprintln(os.Stderr, err)
		a.dirty = true
		return
	}
	if payload == "" {
		a.status = "Select at least one file."
		a.dirty = true
		return
	}
	source, err := a.manager.CreateDataSource()
	a.check(err)
	if err != nil {
		return
	}
	source.SetSendHandler(func(e wl.DataSourceSendEvent) {
		// A slow recipient must not block the Wayland dispatch loop.
		go func() {
			_ = unix.SetNonblock(e.Fd, true)
			file := os.NewFile(uintptr(e.Fd), "drag-data")
			defer file.Close()
			_ = file.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if e.MimeType == "text/uri-list" {
				if _, err := io.WriteString(file, payload); err != nil {
					fmt.Fprintln(os.Stderr, "drag transfer:", err)
				}
			}
		}()
	})
	finish := func(message string) {
		a.check(source.Destroy())
		a.dragging = false
		a.status = message
		a.dirty = true
	}
	source.SetCancelledHandler(func(wl.DataSourceCancelledEvent) { finish("Drag cancelled.") })
	source.SetDndFinishedHandler(func(wl.DataSourceDndFinishedEvent) { finish("Files offered to destination.") })
	a.check(source.Offer("text/uri-list"))
	a.check(source.SetActions(uint32(wl.DataDeviceManagerDndActionCopy)))
	a.check(a.device.StartDrag(source, a.surface, nil, serial))
	a.dragging = true
	a.status = "Dragging selected files..."
	a.dirty = true
}
