//go:build !windows

package process

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// geteuidOrUnknown returns the effective UID of this process, or -1 if it can
// not be determined. On Unix this always succeeds.
func geteuidOrUnknown() int {
	return os.Geteuid()
}

// configureProcessGroup makes the launched child the leader of a new process
// group so the controller can scope discovery to the tracked group. Preserves
// any pre-existing SysProcAttr the caller may have set.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// trackedGroupID resolves the process-group id for the child PID. Because the
// child is started with Setpgid and no explicit Pgid, it leads its own group,
// so the group id equals the child PID. We still query the kernel so the value
// stays correct if that assumption ever changes.
func trackedGroupID(pid int) int {
	if pgid, err := syscall.Getpgid(pid); err == nil {
		return pgid
	}
	return pid
}

// osProcessFinder is the production processFinder backed by `ps`, which lets us
// read PID, UID (ruid), and PGID in a single call so discovery can be scoped to
// the owning service user and the tracked process group. Matching the executable
// name uses the `comm` field (argv[0] basename, truncated by the kernel to 15
// chars) to mirror the previous `pgrep -x` exact-name semantics as closely as
// the tool allows.
type osProcessFinder struct{}

func (osProcessFinder) FindByName(name string) ([]processInfo, error) {
	// -A: all processes; -o: pid,ruid,pgid,state,comm with no header.
	//
	// `state` is REQUIRED, not cosmetic: `ps -A` lists zombies (<defunct>), and a
	// zombie still carries its original comm. Without filtering state we count
	// dead processes as live, IsRunning() returns true forever, and games.start
	// becomes a silent no-op because GABS believes the game is already up. That
	// is the same class as the 14-hour-zombie bug documented in controller.go —
	// fixed there for the stale-wrapper-handle path, but the name lookup itself
	// still counted zombies, so it recurred one layer over. Observed on Valheim:
	// 9 <defunct> valheim.x86_64 parented to gabs, 0 live, every launch refused.
	cmd := exec.Command("ps", "-A", "-o", "pid=,ruid=,pgid=,state=,comm=")
	output, err := cmd.Output()
	if err != nil {
		// ps returns non-zero only on real failure; no-match still exits 0 with
		// an empty body, so any error here is genuine.
		return nil, err
	}

	// The kernel truncates comm to 15 characters (TASK_COMM_LEN-1). Compare
	// against the same truncation so long executable names still match.
	target := name
	if len(target) > 15 {
		target = target[:15]
	}

	var procs []processInfo
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		// State is a single letter, optionally followed by modifiers (Ss, Rl, Z+).
		// Z means the process has exited and is awaiting reaping by its parent —
		// it holds a PID but can never run again, so it is not "running" by any
		// useful definition. Skip it so liveness reflects reality.
		if strings.HasPrefix(fields[3], "Z") {
			continue
		}
		// comm may contain spaces; everything after the first four fields is it.
		comm := strings.Join(fields[4:], " ")
		if comm != target && comm != name {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		uid, err2 := strconv.Atoi(fields[1])
		pgid, err3 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		procs = append(procs, processInfo{PID: pid, UID: uid, PGID: pgid})
	}
	return procs, nil
}
