//go:build windows

package hw

import (
	"errors"
	"fmt"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	modKernel32                          = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx             = modKernel32.NewProc("GlobalMemoryStatusEx")
	procGetLogicalProcessorInformationEx = modKernel32.NewProc("GetLogicalProcessorInformationEx")
)

// nvidiaSMIPath resolves nvidia-smi.exe in System32 only, never through PATH (ruling B17).
func nvidiaSMIPath() (string, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	p := filepath.Join(sys, "nvidia-smi.exe")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("nvidia-smi not found in System32: %w", err)
	}
	return p, nil
}

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func querySystem(diskPath string) (System, error) {
	var s System
	var errs []error

	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		errs = append(errs, fmt.Errorf("GlobalMemoryStatusEx: %w", err))
	} else {
		s.RAMTotalMB = int64(m.TotalPhys >> 20)
		s.RAMFreeMB = int64(m.AvailPhys >> 20)
	}

	if phys, logical, err := cores(); err != nil {
		errs = append(errs, err)
	} else {
		s.Physical, s.Logical = phys, logical
	}

	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		if v, _, err := k.GetStringValue("ProcessorNameString"); err == nil {
			s.CPUName = strings.TrimSpace(v)
		}
		k.Close()
	}

	if diskPath == "" {
		diskPath = "."
	}
	if abs, err := filepath.Abs(diskPath); err == nil {
		diskPath = abs
	}
	if p, err := windows.UTF16PtrFromString(diskPath); err == nil {
		var free, total, totalFree uint64
		if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
			errs = append(errs, fmt.Errorf("GetDiskFreeSpaceEx: %w", err))
		} else {
			s.DiskFreeMB, s.DiskTotalMB = int64(free>>20), int64(total>>20)
		}
	}

	s.OS = osName()
	return s, errors.Join(errs...)
}

// cores counts physical cores (one RelationProcessorCore record each) and logical processors (bits
// of their group masks).
func cores() (physical, logical int, err error) {
	const relationProcessorCore = 0
	var size uint32
	procGetLogicalProcessorInformationEx.Call(relationProcessorCore, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return 0, 0, errors.New("GetLogicalProcessorInformationEx: no size")
	}
	buf := make([]byte, size)
	if r, _, e := procGetLogicalProcessorInformationEx.Call(relationProcessorCore, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
		return 0, 0, fmt.Errorf("GetLogicalProcessorInformationEx: %w", e)
	}
	for off := uint32(0); off+8 <= size; {
		recSize := *(*uint32)(unsafe.Pointer(&buf[off+4]))
		if recSize == 0 || off+recSize > size {
			break
		}
		if *(*uint32)(unsafe.Pointer(&buf[off])) == relationProcessorCore {
			physical++
			groupCount := *(*uint16)(unsafe.Pointer(&buf[off+30]))
			for g := uint32(0); g < uint32(groupCount); g++ {
				maskOff := off + 32 + g*16
				if maskOff+8 > off+recSize {
					break
				}
				logical += bits.OnesCount64(*(*uint64)(unsafe.Pointer(&buf[maskOff])))
			}
		}
		off += recSize
	}
	return physical, logical, nil
}

func osName() string {
	v := windows.RtlGetVersion()
	name := "Windows"
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		if p, _, err := k.GetStringValue("ProductName"); err == nil {
			name = p
		}
		k.Close()
	}
	// ProductName still says "Windows 10" on Windows 11; the build number tells them apart.
	if v.BuildNumber >= 22000 {
		name = strings.Replace(name, "Windows 10", "Windows 11", 1)
	}
	return fmt.Sprintf("%s %d.%d.%d", name, v.MajorVersion, v.MinorVersion, v.BuildNumber)
}

// DiskSpace reports the bytes free to this user and the size of the volume holding path.
func DiskSpace(path string) (free, total uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, 0, fmt.Errorf("GetDiskFreeSpaceEx: %w", err)
	}
	return free, total, nil
}
