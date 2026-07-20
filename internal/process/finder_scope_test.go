package process

import (
	"testing"
)

// fakeFinder is an injectable processFinder returning a fixed process table.
type fakeFinder struct {
	byName map[string][]processInfo
	err    error
	calls  int
}

func (f *fakeFinder) FindByName(name string) ([]processInfo, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.byName[name], nil
}

func pidSet(pids []int) map[int]bool {
	m := make(map[int]bool, len(pids))
	for _, p := range pids {
		m[p] = true
	}
	return m
}

// Two same-name clients owned by DIFFERENT users: discovery must return only
// the process owned by the GABS service user. This is the core isolation
// invariant — GABS running as valbot must never surface the primary user's
// same-name client.
func TestFindScopedProcesses_DifferentUsers(t *testing.T) {
	const serviceUID = 1001
	c := &Controller{
		serviceUID: serviceUID,
		finder: &fakeFinder{byName: map[string][]processInfo{
			"valheim.x86_64": {
				{PID: 100, UID: 1000, PGID: 100}, // primary user's client
				{PID: 200, UID: 1001, PGID: 200}, // valbot's client (ours)
			},
		}},
	}

	pids, err := c.findScopedProcesses("valheim.x86_64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := pidSet(pids)
	if len(got) != 1 || !got[200] {
		t.Fatalf("expected only PID 200 (service user), got %v", pids)
	}
	if got[100] {
		t.Fatalf("MUST NOT include PID 100 owned by another user; got %v", pids)
	}
}

// Two same-name clients owned by the SAME service user but in different process
// groups: when a tracked group is known and live, discovery narrows to the
// tracked group so an unrelated manual client the service user launched is not
// stopped.
func TestFindScopedProcesses_SameUserDifferentGroups(t *testing.T) {
	const serviceUID = 1001
	c := &Controller{
		serviceUID:  serviceUID,
		trackedPGID: 200,
		finder: &fakeFinder{byName: map[string][]processInfo{
			"valheim.x86_64": {
				{PID: 150, UID: 1001, PGID: 150}, // manual client, not tracked
				{PID: 200, UID: 1001, PGID: 200}, // our tracked client
				{PID: 201, UID: 1001, PGID: 200}, // child of our tracked client
			},
		}},
	}

	pids, err := c.findScopedProcesses("valheim.x86_64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := pidSet(pids)
	if !got[200] || !got[201] {
		t.Fatalf("expected tracked group members 200,201; got %v", pids)
	}
	if got[150] {
		t.Fatalf("MUST NOT include untracked same-user client PID 150; got %v", pids)
	}
}

// Tracked group has no live members (game detached from wrapper's group): must
// fall back to the UID-scoped set rather than returning nothing, preserving the
// direct-wrapper stop behavior. Still must not cross the user boundary.
func TestFindScopedProcesses_TrackedGroupGoneFallsBackToUID(t *testing.T) {
	const serviceUID = 1001
	c := &Controller{
		serviceUID:  serviceUID,
		trackedPGID: 999, // group with no live members
		finder: &fakeFinder{byName: map[string][]processInfo{
			"valheim.x86_64": {
				{PID: 100, UID: 1000, PGID: 100}, // other user
				{PID: 300, UID: 1001, PGID: 300}, // our user, detached group
			},
		}},
	}

	pids, err := c.findScopedProcesses("valheim.x86_64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := pidSet(pids)
	if len(got) != 1 || !got[300] {
		t.Fatalf("expected UID-scoped fallback to PID 300, got %v", pids)
	}
}

// Unknown UID (uid == -1, e.g. Windows) disables the UID filter; name matching
// still applies and, without a tracked group, all matches are returned.
func TestScopeProcesses_UnknownUIDDisablesFilter(t *testing.T) {
	procs := []processInfo{
		{PID: 1, UID: -1, PGID: -1},
		{PID: 2, UID: -1, PGID: -1},
	}
	pids := scopeProcesses(procs, -1, 0)
	if len(pids) != 2 {
		t.Fatalf("expected both PIDs when UID unknown, got %v", pids)
	}
}

// A process with an unknown UID is not filtered out even when the service UID is
// known — we only exclude on a positive mismatch, never on missing data.
func TestScopeProcesses_ProcessUnknownUIDKept(t *testing.T) {
	procs := []processInfo{
		{PID: 1, UID: -1, PGID: -1}, // unknown owner: keep
		{PID: 2, UID: 1000, PGID: 0}, // known other owner: drop
		{PID: 3, UID: 1001, PGID: 0}, // us: keep
	}
	got := pidSet(scopeProcesses(procs, 1001, 0))
	if !got[1] || !got[3] || got[2] {
		t.Fatalf("expected {1,3}, got %v", got)
	}
}

// No tracked group and single service-user match returns that PID.
func TestFindScopedProcesses_NoTrackedGroup(t *testing.T) {
	c := &Controller{
		serviceUID: 1001,
		finder: &fakeFinder{byName: map[string][]processInfo{
			"valheim.x86_64": {{PID: 200, UID: 1001, PGID: 200}},
		}},
	}
	pids, err := c.findScopedProcesses("valheim.x86_64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pids) != 1 || pids[0] != 200 {
		t.Fatalf("expected [200], got %v", pids)
	}
}

// IsRunning for a Steam-launched game routes through the scoped finder: a
// same-name process owned by another user must report NOT running for this lane.
func TestIsRunning_SteamScopedToServiceUser(t *testing.T) {
	c := &Controller{
		serviceUID: 1001,
		finder: &fakeFinder{byName: map[string][]processInfo{
			"valheim.x86_64": {{PID: 100, UID: 1000, PGID: 100}}, // only other user's
		}},
	}
	c.spec = LaunchSpec{GameId: "valheim", Mode: "SteamAppId", PathOrId: "892970", StopProcessName: "valheim.x86_64"}
	if c.IsRunning() {
		t.Fatal("IsRunning must be false: only another user's same-name process exists")
	}
}
