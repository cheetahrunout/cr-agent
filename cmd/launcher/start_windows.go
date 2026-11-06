//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/cheetahrunout/cr-agent/internal/launch"
)

// agentProc is a running agent inside a Job Object: ending the launcher, or failing the health gate,
// ends the agent and everything it started (the engine runs in a job of its own, nested in this one).
type agentProc struct {
	cmd  *exec.Cmd
	job  windows.Handle
	done chan struct{}
	once sync.Once
}

func startAgent(exe string, args []string) (launch.Proc, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	if h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid)); err == nil {
		windows.AssignProcessToJobObject(job, h) // best effort: the agent still runs if this fails
		windows.CloseHandle(h)
	}
	p := &agentProc{cmd: cmd, job: job, done: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

func (p *agentProc) Done() <-chan struct{} { return p.done }

func (p *agentProc) ExitCode() int {
	if p.cmd.ProcessState == nil {
		return -1
	}
	return p.cmd.ProcessState.ExitCode()
}

func (p *agentProc) Kill() {
	p.once.Do(func() {
		windows.TerminateJobObject(p.job, 1)
		p.cmd.Process.Kill()
		<-p.done
		windows.CloseHandle(p.job)
	})
}

// notify shows the host a plain message box. The launcher stays small: no dialog library.
func notify(text string) {
	t, err1 := windows.UTF16PtrFromString(text)
	c, err2 := windows.UTF16PtrFromString("CR Agent")
	if err1 == nil && err2 == nil {
		windows.MessageBox(0, t, c, windows.MB_OK|windows.MB_ICONWARNING)
	}
}
