package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/OmniMintX/overmind/internal/config"
)

func TestResolveNotify(t *testing.T) {
	base := config.Config{Notify: config.NotifyAuto}
	if mode, err := resolveNotify(base, ""); err != nil || mode != config.NotifyAuto {
		t.Errorf("config default: mode %q err %v", mode, err)
	}
	if mode, err := resolveNotify(base, "off"); err != nil || mode != config.NotifyOff {
		t.Errorf("flag override: mode %q err %v", mode, err)
	}
	if _, err := resolveNotify(base, "loud"); err == nil {
		t.Error("unknown flag value must error")
	}
	if mode, err := resolveNotify(config.Config{Notify: config.NotifyBell}, ""); err != nil || mode != config.NotifyBell {
		t.Errorf("config bell: mode %q err %v", mode, err)
	}
}

func TestNotifierForOffIsNil(t *testing.T) {
	if n := notifierFor(config.NotifyOff, &bytes.Buffer{}); n != nil {
		t.Fatal("notify=off must yield a nil Notifier (silent scheduler)")
	}
}

func TestUserNotifierAutoDarwinOsascript(t *testing.T) {
	var got string
	var log bytes.Buffer
	n := &userNotifier{mode: config.NotifyAuto, logw: &log, goos: "darwin",
		runScript: func(s string) error { got = s; return nil }}
	n.Notify(`ti"tle`, `bo\dy`)
	if !strings.Contains(got, `display notification "bo\\dy" with title "ti\"tle"`) {
		t.Fatalf("script must escape quotes/backslashes, got %q", got)
	}
	if log.Len() != 0 {
		t.Fatalf("successful osascript must not ring the bell: %q", log.String())
	}
}

func TestUserNotifierAutoFallsBackToBell(t *testing.T) {
	var log bytes.Buffer
	n := &userNotifier{mode: config.NotifyAuto, logw: &log, goos: "darwin",
		runScript: func(string) error { return errors.New("no GUI") }}
	n.Notify("title", "body")
	out := log.String()
	if !strings.Contains(out, "Warning:") || !strings.Contains(out, "\a") {
		t.Fatalf("failed osascript must warn and fall back to the bell, got %q", out)
	}
}

func TestUserNotifierAutoNonDarwinIsBell(t *testing.T) {
	var log bytes.Buffer
	n := &userNotifier{mode: config.NotifyAuto, logw: &log, goos: "linux",
		runScript: func(string) error { t.Fatal("osascript must not run off darwin"); return nil }}
	n.Notify("title", "body")
	if !strings.Contains(log.String(), "\atitle: body") {
		t.Fatalf("want bell on non-darwin auto, got %q", log.String())
	}
}

func TestUserNotifierBellSkipsOsascript(t *testing.T) {
	var log bytes.Buffer
	n := &userNotifier{mode: config.NotifyBell, logw: &log, goos: "darwin",
		runScript: func(string) error { t.Fatal("bell mode must not run osascript"); return nil }}
	n.Notify("title", "body")
	if !strings.Contains(log.String(), "\atitle: body") {
		t.Fatalf("want bell output, got %q", log.String())
	}
}
