//go:build windows

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// proc is a child started suspended, placed in a Job Object, then resumed (ruling B15).
type proc struct {
	job    windows.Handle
	handle windows.Handle
	pid    uint32
	out    *os.File // read end of the merged stdout/stderr pipe

	waitOnce sync.Once
	done     chan struct{}
	exitCode uint32
	peak     uint64
}

// spawnOptions controls how the child is started.
type spawnOptions struct {
	Exe       string
	Args      []string
	Dir       string
	Level     Level  // how the child is isolated; there is no level below low integrity
	MemLimit  uint64 // job memory cap in bytes, 0 = none
	GPUSelect string
}

func spawn(o spawnOptions) (*proc, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			windows.CloseHandle(job)
		}
	}()
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
		windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS | windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION
	info.BasicLimitInformation.ActiveProcessLimit = 1
	if o.MemLimit > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(o.MemLimit)
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	// Our own pipes: one merged stdout/stderr pipe, stdin from NUL. Only these two handles are
	// inherited (PROC_THREAD_ATTRIBUTE_HANDLE_LIST), never anything else the agent holds.
	var rd, wr windows.Handle
	if err := windows.CreatePipe(&rd, &wr, nil, 64*1024); err != nil {
		return nil, fmt.Errorf("CreatePipe: %w", err)
	}
	if err := windows.SetHandleInformation(wr, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		windows.CloseHandle(rd)
		windows.CloseHandle(wr)
		return nil, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	nul, err := windows.CreateFile(windows.StringToUTF16Ptr("NUL"), windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, sa, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		windows.CloseHandle(rd)
		windows.CloseHandle(wr)
		return nil, fmt.Errorf("open NUL: %w", err)
	}
	defer windows.CloseHandle(nul)
	defer windows.CloseHandle(wr) // the child holds its own copy
	readEnd := os.NewFile(uintptr(rd), "engine-output")
	defer func() {
		if !ok {
			readEnd.Close()
		}
	}()

	attrs, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	inherit := []windows.Handle{nul, wr}
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&inherit[0]),
		uintptr(len(inherit))*unsafe.Sizeof(inherit[0])); err != nil {
		return nil, err
	}
	si := &windows.StartupInfoEx{}
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput = nul
	si.StdOutput = wr
	si.StdErr = wr
	si.ProcThreadAttributeList = attrs.List()

	exe16, err := windows.UTF16PtrFromString(o.Exe)
	if err != nil {
		return nil, err
	}
	cmd16, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{o.Exe}, o.Args...)))
	if err != nil {
		return nil, err
	}
	dir16, err := windows.UTF16PtrFromString(o.Dir)
	if err != nil {
		return nil, err
	}
	env := envBlock(childEnv(o.GPUSelect))
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW | windows.CREATE_UNICODE_ENVIRONMENT |
		windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_DEFAULT_ERROR_MODE)

	var caps securityCapabilities
	var pi windows.ProcessInformation
	switch o.Level {
	case LevelAppContainer:
		sid, err := engineContainer()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSandboxUnavailable, err)
		}
		caps.AppContainerSid = sid // and no capabilities: no network of any kind
		if err := attrs.Update(procThreadAttributeSecurityCapabilities, unsafe.Pointer(&caps), unsafe.Sizeof(caps)); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSandboxUnavailable, err)
		}
		if err := windows.CreateProcess(exe16, cmd16, nil, nil, true, flags, &env[0], dir16, &si.StartupInfo, &pi); err != nil {
			if tokenError(err) {
				return nil, fmt.Errorf("%w: CreateProcess: %v", ErrSandboxUnavailable, err)
			}
			return nil, fmt.Errorf("CreateProcess: %w", err)
		}
	case LevelLowIntegrity:
		tok, err := lowIntegrityToken()
		if err != nil {
			return nil, fmt.Errorf("%w: low integrity: %v", ErrSandboxUnavailable, err)
		}
		err = windows.CreateProcessAsUser(tok, exe16, cmd16, nil, nil, true, flags, &env[0], dir16, &si.StartupInfo, &pi)
		tok.Close()
		if err != nil {
			if tokenError(err) {
				return nil, fmt.Errorf("%w: low integrity: CreateProcessAsUser: %v", ErrSandboxUnavailable, err)
			}
			return nil, fmt.Errorf("CreateProcessAsUser: %w", err)
		}
	default:
		return nil, fmt.Errorf("unknown isolation level %d", o.Level)
	}
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Thread)
		windows.CloseHandle(pi.Process)
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		windows.TerminateJobObject(job, 1)
		windows.CloseHandle(pi.Thread)
		windows.CloseHandle(pi.Process)
		return nil, fmt.Errorf("ResumeThread: %w", err)
	}
	windows.CloseHandle(pi.Thread)
	ok = true
	p := &proc{job: job, handle: pi.Process, pid: pi.ProcessId, out: readEnd, done: make(chan struct{})}
	go p.wait()
	return p, nil
}

func (p *proc) wait() {
	windows.WaitForSingleObject(p.handle, windows.INFINITE)
	var code uint32
	windows.GetExitCodeProcess(p.handle, &code)
	p.exitCode = code
	close(p.done)
}

// PID is the child's process id.
func (p *proc) PID() uint32 { return p.pid }

// Done is closed when the child exits.
func (p *proc) Done() <-chan struct{} { return p.done }

// ExitCode is valid after Done.
func (p *proc) ExitCode() uint32 { return p.exitCode }

// Kill terminates everything in the job and releases the handles.
func (p *proc) Kill() {
	windows.TerminateJobObject(p.job, 1)
	<-p.done
	p.waitOnce.Do(func() {
		p.peak = p.jobPeak() // the job remembers its peak after the process is gone, until the handle closes
		windows.CloseHandle(p.handle)
		windows.CloseHandle(p.job) // KILL_ON_JOB_CLOSE: also the safety net if TerminateJobObject failed
	})
}

// jobPeak is the most memory the job ever had committed, the figure the job memory cap is held against.
func (p *proc) jobPeak() uint64 {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(p.job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return 0
	}
	return uint64(info.PeakJobMemoryUsed)
}

// PeakMemory is the job's peak committed memory in bytes: live while the engine runs, final after Kill.
func (p *proc) PeakMemory() uint64 {
	select {
	case <-p.done:
		if p.peak != 0 {
			return p.peak
		}
	default:
	}
	return p.jobPeak()
}

// tokenError tells a failure of the sandbox's token from any other: the process was not created because
// of what the token is or may do, not because of the file, its arguments or the model.
func tokenError(err error) bool {
	for _, code := range []windows.Errno{
		windows.ERROR_PRIVILEGE_NOT_HELD, windows.ERROR_NO_TOKEN, windows.ERROR_BAD_TOKEN_TYPE,
		windows.ERROR_TOKEN_ALREADY_IN_USE, windows.ERROR_BAD_IMPERSONATION_LEVEL, windows.ERROR_INVALID_OWNER,
		windows.Errno(1367), // ERROR_NO_SUCH_LOGON_SESSION
		windows.Errno(1385), // ERROR_LOGON_TYPE_NOT_GRANTED
	} {
		if errors.Is(err, code) {
			return true
		}
	}
	return false
}

// lowIntegrityToken duplicates our primary token and lowers it to the low mandatory level (S-1-16-4096).
func lowIntegrityToken() (windows.Token, error) {
	var cur windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ADJUST_DEFAULT|windows.TOKEN_ASSIGN_PRIMARY, &cur); err != nil {
		return 0, err
	}
	defer cur.Close()
	var dup windows.Token
	if err := windows.DuplicateTokenEx(cur, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation, windows.TokenPrimary, &dup); err != nil {
		return 0, err
	}
	sid, err := windows.CreateWellKnownSid(windows.WinLowLabelSid)
	if err != nil {
		dup.Close()
		return 0, err
	}
	tml := windows.Tokenmandatorylabel{Label: windows.SIDAndAttributes{Sid: sid, Attributes: windows.SE_GROUP_INTEGRITY}}
	if err := windows.SetTokenInformation(dup, windows.TokenIntegrityLevel, (*byte)(unsafe.Pointer(&tml)), tml.Size()); err != nil {
		dup.Close()
		return 0, err
	}
	return dup, nil
}

// engineEnvAllow is everything the engine's environment may hold, by name, upper case. It is a list
// of what Windows and the CUDA runtime need, so nothing else on the PC reaches the engine: not a token
// (HF_TOKEN), not llama-server's own LLAMA_ARG_* flags, not GGML_BACKEND_PATH or CUDA_INJECTION64_PATH,
// which load a DLL of someone's choosing.
var engineEnvAllow = map[string]bool{
	"SYSTEMROOT": true, "SYSTEMDRIVE": true, "WINDIR": true, "OS": true, "COMSPEC": true,
	"TEMP": true, "TMP": true, "USERPROFILE": true, "USERNAME": true, "HOMEDRIVE": true, "HOMEPATH": true,
	"LOCALAPPDATA": true, "APPDATA": true, "PROGRAMDATA": true, "ALLUSERSPROFILE": true, "PUBLIC": true,
	"PROGRAMFILES": true, "PROGRAMFILES(X86)": true, "PROGRAMW6432": true,
	"COMMONPROGRAMFILES": true, "COMMONPROGRAMFILES(X86)": true, "COMMONPROGRAMW6432": true,
	"NUMBER_OF_PROCESSORS": true, "PROCESSOR_ARCHITECTURE": true, "PROCESSOR_IDENTIFIER": true,
	"PROCESSOR_LEVEL": true, "PROCESSOR_REVISION": true, "COMPUTERNAME": true,
	"CUDA_DEVICE_ORDER": true, "CUDA_CACHE_PATH": true, "CUDA_CACHE_MAXSIZE": true, "CUDA_CACHE_DISABLE": true,
}

// childEnv builds the engine's environment from the allowlist. PATH is fixed to the system folders:
// the engine's own DLLs are found beside it, and the CUDA driver is in System32. gpuSelect "0".."7"
// limits the engine to that GPU, whatever the variable said before.
func childEnv(gpuSelect string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if ok && engineEnvAllow[strings.ToUpper(name)] {
			env = append(env, kv)
		}
	}
	if sys, err := windows.GetSystemDirectory(); err == nil {
		env = append(env, "PATH="+sys+";"+filepath.Dir(sys))
	}
	if len(gpuSelect) == 1 && gpuSelect[0] >= '0' && gpuSelect[0] <= '7' {
		env = append(env, "CUDA_VISIBLE_DEVICES="+gpuSelect)
	}
	sort.Slice(env, func(i, j int) bool { return strings.ToUpper(env[i]) < strings.ToUpper(env[j]) })
	return env
}

func envBlock(env []string) []uint16 {
	var b []uint16
	for _, kv := range env {
		if strings.IndexByte(kv, 0) >= 0 {
			continue
		}
		u, _ := windows.UTF16FromString(kv) // includes the terminating NUL
		b = append(b, u...)
	}
	if len(b) == 0 {
		b = append(b, 0)
	}
	return append(b, 0)
}

var (
	modIphlpapi             = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = modIphlpapi.NewProc("GetExtendedTcpTable")
)

// portOwner returns the PID listening on 127.0.0.1:port (IPv4), per B15's post-health check.
func portOwner(port int) (uint32, error) {
	const tcpTableOwnerPIDListener = 3
	size := uint32(16 * 1024)
	for tries := 0; tries < 4; tries++ {
		buf := make([]byte, size)
		r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0,
			windows.AF_INET, tcpTableOwnerPIDListener, 0)
		if r == uintptr(windows.ERROR_INSUFFICIENT_BUFFER) {
			continue
		}
		if r != 0 {
			return 0, fmt.Errorf("GetExtendedTcpTable: %d", r)
		}
		n := *(*uint32)(unsafe.Pointer(&buf[0]))
		type row struct{ State, LocalAddr, LocalPort, RemoteAddr, RemotePort, PID uint32 }
		rows := unsafe.Slice((*row)(unsafe.Pointer(&buf[4])), n)
		for _, rw := range rows {
			p := int(rw.LocalPort&0xff)<<8 | int(rw.LocalPort>>8&0xff)
			if p == port {
				return rw.PID, nil
			}
		}
		return 0, errors.New("no listener on the port")
	}
	return 0, errors.New("GetExtendedTcpTable: buffer kept growing")
}

// Integrity reports the child's mandatory integrity level ("low", "medium", "high", ...).
func (p *proc) Integrity() string {
	var tok windows.Token
	if err := windows.OpenProcessToken(p.handle, windows.TOKEN_QUERY, &tok); err != nil {
		return "unknown"
	}
	defer tok.Close()
	var n uint32
	windows.GetTokenInformation(tok, windows.TokenIntegrityLevel, nil, 0, &n)
	if n == 0 {
		return "unknown"
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(tok, windows.TokenIntegrityLevel, &buf[0], n, &n); err != nil {
		return "unknown"
	}
	tml := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buf[0]))
	sid := tml.Label.Sid
	rid := sid.SubAuthority(uint32(sid.SubAuthorityCount()) - 1)
	switch {
	case rid < 0x1000:
		return "untrusted"
	case rid < 0x2000:
		return "low"
	case rid < 0x3000:
		return "medium"
	case rid < 0x4000:
		return "high"
	default:
		return "system"
	}
}
