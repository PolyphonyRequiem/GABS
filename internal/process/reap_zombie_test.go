package process

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests prove GABS reaps the children it kills. They start REAL children,
// stop them through stopByProcessName (the exact path games.stop uses), and
// assert no <defunct> (Z) process survives for that PID.
//
// Pre-fix (1c23db6) all three fail: killProcess never called Wait() at all, and
// terminateProcess only reaped inside `if grace > 0`, leaving both the grace<=0
// path and the grace-expired force-kill branch leaking.

// testBinary copies a system binary to a temp file named "reap-test-child" so
// the child's /proc comm is that unique name. Renaming cmd.Args[0] is NOT
// enough: FindByName matches on comm, which comes from the executable, so a
// renamed-argv child still shows up as "sleep" and would either miss discovery
// or collide with unrelated sleeps owned by the same user.
func testBinary(t *testing.T, src string) string {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("cannot read %s: %v", src, err)
	}
	dst := filepath.Join(t.TempDir(), "reap-test-child")
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatalf("failed to stage test binary: %v", err)
	}
	return dst
}

// startChild launches a real child process under a recognisable name and
// guarantees it is cleaned up if the test fails partway.
func startChild(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start child %s: %v", name, err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			var ws syscall.WaitStatus
			_, _ = syscall.Wait4(cmd.Process.Pid, &ws, syscall.WNOHANG, nil)
		}
	})
	return cmd
}

// stateOf returns the single-letter process state from /proc/<pid>/stat, or ""
// if the pid is gone entirely.
func stateOf(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	// field 3 is state, but comm (field 2) may contain spaces inside parens
	s := string(b)
	if i := strings.LastIndex(s, ")"); i >= 0 && i+2 < len(s) {
		return string(s[i+2])
	}
	return ""
}

// assertNoZombie fails the test if pid is still present in Z (<defunct>) state.
// A fully-gone pid is the pass condition; a Z entry means we killed without
// reaping.
func assertNoZombie(t *testing.T, pid int) {
	t.Helper()
	// Give the kernel a moment to settle the state transition.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := stateOf(pid)
		if st == "" {
			return // reaped and gone — correct
		}
		if !strings.HasPrefix(st, "Z") {
			// still running/sleeping; keep waiting for it to die
			time.Sleep(20 * time.Millisecond)
			continue
		}
		// Z state — but it may be a transient window before our reap lands.
		time.Sleep(20 * time.Millisecond)
	}
	if st := stateOf(pid); strings.HasPrefix(st, "Z") {
		t.Fatalf("pid %d is still a ZOMBIE (state %q) after stop — the kill path "+
			"did not reap it", pid, st)
	}
}

// controllerForChild builds a Controller scoped to find our test children.
func controllerForChild(pid int) *Controller {
	return &Controller{
		spec: LaunchSpec{
			GameId:          "reap-test",
			StopProcessName: "reap-test-child",
		},
	}
}

// TestStopByProcessName_ForceKillReapsChild: force=true → killProcess. Pre-fix
// that path was FindProcess+Kill with no Wait() whatsoever, so every forced stop
// leaked a zombie.
func TestStopByProcessName_ForceKillReapsChild(t *testing.T) {
	cmd := startChild(t, testBinary(t, "/bin/sleep"), "30")
	pid := cmd.Process.Pid
	c := controllerForChild(pid)

	if err := c.stopByProcessName("reap-test-child", true /*force*/, 0); err != nil {
		t.Fatalf("stopByProcessName(force=true) errored: %v", err)
	}
	assertNoZombie(t, pid)
}

// TestStopByProcessName_GraceZeroReapsChild: force=false, grace=0 →
// terminateProcess grace<=0 branch. Pre-fix it SIGTERMs and returns with no
// Wait() at all, leaking a zombie once the child exits on SIGTERM.
func TestStopByProcessName_GraceZeroReapsChild(t *testing.T) {
	cmd := startChild(t, testBinary(t, "/bin/sleep"), "30") // default disposition: exits on SIGTERM
	pid := cmd.Process.Pid
	c := controllerForChild(pid)

	if err := c.stopByProcessName("reap-test-child", false /*force*/, 0 /*grace*/); err != nil {
		t.Fatalf("stopByProcessName(force=false, grace=0) errored: %v", err)
	}
	assertNoZombie(t, pid)
}

// TestStopByProcessName_GraceExpiredForceKillReapsChild: force=false with a
// short grace, against a child that IGNORES SIGTERM so the grace period expires
// and terminateProcess takes the force-kill timeout branch. Pre-fix that branch
// `return process.Kill()` with no follow-up Wait(), leaking a zombie even though
// grace was positive.
func TestStopByProcessName_GraceExpiredForceKillReapsChild(t *testing.T) {
	// trap '' TERM makes the shell ignore SIGTERM; the busy `while` loop keeps
	// the shell resident as a distinct process. A bare `trap ''; sleep 30` does
	// NOT work — sh optimises the tail command into an exec(2), replacing the
	// shell with sleep and discarding the trap entirely, so the child dies on
	// the first SIGTERM in ~60µs and the grace branch is never exercised.
	cmd := startChild(t, testBinary(t, "/bin/sh"), "-c", "trap '' TERM; while true; do sleep 0.2; done")
	pid := cmd.Process.Pid
	// The shell installs its TERM trap slightly after exec(2); if we signal
	// before that, the child dies on the first SIGTERM and the grace branch is
	// never exercised. Gate on the trap being demonstrably in place.
	if !trapInstalled(t, pid, 3*time.Second) {
		t.Fatalf("child %d never held its SIGTERM trap; cannot exercise grace-expiry branch", pid)
	}
	c := controllerForChild(pid)

	start := time.Now()
	if err := c.stopByProcessName("reap-test-child", false /*force*/, 300*time.Millisecond); err != nil {
		t.Fatalf("stopByProcessName(grace expiring) errored: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("grace path returned in %v — expected it to wait out the ~300ms "+
			"grace before force-killing; test is not exercising the timeout branch", elapsed)
	}
	assertNoZombie(t, pid)
}

// trapInstalled confirms the process at pid is ignoring SIGTERM by probing it
// with SIGTERM and checking it survives. A survivor proves the grace-expiry
// branch will actually be exercised. The probe is harmless: the real stop path
// re-sends SIGTERM and then force-kills.
//
// Timing matters. The shell installs its TERM trap a little after exec(2), and a
// probe that lands first kills the child on the default disposition — which
// looks identical to "the trap never worked". Verified against a live child: an
// 800ms head start is reliably enough, an immediate probe is not. So we wait for
// the shell to settle before probing at all, and treat only a post-probe death
// as a genuine failure.
func trapInstalled(t *testing.T, pid int, timeout time.Duration) bool {
	t.Helper()

	// Let the shell reach its trap statement before probing.
	settle := 800 * time.Millisecond
	if settle > timeout {
		settle = timeout / 2
	}
	time.Sleep(settle)

	if st := stateOf(pid); st == "" || strings.HasPrefix(st, "Z") {
		return false // child died before we probed it
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Signal(syscall.SIGTERM)
	time.Sleep(300 * time.Millisecond)

	st := stateOf(pid)
	return st != "" && !strings.HasPrefix(st, "Z")
}
