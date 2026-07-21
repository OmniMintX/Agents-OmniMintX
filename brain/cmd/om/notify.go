package main

import (
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"github.com/OmniMintX/overmind/internal/config"
	"github.com/OmniMintX/overmind/internal/scheduler"
)

// resolveNotify returns the effective notify mode for this run: the
// --notify flag when set, else the config knob (file or OVERMIND_NOTIFY).
func resolveNotify(cfg config.Config, flag string) (string, error) {
	mode, err := config.NormalizeNotify(cfg.Notify)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(flag) != "" {
		if mode, err = config.NormalizeNotify(flag); err != nil {
			return "", err
		}
	}
	return mode, nil
}

// notifierFor builds the scheduler.Notifier for a notify mode. NotifyOff
// returns nil (the scheduler treats a nil Notifier as silent).
func notifierFor(mode string, logw io.Writer) scheduler.Notifier {
	if mode == config.NotifyOff {
		return nil
	}
	return &userNotifier{mode: mode, logw: logw, goos: runtime.GOOS, runScript: runOsascript}
}

// userNotifier delivers best-effort user notifications: mode auto tries a
// macOS desktop notification (osascript) and falls back to the terminal
// bell; mode bell (or auto on non-darwin) rings the bell. Errors are
// warnings on logw — a failed notification must never fail the run.
type userNotifier struct {
	mode      string
	logw      io.Writer
	goos      string                    // runtime.GOOS, injectable for tests
	runScript func(script string) error // osascript runner, injectable for tests
}

func (n *userNotifier) Notify(title, body string) {
	if n.mode == config.NotifyAuto && n.goos == "darwin" {
		if err := n.runScript(notificationScript(title, body)); err == nil {
			return
		} else if n.logw != nil {
			fmt.Fprintf(n.logw, "Warning: desktop notification failed (%v) — falling back to terminal bell\n", err)
		}
	}
	if n.logw != nil {
		fmt.Fprintf(n.logw, "\a%s: %s\n", title, body)
	}
}

// notificationScript builds the AppleScript for a desktop notification,
// escaping backslashes and double quotes so title/body cannot break out of
// the string literals.
func notificationScript(title, body string) string {
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return fmt.Sprintf(`display notification "%s" with title "%s"`, esc.Replace(body), esc.Replace(title))
}

// runOsascript executes one AppleScript line via /usr/bin/osascript.
func runOsascript(script string) error {
	return exec.Command("osascript", "-e", script).Run()
}
