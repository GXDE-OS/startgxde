/*
 * Copyright (C) 2014 ~ 2024 Deepin Technology Co., Ltd.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTLESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// maybeRestartDbusDaemon repairs the session bus state when startdde is launched
// into a Wayland session but the previous session was X11 (e.g. log out of X11
// and log back into Wayland, or switch session types). The session dbus-daemon
// is normally launched with --systemd-activation, so dbus-activated programs
// inherit their environment from systemd --user rather than from dbus-daemon's
// own activation environment. Because systemd --user persists across logout, its
// environment still carries X11 leftovers (notably QT_QPA_PLATFORM=xcb) which
// break Wayland programs.
//
// What must be fixed (both before any shared session-bus connection is
// established, since dbus.SessionBus caches a process-wide singleton):
//   1. Clear the stale X11 variables from the activation environments (both the
//      dbus-daemon's and systemd --user's).
//   2. Optionally restart a non-systemd (dbus-launch) daemon if it was started
//      under X11. This is skipped when the bus is the systemd-managed user bus
//      (unix:path=/run/user/<uid>/bus), which must NOT be replaced.
//
// The implementation deliberately avoids blocking D-Bus calls to a possibly
// stale daemon: it inspects /proc for the daemon and shell-outs to external
// helpers, all guarded by timeouts.
func maybeRestartDbusDaemon() {
	if os.Getenv("XDG_SESSION_TYPE") != "wayland" {
		// Only Wayland sessions need a clean environment.
		return
	}

	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	isSystemdBus := strings.HasPrefix(addr, "unix:path=") &&
		strings.Contains(addr, "/run/user/") &&
		strings.HasSuffix(addr, "/bus")

	// Step 1: publish a clean Wayland environment into systemd --user BEFORE
	// restarting the bus, so the freshly started bus daemon inherits the clean
	// environment instead of the stale X11 one.
	republishWaylandActivationEnv()

	// Step 2: restart the bus daemon if it still carries X11 env in its own
	// process environment (the environment inherited by dbus-activated
	// Exec= services such as dde-osd).
	if !isSystemdBus {
		// dbus-launch-style session bus: replace it directly.
		pid, err := findSessionDbusDaemonPID(addr)
		if err != nil {
			logger.Warning("failed to locate session dbus-daemon:", err)
		} else if pid != 0 && isX11EnvDaemon(pid) {
			logger.Info("detected dbus-daemon started under X11 session, " +
				"restarting it for the Wayland session")
			restartDbusDaemon(pid)
		}
	} else {
		// systemd-managed user bus (unix:path=/run/user/<uid>/bus). The bus
		// daemon process persists across logout, so its OWN environment
		// (inherited from the X11 session it was started in) still carries
		// QT_QPA_PLATFORM=xcb. dbus-activated programs launched via an
		// Exec= service inherit that process environment, not the dbus
		// activation state nor the systemd environment -- which is exactly
		// why dde-osd keeps getting xcb after an X11->Wayland switch. The
		// only fix is to restart the bus daemon itself through systemd.
		if pid, err := findSessionDbusDaemonPID(addr); err == nil && pid != 0 {
			if isX11EnvDaemon(pid) {
				logger.Info("session bus daemon carries X11 env, " +
					"restarting dbus.service for the Wayland session")
				restartSystemdUserBus()
			}
		} else if err != nil {
			logger.Warning("failed to locate session dbus-daemon:", err)
		}
	}

	// Also fix startdde's own process environment. Programs started directly
	// by startdde (autostart entries, launch groups, ...) inherit this
	// environment rather than the dbus/systemd activation environment, so a
	// leaked QT_QPA_PLATFORM=xcb here would still break them under Wayland.
	sanitizeOwnEnvironment()
}

// sanitizeOwnEnvironment makes sure startdde does not carry an X11-only
// QT_QPA_PLATFORM into the Wayland session. Any value other than "wayland"
// (notably the stale "xcb") is reset so Qt auto-detects the Wayland backend
// (WAYLAND_DISPLAY is already set by the session).
func sanitizeOwnEnvironment() {
	if v := os.Getenv("QT_QPA_PLATFORM"); v != "" && v != "wayland" {
		logger.Infof("resetting own QT_QPA_PLATFORM from %q to wayland", v)
		os.Setenv("QT_QPA_PLATFORM", "wayland")
	}
}

// republishWaylandActivationEnv overwrites the session activation environment
// so that dbus/systemd-activated programs no longer inherit the stale
// QT_QPA_PLATFORM=xcb from a previous X11 session. The --systemd flag makes
// dbus-update-activation-environment propagate the change to systemd --user as
// well (the bus is launched with --systemd-activation, so activated units read
// from there). As a fallback, systemctl --user is used directly.
func republishWaylandActivationEnv() {
	// Preferred: a single command fixes both the dbus and the systemd layers.
	if _, err := exec.LookPath("dbus-update-activation-environment"); err == nil {
		runCmdWithTimeout(10*time.Second, "dbus-update-activation-environment",
			"--systemd",
			"QT_QPA_PLATFORM=wayland",
			"XDG_SESSION_TYPE=wayland")
	}

	// Belt-and-suspenders: also explicitly unset the X11 variable from the
	// systemd --user environment in case the command above did not take effect
	// for that layer (e.g. the bus address was not yet usable).
	if _, err := exec.LookPath("systemctl"); err == nil {
		runCmdWithTimeout(10*time.Second, "systemctl", "--user",
			"unset-environment", "QT_QPA_PLATFORM")
	}
}

// findSessionDbusDaemonPID returns the pid of the bus daemon serving addr: the
// dbus-daemon or dbus-broker process holding the listening socket the address
// points at. It returns (0, nil) when none is found.
//
// Matching any "dbus-daemon --session" is not enough: private buses such as
// the ones dbus-run-session leaves behind are also session daemons, and one
// started from an X11 environment would make every login restart (or, for a
// dbus-launch bus, kill) a bus it has nothing to do with.
func findSessionDbusDaemonPID(addr string) (int, error) {
	path := sessionBusSocketPath(addr)
	if path == "" {
		return 0, nil
	}

	sockets, err := listeningUnixSockets(path)
	if err != nil || len(sockets) == 0 {
		return 0, err
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	broker := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if name != "dbus-daemon" && !strings.HasPrefix(name, "dbus-broker") {
			continue
		}
		if !holdsSocket(pid, sockets) {
			continue
		}
		// dbus-broker-launch (comm is truncated to "dbus-broker-lau") is the
		// one spawning activated services, prefer it over its dbus-broker child.
		if name != "dbus-broker" {
			return pid, nil
		}
		broker = pid
	}
	return broker, nil
}

// sessionBusSocketPath returns the socket a D-Bus address connects to, in
// /proc/net/unix notation: abstract names are prefixed with '@'.
func sessionBusSocketPath(addr string) string {
	for _, a := range strings.Split(addr, ";") {
		if !strings.HasPrefix(a, "unix:") {
			continue
		}
		for _, kv := range strings.Split(strings.TrimPrefix(a, "unix:"), ",") {
			key, value, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			if v, err := url.PathUnescape(value); err == nil {
				value = v
			}
			switch key {
			case "path":
				return value
			case "abstract":
				return "@" + value
			}
		}
	}
	return ""
}

// listeningUnixSockets returns the "socket:[inode]" links of the listening
// unix sockets bound to path.
func listeningUnixSockets(path string) (map[string]bool, error) {
	data, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		return nil, err
	}
	sockets := make(map[string]bool)
	// Num RefCount Protocol Flags Type St Inode Path
	for _, line := range strings.Split(string(data), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[7] != path {
			continue
		}
		// __SO_ACCEPTCON is only set on listening sockets.
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&0x10000 == 0 {
			continue
		}
		sockets["socket:["+fields[6]+"]"] = true
	}
	return sockets, nil
}

func holdsSocket(pid int, sockets map[string]bool) bool {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, fd := range fds {
		link, err := os.Readlink(filepath.Join(dir, fd.Name()))
		if err == nil && sockets[link] {
			return true
		}
	}
	return false
}

// isX11EnvDaemon reports whether the process with the given pid was launched
// inside an X11 session by inspecting its /proc/<pid>/environ.
func isX11EnvDaemon(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		logger.Warning("failed to read dbus-daemon environ:", err)
		return false
	}
	env := string(data)
	return strings.Contains(env, "XDG_SESSION_TYPE=x11") ||
		strings.Contains(env, "QT_QPA_PLATFORM=xcb")
}

// runCmdWithTimeout runs a command and kills it if it does not finish within
// timeout. Failures are logged but never fatal: a misbehaving helper must not
// block the whole session startup.
func runCmdWithTimeout(timeout time.Duration, name string, args ...string) {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		logger.Warningf("%s failed to start: %v", name, err)
		return
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			logger.Warningf("%s %v: %v", name, args, err)
		}
	case <-time.After(timeout):
		logger.Warningf("%s %v timed out, killing it", name, args)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

// restartDbusDaemon kills the stale dbus-daemon (oldPid) and starts a fresh one
// via dbus-launch, publishing the new address through DBUS_SESSION_BUS_ADDRESS
// so that the following dbus.SessionBus() call connects to the new daemon.
//
// A timeout guards the dbus-launch invocation so a misbehaving dbus-launch can
// never block the whole session startup.
func restartDbusDaemon(oldPid int) {
	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		// Force dbus-launch to spawn a brand new daemon instead of reusing
		// the stale address from the environment (which would defeat the
		// restart). The library's SessionBus() will pick up the new address
		// we set below.
		oldAddr, hadAddr := os.LookupEnv("DBUS_SESSION_BUS_ADDRESS")
		oldPidEnv, hadPidEnv := os.LookupEnv("DBUS_SESSION_BUS_PID")
		os.Unsetenv("DBUS_SESSION_BUS_ADDRESS")
		os.Unsetenv("DBUS_SESSION_BUS_PID")
		cmd := exec.Command("dbus-launch", "--sh-syntax")
		out, err := cmd.Output()
		if hadAddr {
			os.Setenv("DBUS_SESSION_BUS_ADDRESS", oldAddr)
		}
		if hadPidEnv {
			os.Setenv("DBUS_SESSION_BUS_PID", oldPidEnv)
		}
		ch <- result{out, err}
	}()

	var res result
	select {
	case res = <-ch:
	case <-time.After(10 * time.Second):
		logger.Warning("dbus-launch timed out, skipping dbus-daemon restart")
		return
	}

	if res.err != nil {
		logger.Warning("dbus-launch failed:", res.err)
		return
	}

	var address, newPid string
	for _, line := range strings.Split(string(res.out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "DBUS_SESSION_BUS_ADDRESS="):
			address = strings.TrimPrefix(line, "DBUS_SESSION_BUS_ADDRESS=")
		case strings.HasPrefix(line, "DBUS_SESSION_BUS_PID="):
			newPid = strings.TrimPrefix(line, "DBUS_SESSION_BUS_PID=")
		}
	}

	if address == "" {
		logger.Warning("dbus-launch did not return DBUS_SESSION_BUS_ADDRESS")
		return
	}

	os.Setenv("DBUS_SESSION_BUS_ADDRESS", address)
	if newPid != "" {
		os.Setenv("DBUS_SESSION_BUS_PID", newPid)
	}

	// Terminate the stale X11-era dbus-daemon. All its clients should already
	// be gone after logout, so this is safe.
	if oldPid > 0 {
		if err := syscall.Kill(oldPid, syscall.SIGTERM); err != nil {
			logger.Warningf("failed to stop stale dbus-daemon %d: %v", oldPid, err)
			// Fallback to SIGKILL if it ignored SIGTERM.
			_ = syscall.Kill(oldPid, syscall.SIGKILL)
		}
	}

	logger.Infof("restarted dbus-daemon (old pid %d, new address %s)", oldPid, address)
}

// restartSystemdUserBus restarts the systemd-managed user session bus
// (dbus.service / dbus-broker.service) so that the new bus daemon process
// inherits a clean Wayland environment instead of the X11 stale
// QT_QPA_PLATFORM=xcb that the previous daemon process carried. This is the
// only way to fix dbus-activated programs (e.g. dde-osd, launched via an
// Exec= service) because they inherit the bus daemon's own process
// environment, not the dbus activation state nor the systemd environment.
//
// It is safe to call early: at this point no shared session-bus connection
// has been established yet (dbus.SessionBus caches a process-wide singleton),
// so the following dbus.SessionBus() will reconnect to the restarted bus.
func restartSystemdUserBus() {
	const unit = "dbus.service"
	// Prefer dbus-broker if that is what the session uses.
	if _, err := exec.LookPath("dbus-broker"); err == nil {
		runCmdWithTimeout(15*time.Second, "systemctl", "--user", "restart", "dbus-broker.service")
	} else {
		runCmdWithTimeout(15*time.Second, "systemctl", "--user", "restart", unit)
	}

	// Wait until the bus socket is available again so the subsequent
	// dbus.SessionBus() connects to the fresh daemon.
	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	socketPath := ""
	if strings.HasPrefix(addr, "unix:path=") {
		socketPath = strings.TrimPrefix(addr, "unix:path=")
	}
	if socketPath == "" {
		return
	}
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(socketPath); err == nil {
			logger.Info("session bus restarted and socket is ready")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	logger.Warning("session bus socket did not reappear after restart: ", socketPath)
}
