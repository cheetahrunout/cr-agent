package models

import (
	"os"

	"github.com/cheetahrunout/cr-agent/internal/confine"
)

// noReparse and underRoot keep every write inside the model folder (internal/confine).
func noReparse(root, path string) error { return confine.NoReparse(root, path) }

func underRoot(f *os.File, root string) error { return confine.UnderRoot(f, root) }
