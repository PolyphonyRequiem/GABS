//go:build windows

package process

import (
	"os/exec"
	"strconv"
	"strings"
)

// geteuidOrUnknown returns -1 on Windows: there is no POSIX UID, so UID scoping
// is disabled and discovery relies on the tracked process group / session plus
// name matching. The service still runs under a distinct Windows account, but we
// do not resolve owners here.
func geteuidOrUnknown() int {
	return -1
}

// configureProcessGroup creates a new process group for the child so signals can
// be scoped. CREATE_NEW_PROCESS_GROUP == 0x00000200.
func configureProcessGroup(cmd *exec.Cmd) {
	// syscall.SysProcAttr on Windows exposes CreationFlags. Setting the new
	// process group flag keeps parity with the Unix pgroup behavior for scoping.
	// Import kept local to avoid pulling syscall into the shared file set.
	setWindowsProcessGroup(cmd)
}

// trackedGroupID returns the child PID; on Windows we treat the new process
// group as identified by the child PID (the group leader).
func trackedGroupID(pid int) int {
	return pid
}

// osProcessFinder is the production processFinder backed by `tasklist`. Windows
// cannot cheaply attribute a POSIX UID/PGID per process here, so those fields
// are reported as -1 (unknown), which disables UID/PGID filtering and preserves
// the prior name-based behavior while still routing through the scoped seam.
type osProcessFinder struct{}

func (osProcessFinder) FindByName(name string) ([]processInfo, error) {
	cmd := exec.Command("tasklist", "/FI", "IMAGENAME eq "+name, "/FO", "CSV", "/NH")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var procs []processInfo
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.Contains(line, name) {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		pidStr := strings.Trim(parts[1], "\"")
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		procs = append(procs, processInfo{PID: pid, UID: -1, PGID: -1})
	}
	return procs, nil
}
