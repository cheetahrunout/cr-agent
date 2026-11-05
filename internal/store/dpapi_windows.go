//go:build windows

package store

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dpapi protects with CryptProtectData for the current user. The secret's name is mixed in as
// entropy, so a blob copied over another secret's file does not open.
type dpapi struct{}

func defaultProtector() Protector { return dpapi{} }

func blobOf(b []byte) windows.DataBlob {
	if len(b) == 0 {
		return windows.DataBlob{}
	}
	return windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
}

func entropy(name string) []byte { return []byte("cr-agent/v1/" + name) }

func (dpapi) Protect(name string, plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, errors.New("empty secret")
	}
	in, ent := blobOf(plain), blobOf(entropy(name))
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, &ent, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

func (dpapi) Unprotect(name string, blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("empty blob")
	}
	in, ent := blobOf(blob), blobOf(entropy(name))
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, &ent, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, errors.New("DPAPI could not open the blob")
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
