package main

import (
	"os"
	"os/exec"

	"pkg.deepin.io/dde/startdde/keyring"
)

// checkLoginKeyringArg makes startdde only run the login keyring check and
// exit. It is passed by checkLoginKeyring when re-executing startdde and is
// never meant to be used by hand.
const checkLoginKeyringArg = "--check-login-keyring"

// checkLoginKeyring makes sure the login keyring exists and is the default.
//
// The check runs in a short-lived child process rather than in startdde
// itself: libgnome-keyring opens a private session bus connection, adds a
// match for every org.freedesktop.DBus.NameOwnerChanged signal and hooks the
// connection into the default GMainContext. startdde never runs that context,
// so once the synchronous calls return nobody reads the socket again and the
// signals pile up in it (and in libdbus) for the whole session. The library
// offers no way to close that connection; a child process takes it away when
// it exits.
func checkLoginKeyring() error {
	// /proc/self/exe still works when the binary was replaced by an upgrade.
	cmd := exec.Command("/proc/self/exe")
	cmd.Args = []string{os.Args[0], checkLoginKeyringArg}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runLoginKeyringCheckIfRequested runs the check and exits when startdde was
// re-executed by checkLoginKeyring. It must be called before main does
// anything else.
func runLoginKeyringCheckIfRequested() {
	if len(os.Args) != 2 || os.Args[1] != checkLoginKeyringArg {
		return
	}
	err := keyring.CheckLogin()
	if err != nil {
		logger.Warning(err)
		os.Exit(1)
	}
	os.Exit(0)
}
