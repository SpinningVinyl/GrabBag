//go:build linux

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type command struct {
	// Bytes preserve Linux filenames that are not valid UTF-8 across JSON IPC.
	Paths                 [][]byte
	Replace, Toggle, Stop bool
	Token                 string
}

type entry struct {
	path     string
	selected bool
}

func readPaths(r io.Reader, delimiter byte) ([]string, error) {
	reader := bufio.NewReader(r)
	var paths []string
	for {
		path, err := reader.ReadString(delimiter)
		path = strings.TrimSuffix(path, string(delimiter))
		if path != "" {
			paths = append(paths, path)
		}
		if errors.Is(err, io.EOF) {
			return paths, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func validatePaths(paths []string) ([]string, error) {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%q is not a regular file", path)
		}
		result = append(result, absolute)
	}
	return result, nil
}

func fileURIs(items []entry) (string, error) {
	var paths []string
	for _, item := range items {
		if item.selected {
			paths = append(paths, item.path)
		}
	}
	paths, err := validatePaths(paths)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, path := range paths {
		b.WriteString((&url.URL{Scheme: "file", Path: path}).String())
		b.WriteString("\r\n")
	}
	return b.String(), nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "grbg:", err)
		os.Exit(1)
	}
}

func run() error {
	var cmd command
	zero := flag.Bool("0", false, "read NUL-separated paths from stdin (default: newline)")
	flag.BoolVar(&cmd.Replace, "replace", false, "replace the file list")
	flag.BoolVar(&cmd.Toggle, "toggle", false, "show or hide the window")
	flag.BoolVar(&cmd.Stop, "stop", false, "stop the background application")
	foreground := flag.Bool("foreground", false, "run the application in the foreground (debugging)")
	serve := flag.Bool("serve", false, "internal background worker")
	flag.Parse()
	if *serve {
		return serveApp()
	}
	if cmd.Stop && cmd.Toggle || (cmd.Stop || cmd.Toggle) && (cmd.Replace || len(flag.Args()) > 0 || *zero) {
		return errors.New("--stop and --toggle must be used without file arguments, --replace, or -0")
	}
	paths := flag.Args()
	if !cmd.Stop && !cmd.Toggle {
		info, err := os.Stdin.Stat()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeCharDevice == 0 {
			delimiter := byte('\n')
			if *zero {
				delimiter = 0
			}
			input, err := readPaths(os.Stdin, delimiter)
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}
			paths = append(paths, input...)
		}
	}
	paths, err := validatePaths(paths)
	if err != nil {
		return err
	}
	for _, path := range paths {
		cmd.Paths = append(cmd.Paths, []byte(path))
	}
	cmd.Token = os.Getenv("XDG_ACTIVATION_TOKEN")
	dir, err := runtimeDir()
	if err != nil {
		return err
	}
	// Serialize competing first invocations; the worker becomes ready before release.
	lock, err := os.OpenFile(filepath.Join(dir, "start.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	socket := filepath.Join(dir, "socket")
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err == nil {
		defer conn.Close()
		return sendCommand(conn, cmd)
	}
	if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ECONNREFUSED) {
		return err
	}
	if cmd.Stop {
		return nil
	}
	if *foreground {
		// Release the startup lock once the listener exists, inside serveAppWith.
		return serveAppWith(&cmd, func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) })
	}
	logPath := filepath.Join(dir, "log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	worker := exec.Command(exe, "--serve")
	worker.Stdout, worker.Stderr = log, log
	worker.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := worker.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- worker.Wait() }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-exited:
			data, _ := os.ReadFile(logPath)
			return fmt.Errorf("background startup failed: %s", strings.TrimSpace(string(data)))
		case <-deadline.C:
			_ = worker.Process.Kill()
			return fmt.Errorf("startup timed out; see %s", logPath)
		case <-ticker.C:
			conn, err = net.DialTimeout("unix", socket, 100*time.Millisecond)
			if err == nil {
				defer conn.Close()
				return sendCommand(conn, cmd)
			}
		}
	}
}

func runtimeDir() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		return "", errors.New("XDG_RUNTIME_DIR is unset; run inside a Wayland desktop session")
	}
	display := os.Getenv("WAYLAND_DISPLAY")
	if display == "" {
		display = "wayland-0"
	}
	sum := sha256.Sum256([]byte(display))
	dir := filepath.Join(base, fmt.Sprintf("grbg-%x", sum[:6]))
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Getuid()) {
		return "", fmt.Errorf("%s must be a private directory owned by the current user", dir)
	}
	return dir, nil
}

func sendCommand(conn net.Conn, cmd command) error {
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	if err := json.NewEncoder(conn).Encode(cmd); err != nil {
		return err
	}
	var response string
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return err
	}
	if response != "" {
		return errors.New(response)
	}
	return nil
}

type invocation struct {
	cmd      command
	result   chan error
	answered chan struct{}
}

func listenCommands(listener net.Listener, commands chan<- invocation, done <-chan struct{}) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			var cmd command
			err := json.NewDecoder(io.LimitReader(conn, 16<<20)).Decode(&cmd)
			if err == nil {
				call := invocation{cmd, make(chan error, 1), make(chan struct{})}
				select {
				case commands <- call:
				case <-done:
					return
				}
				// Every accepted call receives a result, including --stop, before shutdown.
				err = <-call.result
				defer close(call.answered)
			}
			message := ""
			if err != nil {
				message = err.Error()
			}
			_ = json.NewEncoder(conn).Encode(message)
		}()
	}
}
