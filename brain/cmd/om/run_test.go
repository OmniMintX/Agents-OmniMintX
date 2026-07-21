package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/config"
)

// fakeProjectAPI stubs the two aoclient calls ensureAutonomy makes.
type fakeProjectAPI struct {
	stored   aoclient.ProjectConfig
	getErr   error
	putErr   error
	putCalls int
	putCfg   aoclient.ProjectConfig
}

func (f *fakeProjectAPI) GetProjectConfig(ctx context.Context, projectID string) (aoclient.ProjectConfig, error) {
	return f.stored, f.getErr
}

func (f *fakeProjectAPI) UpdateProjectConfig(ctx context.Context, projectID string, cfg aoclient.ProjectConfig) (aoclient.Project, error) {
	f.putCalls++
	f.putCfg = cfg
	return aoclient.Project{}, f.putErr
}

func TestEnsureAutonomySetsWhenDifferent(t *testing.T) {
	var stored aoclient.ProjectConfig
	raw := `{"defaultBranch":"develop","agentConfig":{"model":"claude-opus-4-5","permissions":"default"}}`
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatal(err)
	}
	ao := &fakeProjectAPI{stored: stored}
	var log bytes.Buffer
	if err := ensureAutonomy(context.Background(), ao, "p1", aoclient.PermissionAcceptEdits, &log); err != nil {
		t.Fatalf("ensureAutonomy: %v", err)
	}
	if ao.putCalls != 1 {
		t.Fatalf("putCalls = %d, want 1", ao.putCalls)
	}
	if ao.putCfg.AgentConfig.Permissions != aoclient.PermissionAcceptEdits {
		t.Errorf("PUT permissions = %q", ao.putCfg.AgentConfig.Permissions)
	}
	enc, err := json.Marshal(ao.putCfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"defaultBranch":"develop"`, `"model":"claude-opus-4-5"`} {
		if !strings.Contains(string(enc), want) {
			t.Errorf("PUT body %s must preserve %s (PUT replaces config wholesale)", enc, want)
		}
	}
	if !strings.Contains(log.String(), "permissions default → accept-edits") {
		t.Errorf("log = %q", log.String())
	}
}

func TestEnsureAutonomySkipsWhenSame(t *testing.T) {
	ao := &fakeProjectAPI{stored: aoclient.ProjectConfig{
		AgentConfig: aoclient.AgentConfig{Permissions: aoclient.PermissionAuto},
	}}
	var log bytes.Buffer
	if err := ensureAutonomy(context.Background(), ao, "p1", aoclient.PermissionAuto, &log); err != nil {
		t.Fatalf("ensureAutonomy: %v", err)
	}
	if ao.putCalls != 0 {
		t.Errorf("putCalls = %d, want 0 (idempotent)", ao.putCalls)
	}
	if !strings.Contains(log.String(), "đã ở auto") {
		t.Errorf("log = %q", log.String())
	}
}

func TestEnsureAutonomyFailsOnError(t *testing.T) {
	ao := &fakeProjectAPI{getErr: errors.New("boom")}
	if err := ensureAutonomy(context.Background(), ao, "p1", aoclient.PermissionAuto, &bytes.Buffer{}); err == nil {
		t.Error("GET error must fail the run")
	}
	ao = &fakeProjectAPI{putErr: errors.New("boom")}
	if err := ensureAutonomy(context.Background(), ao, "p1", aoclient.PermissionAuto, &bytes.Buffer{}); err == nil {
		t.Error("PUT error must fail the run")
	}
}

func TestResolveAutonomy(t *testing.T) {
	base := config.Config{Autonomy: config.AutonomyAuto}

	if mode, err := resolveAutonomy(base, ""); err != nil || mode != config.AutonomyAuto {
		t.Errorf("config default: mode %q err %v", mode, err)
	}
	if mode, err := resolveAutonomy(base, "off"); err != nil || mode != config.AutonomyOff {
		t.Errorf("flag override: mode %q err %v", mode, err)
	}
	if _, err := resolveAutonomy(base, "yolo"); err == nil {
		t.Error("unknown flag value must error")
	}

	if _, err := resolveAutonomy(base, "bypass-permissions"); err == nil ||
		!strings.Contains(err.Error(), "autonomy_allow_bypass") {
		t.Errorf("bypass without opt-in must be refused, got %v", err)
	}
	blocked := config.Config{Autonomy: config.AutonomyBypass}
	if _, err := resolveAutonomy(blocked, ""); err == nil {
		t.Error("bypass from config without opt-in must be refused")
	}
	allowed := config.Config{Autonomy: config.AutonomyBypass, AutonomyAllowBypass: true}
	if mode, err := resolveAutonomy(allowed, ""); err != nil || mode != config.AutonomyBypass {
		t.Errorf("bypass with opt-in: mode %q err %v", mode, err)
	}
}
