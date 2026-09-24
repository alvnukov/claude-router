package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type deployFixture struct {
	mu         sync.Mutex
	active     string
	slots      map[string]*deploySlotState
	calls      []string
	fail       string
	onState    func(slot string, s *deploySlotState) error // simulates launchd restarts
	controller *deployController
	api        *httptest.Server
}

func newDeployFixture(t *testing.T) *deployFixture {
	t.Helper()
	f := &deployFixture{active: "blue", slots: map[string]*deploySlotState{"blue": {Mode: modeActive, PID: 42, Digest: "old"}, "green": {Mode: modeStandby}}}
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/healthz" {
			f.calls = append(f.calls, "public-ready:"+f.active)
			if f.fail == "public-ready" {
				http.Error(w, "not ready", 503)
				return
			}
			s := f.slots[f.active]
			fmt.Fprintf(w, `{"slot":%q,"pid":%d,"mode":%q}`, f.active, s.PID, s.Mode)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.api.Close)
	// All configured listeners are OS-assigned; never use a live router port.
	port := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}
	cfg := deployConfig{PublicAPI: f.api.Listener.Addr().String(), PublicUI: port(), BlueAPI: port(), BlueUI: port(), GreenAPI: port(), GreenUI: port()}
	f.controller = &deployController{config: cfg, ops: f}
	return f
}

func (f *deployFixture) current(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "current")
	return f.active, nil
}
func (f *deployFixture) state(_ context.Context, slot string) (deploySlotState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "state:"+slot)
	if f.onState != nil {
		if err := f.onState(slot, f.slots[slot]); err != nil {
			return deploySlotState{}, err
		}
	}
	return *f.slots[slot], nil
}
func (f *deployFixture) start(_ context.Context, slot, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start:"+slot)
	if f.fail == "start" {
		return errors.New("start failed")
	}
	f.slots[slot] = &deploySlotState{Mode: modeStandby, PID: 43, Digest: digest}
	return nil
}
func (f *deployFixture) admin(_ context.Context, slot, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, action+":"+slot)
	if f.fail == action && !(action == "activate" && slot == "blue") {
		return errors.New(action + " failed")
	}
	s := f.slots[slot]
	switch action {
	case "snapshot":
		if s.Mode != modeActive {
			return errors.New("snapshot inactive")
		}
	case "quiesce":
		if s.Mode != modeActive && s.Mode != modeQuiesced {
			return errors.New("quiesce inactive")
		}
		s.Mode = modeQuiesced
	case "activate":
		if s.Mode == modeDraining {
			return errors.New("draining cannot activate")
		}
		s.Mode = modeActive
	case "drain":
		s.Mode = modeDraining
	}
	return nil
}
func (f *deployFixture) flip(_ context.Context, slot string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "flip:"+slot)
	if f.fail == "flip" {
		return errors.New("flip failed")
	}
	f.active = slot
	return nil
}
func (f *deployFixture) stop(_ context.Context, slot string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop:"+slot)
	if f.fail == "stop" {
		return errors.New("stop failed")
	}
	f.slots[slot].PID = 0
	return nil
}
func (f *deployFixture) save(_ context.Context, slot string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "save:"+slot)
	if f.fail == "save" {
		return errors.New("save failed")
	}
	return nil
}

func TestDeployOrdersReadinessBeforeDrainAndSavesCaddy(t *testing.T) {
	f := newDeployFixture(t)
	if err := f.controller.deploy(t.Context(), "new", false); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls, ",")
	for _, step := range []string{"start:green", "snapshot:blue", "quiesce:blue", "activate:green", "flip:green", "public-ready:green", "drain:blue", "stop:blue", "save:green"} {
		if !strings.Contains(got, step) {
			t.Fatalf("missing %s: %s", step, got)
		}
	}
	if strings.Index(got, "public-ready:green") > strings.Index(got, "drain:blue") || strings.Index(got, "drain:blue") > strings.Index(got, "stop:blue") {
		t.Fatalf("wrong deploy ordering: %s", got)
	}
	if f.active != "green" || f.slots["blue"].PID != 0 {
		t.Fatalf("incomplete deploy: %+v", f)
	}
}

func TestDeployRollsBackBeforeAndAfterFlipWithoutDraining(t *testing.T) {
	for _, point := range []string{"start", "snapshot", "quiesce", "activate", "flip", "public-ready"} {
		t.Run(point, func(t *testing.T) {
			f := newDeployFixture(t)
			f.fail = point
			if err := f.controller.deploy(t.Context(), "new", false); err == nil {
				t.Fatal("expected failure")
			}
			if f.active != "blue" || f.slots["blue"].Mode != modeActive {
				t.Fatalf("rollback failed: active=%s old=%s", f.active, f.slots["blue"].Mode)
			}
			if strings.Contains(strings.Join(f.calls, ","), "drain:blue") {
				t.Fatal("drained old before verified readiness")
			}
		})
	}
}

func TestDeployResumeAfterDrainingOldSlot(t *testing.T) {
	f := newDeployFixture(t)
	f.active = "green"
	f.slots["green"] = &deploySlotState{Mode: modeActive, PID: 43, Digest: "new"}
	f.slots["blue"].Mode = modeDraining
	if err := f.controller.deploy(t.Context(), "new", false); err != nil {
		t.Fatal(err)
	}
	if f.slots["blue"].PID != 0 || !strings.Contains(strings.Join(f.calls, ","), "stop:blue") {
		t.Fatalf("did not finish interrupted drain: %+v", f.slots)
	}
}

func TestDeployNoopSameDigestAndForcedSwitch(t *testing.T) {
	f := newDeployFixture(t)
	if err := f.controller.deploy(t.Context(), "old", false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f.calls, ","), "start:") {
		t.Fatal("same digest started new slot")
	}
	if err := f.controller.deploy(t.Context(), "old", true); err != nil {
		t.Fatal(err)
	}
	if f.active != "green" {
		t.Fatal("forced switch did not occur")
	}
}

func TestDeployDoesNotStopNewSlotAfterDrainFailure(t *testing.T) {
	f := newDeployFixture(t)
	f.fail = "stop"
	if err := f.controller.deploy(t.Context(), "new", false); err == nil {
		t.Fatal("expected bootout failure")
	}
	if f.active != "green" || f.slots["green"].PID == 0 || f.slots["blue"].Mode != modeDraining {
		t.Fatalf("post-drain failure destroyed active slot: active=%s slots=%+v", f.active, f.slots)
	}
}

func TestDeployTreatsOldSlotRestartedDuringDrainAsDrained(t *testing.T) {
	restarts := map[string]func(*deploySlotState) error{
		// The marker already names green, so launchd brings blue back standby.
		"standby": func(s *deploySlotState) error {
			*s = deploySlotState{Mode: modeStandby, PID: 44, Digest: s.Digest}
			return nil
		},
		"refused": func(*deploySlotState) error {
			return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
		},
	}
	for name, restart := range restarts {
		t.Run(name, func(t *testing.T) {
			f := newDeployFixture(t)
			f.onState = func(slot string, s *deploySlotState) error {
				if slot == "blue" && s.Mode == modeDraining {
					s.Pending = 3
					return restart(s)
				}
				return nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if err := f.controller.deploy(ctx, "new", false); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(f.calls, ","); !strings.Contains(got, "stop:blue") || !strings.HasSuffix(got, "save:green") {
				t.Fatalf("restarted old slot was not retired: %s", got)
			}
		})
	}
}

func TestDeployAbortsWhenOldSlotRestartsBeforeFlip(t *testing.T) {
	f := newDeployFixture(t)
	f.onState = func(slot string, s *deploySlotState) error {
		if slot == "blue" && s.Mode == modeQuiesced && f.slots["green"].Mode == modeActive {
			// The marker still names blue, so the restart comes back active.
			*s = deploySlotState{Mode: modeActive, PID: 44, Digest: s.Digest}
		}
		return nil
	}
	if err := f.controller.deploy(t.Context(), "new", false); err == nil {
		t.Fatal("switched while the restarted old slot was writing")
	}
	if got := strings.Join(f.calls, ","); strings.Contains(got, "flip:green") || f.active != "blue" || f.slots["green"].PID != 0 || f.slots["blue"].Mode != modeActive {
		t.Fatalf("two writers left behind: active=%s slots=%+v calls=%s", f.active, f.slots, got)
	}
}

func TestDeployRetiresLeftoverCandidateBeforeStart(t *testing.T) {
	for _, mode := range []lifecycleMode{modeStandby, modeDraining, modeActive} {
		t.Run(string(mode), func(t *testing.T) {
			f := newDeployFixture(t)
			f.slots["green"] = &deploySlotState{Mode: mode, PID: 40, Pending: 2, Digest: "stale"}
			polls := 0
			f.onState = func(slot string, s *deploySlotState) error {
				if slot == "green" && s.Mode == modeDraining {
					if polls++; polls > 2 {
						s.Pending = 0
					}
				}
				return nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if err := f.controller.deploy(ctx, "new", false); err != nil {
				t.Fatal(err)
			}
			got := strings.Join(f.calls, ",")
			stop, start := strings.Index(got, "stop:green"), strings.Index(got, "start:green")
			if stop < 0 || start < stop {
				t.Fatalf("leftover green not stopped before start: %s", got)
			}
			if mode != modeStandby && polls < 3 {
				t.Fatalf("leftover green stopped with requests in flight: %s", got)
			}
		})
	}
}

func TestDeployRejectsReservedPortsInTests(t *testing.T) {
	for _, port := range []string{"8787", "8788", "8791", "8792", "8793", "8794"} {
		f := newDeployFixture(t)
		f.controller.config.GreenAPI = "127.0.0.1:" + port
		if err := f.controller.deploy(t.Context(), "new", false); err == nil {
			t.Fatalf("test accepted reserved port %s", port)
		}
		if len(f.calls) != 0 {
			t.Fatalf("touched slots before refusing port %s: %v", port, f.calls)
		}
	}
}
