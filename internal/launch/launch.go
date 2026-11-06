// Package launch is the launcher's logic (ruling B22): start the agent version current.json names,
// restart into the pointer on exit code 75, hold a new version to the health gate, and go back to
// the previous version when the new one does not write healthy.<version> within two minutes or
// exits before it does.
package launch

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/layout"
	"github.com/cheetahrunout/cr-agent/internal/release"
	"github.com/cheetahrunout/cr-agent/internal/trust"
)

// Exit codes of the agent the launcher gives a meaning to.
const (
	ExitQuit    = 0                  // the host chose Quit: the launcher ends too
	ExitRefused = 2                  // the agent refused to start (bad flags, another copy running): no restart
	ExitRestart = layout.ExitRestart // start whatever the pointer names now
)

// Proc is a started agent.
type Proc interface {
	Done() <-chan struct{} // closed when the process has exited
	ExitCode() int         // valid after Done
	Kill()                 // ends the process (and everything it started) and waits for it
}

// Runner runs agents until one quits.
type Runner struct {
	Root string
	Keys []trust.Key
	Args []string // passed to the agent after --home <root>
	Log  *slog.Logger
	// Start starts the agent binary of a version.
	Start func(exe string, args []string) (Proc, error)
	// Notify tells the host when the launcher gives up (it has no other way: no console, no tray).
	Notify func(text string)

	Gate       time.Duration // 2 min
	Poll       time.Duration // how often healthy.<version> is looked for (250 ms)
	CrashPause time.Duration // wait before restarting a crashed agent (3 s)
	MaxCrashes int           // crashes in a row before the launcher gives up (5)
}

func (r *Runner) defaults() {
	if r.Gate <= 0 {
		r.Gate = layout.HealthGate
	}
	if r.Poll <= 0 {
		r.Poll = 250 * time.Millisecond
	}
	if r.CrashPause <= 0 {
		r.CrashPause = 3 * time.Second
	}
	if r.MaxCrashes <= 0 {
		r.MaxCrashes = 5
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.Notify == nil {
		r.Notify = func(string) {}
	}
}

// damaged is what the host reads when a file of the running version is missing or changed. Antivirus software removing
// one is the usual reason, and the signature check is why the agent will not run without it.
const damaged = "CR Agent did not start because one of its files is missing or changed, so this PC is not sharing now. Antivirus software may have removed it: check its history. Do not add an exclusion; tell whoever runs your hub."

// gaveUp is what the host reads when the agent keeps failing. It does not advise reinstalling: the
// usual causes pass (a model drive that is not connected yet).
const gaveUp = "CR Agent could not start after several tries, so this PC is not sharing now. It tries again the next time you sign in, or start it from the Start menu. The reason is in its log, in the logs folder of CR Agent."

// Verify checks a version before it is started: when its signed manifest is beside it, the signature
// must come from a trusted key and the agent binary must have the listed hash. A version without a
// manifest is a fresh install that has not fetched it yet; it is started as it is.
func Verify(root, version string, keys []trust.Key) error {
	exe := layout.AgentExe(root, version)
	if st, err := os.Stat(exe); err != nil || st.IsDir() {
		return fmt.Errorf("version %s is not on disk", version)
	}
	raw, rerr := os.ReadFile(layout.ManifestFile(root, version))
	sig, serr := os.ReadFile(layout.ManifestFile(root, version) + ".minisig")
	if errors.Is(rerr, os.ErrNotExist) { // manifest.json is written last: no manifest yet, whatever else is there
		return nil
	}
	if rerr != nil || serr != nil {
		return fmt.Errorf("version %s has a manifest without a signature", version)
	}
	m, err := release.Verify(raw, sig, keys)
	if err != nil {
		return fmt.Errorf("version %s: %w", version, err)
	}
	if m.Version != version {
		return fmt.Errorf("the manifest beside version %s is for %s", version, m.Version)
	}
	want, ok := m.File(m.AgentPath())
	sum, size, err := release.HashFile(exe)
	if !ok || err != nil || sum != want.SHA256 || size != want.Size {
		return fmt.Errorf("the agent binary of version %s does not match its signed manifest", version)
	}
	return nil
}

// revert points back at the previous version after a failed trial.
func (r *Runner) revert(p layout.Pointer, why string) (layout.Pointer, error) {
	if p.Previous == "" {
		return p, errors.New("no previous version to go back to")
	}
	// Never point at a version this launcher would refuse to start.
	if err := Verify(r.Root, p.Previous, r.Keys); err != nil {
		return p, fmt.Errorf("the previous version cannot be started either: %w", err)
	}
	r.Log.Error("reverting to the previous version", "failed", p.Version, "back_to", p.Previous, "why", why)
	back := layout.Pointer{Version: p.Previous}
	if err := layout.WritePointer(r.Root, back); err != nil {
		return p, err
	}
	os.Remove(layout.HealthyFile(r.Root, p.Version))
	return back, nil
}

// prune removes versions older than the current and the previous one: N-1 stays on disk.
func (r *Runner) prune(p layout.Pointer) {
	for _, v := range layout.Installed(r.Root) {
		if v == p.Version || v == p.Previous {
			continue
		}
		if err := os.RemoveAll(layout.VersionDir(r.Root, v)); err == nil {
			os.Remove(layout.HealthyFile(r.Root, v))
			r.Log.Info("old version removed", "version", v)
		}
	}
}

func healthy(root, version string) bool {
	_, err := os.Stat(layout.HealthyFile(root, version))
	return err == nil
}

// removeOldLaunchers clears launcher.old and launcher.old.N, left by launcher updates: this start runs the new one.
func (r *Runner) removeOldLaunchers() {
	entries, err := os.ReadDir(r.Root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if n := e.Name(); n == "launcher.old" || strings.HasPrefix(n, "launcher.old.") {
			os.Remove(filepath.Join(r.Root, n))
		}
	}
}

// Run is the launcher's loop. It returns the launcher's exit code.
func (r *Runner) Run() int {
	r.defaults()
	r.removeOldLaunchers()
	crashes := 0
	for {
		p, err := layout.ReadPointer(r.Root)
		if err != nil {
			r.Log.Error("no usable current.json", "err", err.Error())
			return 1
		}
		trial := p.PendingSince != nil
		if err := Verify(r.Root, p.Version, r.Keys); err != nil {
			if !trial {
				r.Log.Error("the installed version cannot be started", "err", err.Error())
				r.Notify(damaged)
				return 1
			}
			if _, rerr := r.revert(p, err.Error()); rerr != nil {
				r.Log.Error("cannot revert", "err", rerr.Error())
				return 1
			}
			continue
		}
		// A trial's mark is not removed here: the updater removes it before it flips the pointer, and
		// a revert removes it. One that is here was written by this version in a trial this launcher
		// did not see end (a reboot), and its release may be committed: reverting now would strand it.
		t0 := time.Now()
		proc, err := r.Start(layout.AgentExe(r.Root, p.Version), append([]string{"--home", r.Root}, r.Args...))
		if err != nil {
			r.Log.Error("the agent did not start", "version", p.Version, "err", err.Error())
			if trial {
				if _, rerr := r.revert(p, "did not start"); rerr == nil {
					continue
				}
			}
			return 1
		}
		r.Log.Info("agent started", "version", p.Version, "on_trial", trial)

		if trial {
			passed, why := r.gate(proc, p.Version, t0)
			if !passed {
				proc.Kill()
				if _, rerr := r.revert(p, why); rerr != nil {
					r.Log.Error("cannot revert", "err", rerr.Error())
					return 1
				}
				continue
			}
			p.PendingSince = nil
			if err := layout.WritePointer(r.Root, p); err != nil {
				r.Log.Error("pointer not committed", "err", err.Error())
			}
			r.Log.Info("new version is healthy", "version", p.Version, "after_ms", time.Since(t0).Milliseconds())
			r.prune(p)
		}

		<-proc.Done()
		code := proc.ExitCode()
		switch code {
		case ExitQuit:
			r.Log.Info("agent quit; launcher ends", "version", p.Version)
			return 0
		case ExitRefused:
			r.Log.Error("the agent refused to start; not restarting", "version", p.Version)
			return ExitRefused
		case ExitRestart:
			r.Log.Info("agent asked for a restart into the pointer", "version", p.Version)
			crashes = 0
			continue
		}
		if time.Since(t0) > 10*time.Minute {
			crashes = 0 // it ran for a while: not a crash loop
		}
		crashes++
		r.Log.Error("agent ended unexpectedly", "version", p.Version, "exit_code", code, "crashes_in_a_row", crashes)
		if crashes >= r.MaxCrashes {
			r.Log.Error("the agent keeps crashing; launcher gives up")
			r.Notify(gaveUp)
			return 1
		}
		time.Sleep(r.CrashPause)
	}
}

// gate waits for a version on trial: healthy.<version> within the gate passes; exiting first, or the
// time running out, fails. The mark is judged only after the process is gone: an agent writes the mark
// and then commits its release, so a commit can never be followed by a revert to a version older than
// it, which that agent would refuse, however a hub times the agent's first connection.
func (r *Runner) gate(proc Proc, version string, t0 time.Time) (bool, string) {
	deadline := time.NewTimer(r.Gate - time.Since(t0))
	defer deadline.Stop()
	tick := time.NewTicker(r.Poll)
	defer tick.Stop()
	for {
		if healthy(r.Root, version) {
			return true, ""
		}
		select {
		case <-proc.Done():
			if healthy(r.Root, version) {
				return true, "" // it got there; the loop below deals with how it ended
			}
			return false, fmt.Sprintf("exited with code %d before it was healthy", proc.ExitCode())
		case <-deadline.C:
			proc.Kill()
			select { // a kill is not instant; a process that ignores it must not hold the launcher
			case <-proc.Done():
			case <-time.After(2 * time.Second):
			}
			if healthy(r.Root, version) {
				return true, ""
			}
			return false, fmt.Sprintf("not healthy within %s", r.Gate)
		case <-tick.C:
		}
	}
}
