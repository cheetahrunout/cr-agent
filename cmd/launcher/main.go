// Command launcher is the small, stable program the installer puts beside the versions (ruling B22).
// It starts the agent version that current.json names, restarts into the pointer when the agent exits
// with code 75, holds a newly installed version to the two-minute health gate, and goes back to the
// previous version when the new one fails it. It verifies the signed manifest beside a version, with
// the same compiled-in trust list as the agent, before it starts that version.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cheetahrunout/cr-agent/internal/launch"
	"github.com/cheetahrunout/cr-agent/internal/trust"
)

func main() { os.Exit(run()) }

type logWriter struct{ f *os.File }

func (w logWriter) Write(p []byte) (int, error) {
	if w.f != nil {
		w.f.Write(p)
	}
	os.Stderr.Write(p) // fails silently in a build without a console
	return len(p), nil
}

func run() int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "launcher:", err)
		return 1
	}
	root := filepath.Dir(exe)
	os.MkdirAll(filepath.Join(root, "logs"), 0o700)
	path := filepath.Join(root, "logs", "launcher.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 2<<20 {
		os.Remove(path + ".1")
		os.Rename(path, path+".1")
	}
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if f != nil {
		defer f.Close()
	}
	log := slog.New(slog.NewJSONHandler(logWriter{f}, nil))

	// A release build runs only with a production signing key compiled in (ruling B20).
	if err := trust.Check(); err != nil {
		log.Error("this build cannot run", "err", err.Error())
		return 2
	}
	r := &launch.Runner{Root: root, Keys: trust.Keys(), Args: os.Args[1:], Log: log, Start: startAgent}
	return r.Run()
}
