package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type LaunchSpec struct {
	GameId          string
	Mode            string // DirectPath|SteamAppId|EpicAppId|CustomCommand
	PathOrId        string
	Args            []string
	WorkingDir      string
	StopProcessName string // Optional process name for stopping the game
}

type BridgeInfo struct {
	Port  int
	Token string
}

// processInfo describes a single discovered process. UID and PGID are used to
// scope discovery to the owning service user and the tracked process group so
// GABS never signals an unrelated same-name process. A UID or PGID of -1 means
// "unknown" (e.g. the platform could not resolve it) and disables that filter.
type processInfo struct {
	PID  int
	UID  int
	PGID int
}

// processFinder discovers processes by executable name. It is the single seam
// through which the controller learns about the system, so tests can inject a
// deterministic set of processes (e.g. two same-name clients owned by different
// users or in different process groups) without spawning anything real.
type processFinder interface {
	FindByName(name string) ([]processInfo, error)
}

// Controller implements a stateless approach to process management
// It queries the actual system state rather than maintaining internal state
type Controller struct {
	spec       LaunchSpec
	cmd        *exec.Cmd
	bridgeInfo *BridgeInfo

	// finder is the process-discovery seam. When nil the OS-backed finder is
	// used; tests override it to supply a deterministic process table.
	finder processFinder

	// serviceUID is the effective UID of this GABS process. Discovery is scoped
	// to processes owned by this user so a GABS running as e.g. `valbot` can
	// never signal a process owned by the primary user. Resolved lazily.
	serviceUID int

	// trackedPGID is the process group of the child GABS launched (Setpgid on
	// Unix). When >0 and the group still has live members, discovery is further
	// restricted to that group, which distinguishes this lane's game from an
	// unrelated same-name client owned by the SAME service user. When the group
	// has vanished (the common direct-wrapper case where the real game detaches
	// from the wrapper's group) discovery falls back to UID scoping so tracking
	// loss never widens the blast radius past the service user.
	trackedPGID int
}

// getFinder returns the configured finder or the default OS-backed one.
func (c *Controller) getFinder() processFinder {
	if c.finder != nil {
		return c.finder
	}
	return osProcessFinder{}
}

// ownUID returns the effective UID of this GABS process (cached). Returns -1 on
// platforms where it cannot be resolved, which disables UID scoping.
func (c *Controller) ownUID() int {
	if c.serviceUID == 0 {
		c.serviceUID = geteuidOrUnknown()
	}
	return c.serviceUID
}

// findScopedProcesses discovers processes named `name` and returns only those
// PIDs that belong to this lane: owned by the GABS service user and, when a
// live tracked process group is known, in that group. This is the single choke
// point every stop/status path routes through so no code path can fall back to
// a host-global match.
func (c *Controller) findScopedProcesses(name string) ([]int, error) {
	procs, err := c.getFinder().FindByName(name)
	if err != nil {
		return nil, err
	}
	return scopeProcesses(procs, c.ownUID(), c.trackedPGID), nil
}

// scopeProcesses applies the ownership + process-group scoping policy.
//
//   - UID filter: keep only processes whose UID matches uid. Skipped when uid or
//     a process's UID is -1 (unknown), so platforms that cannot resolve owners
//     degrade to name-only rather than crashing.
//   - PGID filter: when pgid > 0 AND at least one UID-scoped process is in that
//     group, restrict to that group. If the tracked group has no live members
//     (tracking lost / game detached), fall back to the UID-scoped set instead
//     of returning nothing, preserving direct-wrapper stop behavior.
func scopeProcesses(procs []processInfo, uid, pgid int) []int {
	var uidScoped []processInfo
	for _, p := range procs {
		if uid >= 0 && p.UID >= 0 && p.UID != uid {
			continue
		}
		uidScoped = append(uidScoped, p)
	}

	if pgid > 0 {
		var inGroup []int
		for _, p := range uidScoped {
			if p.PGID == pgid {
				inGroup = append(inGroup, p.PID)
			}
		}
		if len(inGroup) > 0 {
			return inGroup
		}
	}

	pids := make([]int, 0, len(uidScoped))
	for _, p := range uidScoped {
		pids = append(pids, p.PID)
	}
	return pids
}

// Configure sets up the controller with the given launch specification
func (c *Controller) Configure(spec LaunchSpec) error {
	if spec.GameId == "" {
		return &ProcessError{
			Type:    ProcessErrorTypeConfiguration,
			Context: "GameId is required",
			Err:     fmt.Errorf("GameId cannot be empty"),
		}
	}

	switch spec.Mode {
	case "DirectPath", "":
		if spec.PathOrId == "" {
			return &ProcessError{
				Type:    ProcessErrorTypeConfiguration,
				Context: fmt.Sprintf("PathOrId is required for mode %s", spec.Mode),
				Err:     fmt.Errorf("PathOrId cannot be empty for DirectPath mode"),
			}
		}
	case "SteamAppId", "EpicAppId", "CustomCommand":
		if spec.PathOrId == "" {
			return &ProcessError{
				Type:    ProcessErrorTypeConfiguration,
				Context: fmt.Sprintf("PathOrId is required for mode %s", spec.Mode),
				Err:     fmt.Errorf("PathOrId cannot be empty for %s mode", spec.Mode),
			}
		}
	default:
		return &ProcessError{
			Type:    ProcessErrorTypeConfiguration,
			Context: fmt.Sprintf("unsupported launch mode: %s", spec.Mode),
			Err:     fmt.Errorf("unsupported launch mode: %s", spec.Mode),
		}
	}

	c.spec = spec
	return nil
}

// SetBridgeInfo sets the bridge connection information 
func (c *Controller) SetBridgeInfo(port int, token string) {
	c.bridgeInfo = &BridgeInfo{
		Port:  port,
		Token: token,
	}
}

// Start launches the process and waits for verification
func (c *Controller) Start() error {
	// Prepare command based on launch mode
	var cmdName string
	var cmdArgs []string

	switch c.spec.Mode {
	case "DirectPath", "":
		cmdName = c.spec.PathOrId
		cmdArgs = c.spec.Args
	case "SteamAppId":
		cmdName = c.getSteamLauncher()
		if runtime.GOOS == "windows" {
			cmdArgs = []string{"/c", "start", fmt.Sprintf("steam://rungameid/%s", c.spec.PathOrId)}
		} else {
			cmdArgs = []string{fmt.Sprintf("steam://rungameid/%s", c.spec.PathOrId)}
		}
	case "EpicAppId":
		cmdName = c.getSystemOpenCommand()
		cmdArgs = []string{fmt.Sprintf("com.epicgames.launcher://apps/%s?action=launch&silent=true", c.spec.PathOrId)}
	case "CustomCommand":
		cmdName = c.spec.PathOrId
		cmdArgs = c.spec.Args
	default:
		return &ProcessError{
			Type:    ProcessErrorTypeStart,
			Context: fmt.Sprintf("unsupported launch mode: %s", c.spec.Mode),
			Err:     fmt.Errorf("unsupported launch mode: %s", c.spec.Mode),
		}
	}

	// Create command
	c.cmd = exec.Command(cmdName, cmdArgs...)
	if c.spec.WorkingDir != "" {
		c.cmd.Dir = c.spec.WorkingDir
	}

	// Put the child in its own process group so status/stop can scope to the
	// tracked group (this lane's game) rather than any same-name process the
	// service user happens to be running. No-op on platforms without pgroups.
	configureProcessGroup(c.cmd)

	// Set up environment variables
	c.setupEnvironment()

	// Start the process
	if err := c.cmd.Start(); err != nil {
		return &ProcessError{
			Type:    ProcessErrorTypeStart,
			Context: fmt.Sprintf("failed to start %s (mode: %s, target: %s)", c.spec.GameId, c.spec.Mode, c.spec.PathOrId),
			Err:     err,
		}
	}

	// Record the tracked process group (equals the child PID when it leads its
	// own group). Discovery narrows to this group while it has live members.
	if c.cmd.Process != nil {
		c.trackedPGID = trackedGroupID(c.cmd.Process.Pid)
	}

	return nil
}

// setupEnvironment configures environment variables for the process
func (c *Controller) setupEnvironment() {
	bridgePath := c.getBridgePath()
	bridgeEnvVars := []string{
		fmt.Sprintf("GABS_GAME_ID=%s", c.spec.GameId),
		fmt.Sprintf("GABS_BRIDGE_PATH=%s", bridgePath),
	}

	if c.bridgeInfo != nil {
		bridgeEnvVars = append(bridgeEnvVars,
			fmt.Sprintf("GABP_SERVER_PORT=%d", c.bridgeInfo.Port),
			fmt.Sprintf("GABP_TOKEN=%s", c.bridgeInfo.Token),
		)
	}

	env := os.Environ()
	if os.Getenv("SystemRoot") == "" {
		env = append(env, "SystemRoot=C:\\Windows", "WINDIR=C:\\Windows")
	}
	c.cmd.Env = append(env, bridgeEnvVars...)
}

// IsRunning queries the actual system state to determine if the process is running
// This is stateless - it directly checks the real process state
func (c *Controller) IsRunning() bool {
	// For Steam/Epic launchers, check for the actual game process by name if configured
	if c.spec.Mode == "SteamAppId" || c.spec.Mode == "EpicAppId" {
		if c.spec.StopProcessName != "" {
			pids, err := c.findScopedProcesses(c.spec.StopProcessName)
			if err != nil {
				return false
			}
			return len(pids) > 0
		}
		// Without StopProcessName, we can't track launcher-based games
		return false
	}

	// For DIRECT processes launched via a wrapper script (DirectPath), the tracked
	// c.cmd is the WRAPPER, which typically exits right after spawning the real game
	// binary — or the game is a grandchild we never reaped. Signalling the wrapper PID
	// then wrongly reports "running" forever (the 14-hour-zombie bug: user closes the
	// game manually, GABS never notices). When a StopProcessName is configured, trust
	// the REAL process by name over the stale wrapper handle. This makes status honest
	// and lets stale sessions get cleaned up instead of seizing the launcher.
	if c.spec.StopProcessName != "" {
		pids, err := c.findScopedProcesses(c.spec.StopProcessName)
		if err == nil {
			return len(pids) > 0
		}
		// fall through to the cmd-based check if the name lookup itself failed
	}

	// For direct processes with no name to key on, check the managed process handle.
	if c.cmd == nil || c.cmd.Process == nil {
		return false
	}

	// Check if the process has already been waited for
	if c.cmd.ProcessState != nil {
		return false
	}

	// Try to signal the process with signal 0 (doesn't affect the process, just checks existence)
	// This is the most reliable cross-platform approach
	err := c.cmd.Process.Signal(syscall.Signal(0))
	if err != nil {
		// Process is dead, try to reap it to update ProcessState
		go func() {
			c.cmd.Wait() // This will set ProcessState for future calls
		}()
		return false
	}
	return true
}

// WaitForProcessStart waits for the process to be detectable in the system
func (c *Controller) WaitForProcessStart(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return &ProcessError{
				Type:    ProcessErrorTypeStart,
				Context: fmt.Sprintf("timed out waiting for %s to start", c.spec.GameId),
				Err:     fmt.Errorf("process not found in system after %v", timeout),
			}
		case <-ticker.C:
			if c.IsRunning() {
				return nil
			}
		}
	}
}

// Stop gracefully stops the process
func (c *Controller) Stop(grace time.Duration) error {
	// Try to stop by process name first if configured
	if c.spec.StopProcessName != "" {
		if err := c.stopByProcessName(c.spec.StopProcessName, false, grace); err == nil {
			return nil
		}
	}

	if c.cmd == nil || c.cmd.Process == nil {
		return &ProcessError{
			Type:    ProcessErrorTypeStop,
			Context: "no process to stop",
			Err:     fmt.Errorf("no process available"),
		}
	}

	// Try graceful termination first
	if err := c.cmd.Process.Signal(getTerminationSignal()); err != nil {
		// If graceful termination fails, try force kill
		killErr := c.cmd.Process.Kill()
		if killErr != nil {
			return &ProcessError{
				Type:    ProcessErrorTypeStop,
				Context: fmt.Sprintf("failed to stop %s", c.spec.GameId),
				Err:     killErr,
			}
		}
		return nil
	}

	// Wait for graceful shutdown with timeout
	done := make(chan error, 1)
	go func() {
		done <- c.cmd.Wait()
	}()

	select {
	case <-done:
		return nil
	case <-time.After(grace):
		// Grace period expired, force kill
		if err := c.cmd.Process.Kill(); err != nil {
			return &ProcessError{
				Type:    ProcessErrorTypeStop,
				Context: fmt.Sprintf("failed to force kill %s after grace period", c.spec.GameId),
				Err:     err,
			}
		}
		return nil
	}
}

// Kill forcefully terminates the process
func (c *Controller) Kill() error {
	if c.spec.StopProcessName != "" {
		if err := c.stopByProcessName(c.spec.StopProcessName, true, 0); err == nil {
			return nil
		}
	}

	if c.cmd == nil || c.cmd.Process == nil {
		return &ProcessError{
			Type:    ProcessErrorTypeStop,
			Context: "no process to kill",
			Err:     fmt.Errorf("no process available"),
		}
	}

	err := c.cmd.Process.Kill()
	if err != nil {
		return &ProcessError{
			Type:    ProcessErrorTypeStop,
			Context: fmt.Sprintf("failed to kill %s", c.spec.GameId),
			Err:     err,
		}
	}
	return nil
}

// Restart stops and then starts the process
func (c *Controller) Restart() error {
	// Stop then Start, preserving spec
	if err := c.Stop(3 * time.Second); err != nil {
		// Log the stop error but continue with restart
		// The failure might be because the process was already dead
		// In that case, starting should still work
		fmt.Fprintf(os.Stderr, "Warning: Stop failed during restart: %v\n", err)
	}
	return c.Start()
}

// GetPID returns the process ID if available
func (c *Controller) GetPID() int {
	if c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// GetLaunchMode returns the launch mode
func (c *Controller) GetLaunchMode() string {
	return c.spec.Mode
}

// GetStopProcessName returns the stop process name
func (c *Controller) GetStopProcessName() string {
	return c.spec.StopProcessName
}

// IsLauncherProcessRunning checks if the launcher process itself is still running
func (c *Controller) IsLauncherProcessRunning() bool {
	if c.cmd == nil || c.cmd.Process == nil {
		return false
	}

	if c.cmd.ProcessState != nil {
		return false
	}

	err := c.cmd.Process.Signal(syscall.Signal(0))
	return err == nil
}

// Helper methods
func (c *Controller) getSteamLauncher() string {
	switch runtime.GOOS {
	case "windows":
		return "cmd"
	case "darwin":
		return "open"
	default:
		return "xdg-open"
	}
}

func (c *Controller) getSystemOpenCommand() string {
	switch runtime.GOOS {
	case "windows":
		return "cmd"
	case "darwin":
		return "open"
	default:
		return "xdg-open"
	}
}

func (c *Controller) getBridgePath() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".gabs", c.spec.GameId, "bridge.json")
	}
	return filepath.Join(homeDir, ".gabs", c.spec.GameId, "bridge.json")
}

func (c *Controller) stopByProcessName(processName string, force bool, grace time.Duration) error {
	pids, err := c.findScopedProcesses(processName)
	if err != nil {
		return fmt.Errorf("failed to find processes named '%s': %w", processName, err)
	}

	if len(pids) == 0 {
		return fmt.Errorf("no processes found with name '%s'", processName)
	}

	var lastErr error
	stopped := 0
	for _, pid := range pids {
		if force {
			if err := killProcess(pid); err != nil {
				lastErr = err
			} else {
				stopped++
			}
		} else {
			if err := terminateProcess(pid, grace); err != nil {
				lastErr = err
			} else {
				stopped++
			}
		}
	}

	if stopped == 0 {
		if lastErr != nil {
			return fmt.Errorf("failed to stop any processes named '%s': %w", processName, lastErr)
		}
		return fmt.Errorf("failed to stop any processes named '%s'", processName)
	}

	return nil
}

// ProcessError represents different types of process-related errors
type ProcessError struct {
	Type    ProcessErrorType
	Context string
	Err     error
}

type ProcessErrorType int

const (
	ProcessErrorTypeConfiguration ProcessErrorType = iota
	ProcessErrorTypeStart
	ProcessErrorTypeStop
	ProcessErrorTypeStatus
	ProcessErrorTypeNotFound
)

func (e *ProcessError) Error() string {
	switch e.Type {
	case ProcessErrorTypeConfiguration:
		return fmt.Sprintf("configuration error (%s): %v", e.Context, e.Err)
	case ProcessErrorTypeStart:
		return fmt.Sprintf("start error (%s): %v", e.Context, e.Err)
	case ProcessErrorTypeStop:
		return fmt.Sprintf("stop error (%s): %v", e.Context, e.Err)
	case ProcessErrorTypeStatus:
		return fmt.Sprintf("status check error (%s): %v", e.Context, e.Err)
	case ProcessErrorTypeNotFound:
		return fmt.Sprintf("process not found (%s): %v", e.Context, e.Err)
	default:
		return fmt.Sprintf("process error (%s): %v", e.Context, e.Err)
	}
}

// Helper functions for cross-platform process management
func getTerminationSignal() os.Signal {
	switch runtime.GOOS {
	case "windows":
		return os.Interrupt
	default:
		return syscall.SIGTERM
	}
}

// killProcess forcefully terminates a process by PID
func killProcess(pid int) error {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid))
		return cmd.Run()
	default:
		// Unix-like systems
		process, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return process.Kill()
	}
}

// terminateProcess gracefully terminates a process by PID with a timeout
func terminateProcess(pid int, grace time.Duration) error {
	switch runtime.GOOS {
	case "windows":
		// On Windows, try gentle termination first, then force kill if timeout
		cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid))
		if err := cmd.Run(); err != nil {
			return err
		}

		// Wait for process to exit gracefully
		if grace > 0 {
			time.Sleep(grace)
			// Check if process still exists
			checkCmd := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV")
			output, err := checkCmd.Output()
			if err == nil && strings.Contains(string(output), strconv.Itoa(pid)) {
				// Process still exists, force kill it
				return killProcess(pid)
			}
		}
		return nil
	default:
		// Unix-like systems
		process, err := os.FindProcess(pid)
		if err != nil {
			return err
		}

		// Send SIGTERM
		if err := process.Signal(syscall.SIGTERM); err != nil {
			return err
		}

		// Wait for graceful shutdown with timeout
		if grace > 0 {
			done := make(chan error, 1)
			go func() {
				_, err := process.Wait()
				done <- err
			}()

			select {
			case <-done:
				return nil
			case <-time.After(grace):
				// Grace period expired, force kill
				return process.Kill()
			}
		}

		return nil
	}
}
