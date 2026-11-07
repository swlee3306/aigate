package vm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// stepCloud advances the clock by a per-method cost and can fail one method.
type stepCloud struct {
	clock  *time.Time
	cost   map[string]time.Duration
	failAt string
	pre    time.Duration // cost of PrunePreRestore (when withPrune)
	post   time.Duration
}

func (c *stepCloud) step(n string) error {
	*c.clock = c.clock.Add(c.cost[n])
	if c.failAt == n {
		return errors.New(n + " failed")
	}
	return nil
}
func (c *stepCloud) DeleteServer(context.Context, string) error { return c.step("DeleteServer") }
func (c *stepCloud) CreateVolumeFromSnapshot(context.Context, string, string) error {
	return c.step("CreateVolumeFromSnapshot")
}
func (c *stepCloud) CreateServer(context.Context, string, string) error {
	return c.step("CreateServer")
}
func (c *stepCloud) WaitSSH(context.Context, string) error { return c.step("WaitSSH") }
func (c *stepCloud) PrunePreRestore(context.Context) (PruneResult, error) {
	*c.clock = c.clock.Add(c.pre)
	return PruneResult{}, nil
}
func (c *stepCloud) Prune(context.Context) (PruneResult, error) {
	*c.clock = c.clock.Add(c.post)
	return PruneResult{}, nil
}

func newStepCloud(clock *time.Time) *stepCloud {
	return &stepCloud{clock: clock, pre: 4 * time.Second, post: 2 * time.Second, cost: map[string]time.Duration{
		"DeleteServer": 10 * time.Second, "CreateVolumeFromSnapshot": 6 * time.Second,
		"CreateServer": 11 * time.Second, "WaitSSH": 21 * time.Second}}
}

func phaseString(ps []Phase) string {
	var s []string
	for _, p := range ps {
		e := ""
		if p.Err != "" {
			e = "!" + p.Err
		}
		s = append(s, fmt.Sprintf("%s=%s%s", p.Name, p.Dur, e))
	}
	return strings.Join(s, " ")
}

func TestRestorePhasesSuccessWithPrune(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	v := newTestVM(newStepCloud(&now), &now)
	v.PruneRestoreVolumes = true
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := v.LastRestore()
	// pre_prune runs after delete_server, so the recorded order is execution order.
	// Having pruned before the restore, there is no post-restore prune any more.
	if got := phaseString(rec.Phases); got != "delete_server=10s pre_prune=4s create_volume=6s create_server=11s wait_ssh=21s" {
		t.Fatalf("phases: %s", got)
	}
}

func TestRestorePhasesWithoutPruneOmitPrunePhases(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	v := newTestVM(newStepCloud(&now), &now)
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := v.LastRestore()
	if got := phaseString(rec.Phases); got != "delete_server=10s create_volume=6s create_server=11s wait_ssh=21s" {
		t.Fatalf("phases: %s", got)
	}
}

func TestRestorePhasesFailedCreateVolume(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	c := newStepCloud(&now)
	c.failAt = "CreateVolumeFromSnapshot"
	v := newTestVM(c, &now)
	if err := v.Restore(context.Background()); err == nil {
		t.Fatal("want error")
	}
	rec, _ := v.LastRestore()
	if got := phaseString(rec.Phases); got != "delete_server=10s create_volume=6s!create volume: CreateVolumeFromSnapshot failed" {
		t.Fatalf("phases: %s", got)
	}
	if rec.Err == "" {
		t.Fatal("record Err lost")
	}
}

func TestLastRestoreReturnsACopy(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	v := newTestVM(newStepCloud(&now), &now)
	if err := v.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ := v.LastRestore()
	rec.Phases[0].Name = "mutated"
	rec.Phases = append(rec.Phases, Phase{Name: "x"})
	again, _ := v.LastRestore()
	if len(again.Phases) != 4 || again.Phases[0].Name != "delete_server" {
		t.Fatalf("history changed: %s", phaseString(again.Phases))
	}
}
