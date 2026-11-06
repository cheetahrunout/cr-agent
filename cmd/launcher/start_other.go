//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/cheetahrunout/cr-agent/internal/launch"
)

type agentProc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func startAgent(exe string, args []string) (launch.Proc, error) {
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &agentProc{cmd: cmd, done: make(chan struct{})}
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
	p.cmd.Process.Kill()
	<-p.done
}

func notify(string) {}
