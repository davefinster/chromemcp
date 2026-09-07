package main

// A headful session's display: one Xvnc (TigerVNC) per session, which is
// both the X server Chrome renders into and the VNC server the live view
// connects to, on a unix socket inside the session directory. No window
// manager: Chrome is the only client and is told where and how big to be.

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type display struct {
	cmd    *exec.Cmd
	Number int    // X display number; DISPLAY is ":N"
	Net    string // "unix" or "tcp": how the VNC server is reached
	Addr   string // the socket path, or 127.0.0.1:port
	done   chan struct{}
}

func (d *display) Display() string { return ":" + strconv.Itoa(d.Number) }

// vncSocketDir is where the unix sockets go: a short path, because a
// socket path is limited to about a hundred bytes and a session directory
// under a deep TMPDIR is not.
func vncSocketDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("chromemcp-vnc-%d", os.Getuid()))
}

// startDisplay launches Xvnc for a session, WxH pixels, and waits for it
// to be ready to accept clients.
func startDisplay(ctx context.Context, xvnc, id string, w, h int, verbose bool) (*display, error) {
	d := &display{done: make(chan struct{})}
	args := []string{
		"-displayfd", "3",
		"-geometry", fmt.Sprintf("%dx%d", w, h),
		"-depth", "24",
		"-SecurityTypes", "None",
		"-AlwaysShared",
		"-desktop", "chromemcp",
		"-nolisten", "tcp",
		"-ac",
	}
	sock := filepath.Join(vncSocketDir(), id+".sock")
	if len(sock) < 100 && os.MkdirAll(filepath.Dir(sock), 0o700) == nil {
		os.Remove(sock)
		d.Net, d.Addr = "unix", sock
		args = append(args, "-rfbunixpath", sock, "-rfbport", "-1") // no TCP listener
	} else {
		port, err := freePort()
		if err != nil {
			return nil, err
		}
		d.Net, d.Addr = "tcp", fmt.Sprintf("127.0.0.1:%d", port)
		args = append(args, "-rfbport", strconv.Itoa(port), "-localhost")
	}
	// Xvnc writes the display number it picked to this pipe once it is
	// listening, which is also the readiness signal.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	cmd := exec.Command(xvnc, args...)
	cmd.ExtraFiles = []*os.File{pw}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if verbose {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		pw.Close()
		return nil, fmt.Errorf("starting Xvnc: %w", err)
	}
	pw.Close()
	d.cmd = cmd
	go func() {
		cmd.Wait()
		close(d.done)
	}()

	numc := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(pr).ReadString('\n')
		numc <- strings.TrimSpace(line)
	}()
	select {
	case line := <-numc:
		n, err := strconv.Atoi(line)
		if err != nil {
			d.stop()
			return nil, fmt.Errorf("Xvnc reported display %q", line)
		}
		d.Number = n
	case <-d.done:
		return nil, fmt.Errorf("Xvnc exited during startup")
	case <-ctx.Done():
		d.stop()
		return nil, ctx.Err()
	case <-time.After(20 * time.Second):
		d.stop()
		return nil, fmt.Errorf("Xvnc did not come up within 20s")
	}
	// The socket usually exists by now; give it a moment if not.
	for i := 0; i < 50 && d.Net == "unix"; i++ {
		if _, err := os.Stat(d.Addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return d, nil
}

func (d *display) Alive() bool {
	select {
	case <-d.done:
		return false
	default:
		return true
	}
}

func (d *display) stop() {
	if d.cmd.Process != nil {
		syscall.Kill(-d.cmd.Process.Pid, syscall.SIGTERM)
	}
	select {
	case <-d.done:
	case <-time.After(3 * time.Second):
		if d.cmd.Process != nil {
			syscall.Kill(-d.cmd.Process.Pid, syscall.SIGKILL)
		}
		<-d.done
	}
	if d.Net == "unix" {
		os.Remove(d.Addr)
	}
}

// freePort asks the kernel for an unused loopback TCP port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
