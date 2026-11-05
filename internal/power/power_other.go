//go:build !windows

package power

import "errors"

// Watch is Windows only in this build.
func Watch(func(Event)) (stop func(), err error) {
	return nil, errors.New("power notifications: Windows only")
}
