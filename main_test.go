//go:build linux

package main

import (
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	wl "github.com/SpinningVinyl/go-wayland/wayland/client"
	"golang.org/x/sys/unix"
)

func TestInputAndDragPayload(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		delimiter   byte
		want        []string
	}{
		{"lines", " a b \n\nlast", '\n', []string{" a b ", "last"}},
		{"nul", "a\nb\x00back\\slash\x00last\x00", 0, []string{"a\nb", "back\\slash", "last"}},
		{"bytes", "bad\xffname\x00", 0, []string{"bad\xffname"}},
		{"empty", "", '\n', nil},
		{"long record", strings.Repeat("x", 70000), '\n', []string{strings.Repeat("x", 70000)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readPaths(strings.NewReader(tt.input), tt.delimiter)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	if _, err := readPaths(failedReader{}, '\n'); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error lost: %v", err)
	}
	dir := t.TempDir()
	var items []entry
	for _, name := range []string{"a space #%.png", "line\nbreak.png", "bad\xff.png"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		items = append(items, entry{path, true})
	}
	items = append(items, entry{filepath.Join(dir, "unselected-missing"), false})
	payload, err := fileURIs(items)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(payload, "\r\n"), "\r\n")
	if len(lines) != 3 {
		t.Fatalf("bad URI list: %q", payload)
	}
	for i, line := range lines {
		u, err := url.Parse(line)
		if err != nil || u.Scheme != "file" || u.Path != items[i].path {
			t.Fatalf("bad URI: %q (%v)", line, err)
		}
	}
	if err := os.Remove(items[0].path); err != nil {
		t.Fatal(err)
	}
	if _, err := fileURIs(items); err == nil {
		t.Fatal("missing selected file must fail")
	}
	app := &application{items: items}
	if err := app.apply(command{Replace: true, Paths: [][]byte{[]byte(items[0].path)}}); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	if !reflect.DeepEqual(app.items, items) {
		t.Fatal("invalid replacement changed the list")
	}
}

func TestDuplicateCanonicalPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	alias := filepath.Join(dir, "alias")
	other := filepath.Join(dir, "other")
	for _, name := range []string{path, other} {
		if err := os.WriteFile(name, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	got, err := validatePaths([]string{alias})
	if err != nil || !reflect.DeepEqual(got, []string{path}) {
		t.Fatalf("canonical path: %q, %v", got, err)
	}
	for _, paths := range [][]string{{path, path}, {path, alias}} {
		got, err := validatePaths(paths)
		if err != nil || !reflect.DeepEqual(got, []string{path}) {
			t.Fatalf("deduplicated paths: %q, %v", got, err)
		}
	}
	app := &application{items: []entry{{path, false}}, visible: true}
	if err := app.apply(command{Paths: [][]byte{[]byte(other), []byte(alias), []byte(other)}}); err != nil {
		t.Fatal(err)
	}
	if want := []entry{{path, false}, {other, true}}; !reflect.DeepEqual(app.items, want) {
		t.Fatalf("added items: %v; want %v", app.items, want)
	}
	if err := app.apply(command{Replace: true, Paths: [][]byte{[]byte(path), []byte(alias), []byte(other), []byte(other)}}); err != nil {
		t.Fatal(err)
	}
	if want := []entry{{path, true}, {other, true}}; !reflect.DeepEqual(app.items, want) {
		t.Fatalf("replaced items: %v; want %v", app.items, want)
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestCommandRoundTripAndStop(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "socket"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	defer close(done)
	commands := make(chan invocation)
	go listenCommands(listener, commands, done)
	for _, cmd := range []command{{Paths: [][]byte{[]byte("/a\xff\nfile")}, Replace: true}, {Stop: true}} {
		conn, err := net.Dial("unix", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() { result <- sendCommand(conn, cmd) }()
		call := <-commands
		if !reflect.DeepEqual(call.cmd, cmd) {
			t.Fatalf("command changed: %#v", call.cmd)
		}
		call.result <- nil
		<-call.answered
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
}

// Exercise the actual Wayland requests and SCM_RIGHTS transfer, without a GUI.
func TestDragTransfer(t *testing.T) {
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
	if err := server.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "a #\nfile")
	if err := os.WriteFile(path, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := display.Context()
	app := &application{display: display, manager: wl.NewDataDeviceManager(ctx), device: wl.NewDataDevice(ctx), surface: wl.NewSurface(ctx), items: []entry{{path, true}}}
	app.startDrag(123)
	if app.err != nil {
		t.Fatal(app.err)
	}
	readMessage := func() (uint32, uint32, []byte) {
		t.Helper()
		header := make([]byte, 8)
		if _, err := io.ReadFull(server, header); err != nil {
			t.Fatal(err)
		}
		body := make([]byte, int(wl.Uint32(header[4:])>>16)-8)
		if _, err := io.ReadFull(server, body); err != nil {
			t.Fatal(err)
		}
		return wl.Uint32(header), wl.Uint32(header[4:]) & 0xffff, body
	}
	id, opcode, body := readMessage()
	if id != app.manager.ID() || opcode != 0 || len(body) != 4 {
		t.Fatal("expected create_data_source")
	}
	sourceID := wl.Uint32(body)
	id, opcode, body = readMessage()
	if id != sourceID || opcode != 0 || wl.Uint32(body) != 14 || string(body[4:18]) != "text/uri-list\x00" {
		t.Fatalf("bad MIME offer: %q", body)
	}
	id, opcode, body = readMessage()
	if id != sourceID || opcode != 2 || wl.Uint32(body) != uint32(wl.DataDeviceManagerDndActionCopy) {
		t.Fatal("expected copy-only actions")
	}
	id, opcode, body = readMessage()
	if id != app.device.ID() || opcode != 0 || wl.Uint32(body) != sourceID || wl.Uint32(body[4:]) != app.surface.ID() || wl.Uint32(body[12:]) != 123 {
		t.Fatal("invalid start_drag")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := reader.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	message := make([]byte, 28)
	wl.PutUint32(message, sourceID)
	wl.PutUint32(message[4:], uint32(len(message))<<16|1) // wl_data_source.send
	wl.PutString(message[8:], "text/uri-list", 16)
	// Compositors batch events: the FD can arrive with an earlier non-FD event.
	target := make([]byte, 12)
	wl.PutUint32(target, sourceID)
	wl.PutUint32(target[4:], uint32(len(target))<<16) // wl_data_source.target(NULL)
	message = append(target, message...)
	_, _, err = server.WriteMsgUnix(message, unix.UnixRights(int(writer.Fd())), nil)
	writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := ctx.Dispatch(); err != nil {
		t.Fatal(err)
	}
	if err := ctx.Dispatch(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	want, err := fileURIs(app.items)
	if err != nil || string(got) != want {
		t.Fatalf("transfer %q; want %q (%v)", got, want, err)
	}
	// Two requests can also carry both descriptors in a single ancillary message.
	var readers, writers []*os.File
	var fds []int
	for range 2 {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if err := r.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		readers = append(readers, r)
		writers = append(writers, w)
		fds = append(fds, int(w.Fd()))
	}
	batch := append(append([]byte{}, message...), message[len(target):]...)
	_, _, err = server.WriteMsgUnix(batch, unix.UnixRights(fds...), nil)
	for _, w := range writers {
		w.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := ctx.Dispatch(); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range readers {
		got, err := io.ReadAll(r)
		if err != nil || string(got) != want {
			t.Fatalf("batched transfer %q; want %q (%v)", got, want, err)
		}
	}
	ctx.GetProxy(sourceID).(*wl.DataSource).Dispatch(4, -1, nil) // dnd_finished
	if app.dragging || ctx.GetProxy(sourceID) != nil {
		t.Fatal("drag source was not cleaned up")
	}
	app.activate(0)
	if app.items[0].selected {
		t.Fatal("deselect all failed")
	}
	app.activate(0)
	if !app.items[0].selected {
		t.Fatal("select all failed")
	}
	app.activate(1)
	if len(app.items) != 0 {
		t.Fatal("clear failed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("clear removed the file", err)
	}
}
