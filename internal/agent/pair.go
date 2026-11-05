package agent

import (
	"context"
	"errors"

	"github.com/cheetahrunout/cr-agent/internal/pairing"
	"github.com/cheetahrunout/cr-agent/internal/store"
)

// ConnectStored starts the link with the stored pairing. It reports false when this PC is not paired.
func (a *Agent) ConnectStored() bool {
	st := a.cfg.Store
	if st == nil {
		return false
	}
	key, err := st.MachineKey()
	hub := st.Config().Hub
	if err != nil || hub == "" {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			a.log.Error("the stored machine key cannot be read; pair again", "err", err.Error())
		}
		return false
	}
	// config.json is a plain file anyone running as the user can edit: the hub it names goes through
	// the build's host allowlist on every start, or a changed file could send the key elsewhere (B11).
	checked, err := a.cfg.CheckHub(hub)
	if err != nil {
		a.log.Error("the stored hub is not one this build accepts; pair again", "err", err.Error())
		return false
	}
	a.Connect(checked, key)
	return true
}

// PairWith runs the pairing exchange (ruling B1) against an already validated hub URL and code,
// stores the new machine key, and connects with it. A PC that was paired before sends its machine id,
// so the hub can offer "Replace key"; waiting is called while the host makes that choice in the hub.
func (a *Agent) PairWith(ctx context.Context, hub, code string, pc *pairing.Client, waiting func()) (*pairing.Result, error) {
	st := a.cfg.Store
	if st == nil {
		return nil, errors.New("pairing needs a store")
	}
	if pc == nil {
		pc = &pairing.Client{}
	}
	res, err := pc.Pair(ctx, hub, code, st.Config().MachineID, waiting)
	if err != nil {
		return nil, err
	}
	if err := st.SavePairing(res.Hub, res.MachineID, res.Slug, res.KeyID, res.Key); err != nil {
		a.log.Error("pairing not saved", "err", err.Error())
		return nil, &pairing.Error{Problem: pairing.ProblemStore}
	}
	a.log.Info("paired", "machine_id", res.MachineID, "slug", res.Slug, "key_id", res.KeyID) // never the key
	a.Connect(res.Hub, res.Key)
	return res, nil
}

// Unpair stops the link and forgets the machine key.
func (a *Agent) Unpair() error {
	a.Disconnect()
	a.linkMu.Lock()
	a.linkHub, a.linkKey, a.phase = "", "", ""
	a.linkMu.Unlock()
	if a.cfg.Store == nil {
		return nil
	}
	err := a.cfg.Store.Unpair()
	a.cfg.OnChange()
	return err
}
