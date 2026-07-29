package process

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestFindByName_ExcludesZombies is a REAL test: it forks an actual child,
// lets it exit without reaping it, and asserts the resulting <defunct> process
// is not reported as live.
//
// Why this matters: `ps -A` lists zombies, and a zombie keeps its original
// comm. Counting them made IsRunning() return true forever, which made
// games.start a silent no-op — GABS believed the game was already up and
// forked nothing. Observed on Valheim: 9 <defunct> valheim.x86_64 parented to
// gabs, 0 live, every launch refused for hours.
//
// This test FAILS against the pre-fix finder (which parsed pid,ruid,pgid,comm
// with no state column) — verified by reverting the ps args and re-running.
func TestFindByName_ExcludesZombies(t *testing.T) {
	// Start a child that exits immediately. We deliberately do NOT call Wait(),
	// so the kernel keeps it as a zombie parented to this test process.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start child: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() }) // reap at test end, not before

	// Wait for the child to actually reach zombie state.
	if !waitForState(t, pid, "Z", 3*time.Second) {
		t.Skipf("child %d never reached Z state on this platform; skipping", pid)
	}

	comm := commOf(t, pid)
	if comm == "" {
		t.Skipf("could not read comm for zombie %d; skipping", pid)
	}

	procs, err := osProcessFinder{}.FindByName(comm)
	if err != nil {
		t.Fatalf("FindByName(%q) errored: %v", comm, err)
	}

	for _, p := range procs {
		if p.PID == pid {
			t.Fatalf("FindByName(%q) returned zombie PID %d as live — a <defunct> "+
				"process holds a PID but can never run again. Counting it makes "+
				"IsRunning() true forever and turns games.start into a silent no-op.",
				comm, pid)
		}
	}
}

// TestFindByName_FindsLiveProcess is the discriminating counterpart: the same
// finder must still report a genuinely running process. Without this, a finder
// that returned nothing at all would pass the zombie test.
func TestFindByName_FindsLiveProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start child: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	if !waitForState(t, pid, "S", 3*time.Second) {
		t.Skipf("child %d never reached a sleeping state; skipping", pid)
	}

	procs, err := osProcessFinder{}.FindByName("sleep")
	if err != nil {
		t.Fatalf("FindByName(\"sleep\") errored: %v", err)
	}

	for _, p := range procs {
		if p.PID == pid {
			return // found it — correct
		}
	}
	t.Fatalf("FindByName(\"sleep\") did not return live PID %d; the zombie filter "+
		"must not suppress genuinely running processes", pid)
}

// waitForState polls until the process at pid reports a state starting with
// want, or the timeout elapses.
func waitForState(t *testing.T, pid int, want string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), want) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// commOf returns the kernel comm for pid, or "" if it cannot be read.
func commOf(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
