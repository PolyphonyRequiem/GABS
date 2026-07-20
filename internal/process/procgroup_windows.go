//go:build windows

package process

import (
	"os/exec"
	"syscall"
)

// CREATE_NEW_PROCESS_GROUP creates a process group whose leader is the child.
const createNewProcessGroup = 0x00000200

func setWindowsProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
}
