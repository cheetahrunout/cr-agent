//go:build !windows

package store

import "errors"

// There is no secret store on other platforms yet: secrets are refused, not written in the clear.
type noProtector struct{}

func defaultProtector() Protector { return noProtector{} }

func (noProtector) Protect(string, []byte) ([]byte, error) {
	return nil, errors.New("secret storage: Windows only in this build")
}

func (noProtector) Unprotect(string, []byte) ([]byte, error) {
	return nil, errors.New("secret storage: Windows only in this build")
}
