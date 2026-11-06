//go:build windows

package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/cheetahrunout/cr-agent/internal/confine"
)

// The engine's sandbox is an AppContainer with no capabilities (finding H2). Measured on the real
// engine, CUDA and RTX 5090 (docs/engine-sandbox.md): it cannot read the user's files, the agent's state
// or other drives, cannot reach any network address, and cannot be reached on loopback. So the agent
// talks to it over a Unix socket. It needs no administrator rights, and it replaces the low-integrity
// token: an AppContainer process is low integrity as well.
const containerName = "cr-agent-engine"

var (
	modUserenv                = windows.NewLazySystemDLL("userenv.dll")
	procCreateAppContainer    = modUserenv.NewProc("CreateAppContainerProfile")
	procDeriveAppContainerSid = modUserenv.NewProc("DeriveAppContainerSidFromAppContainerName")
	procGetContainerFolder    = modUserenv.NewProc("GetAppContainerFolderPath")
	procDeleteAppContainer    = modUserenv.NewProc("DeleteAppContainerProfile")

	containerMu      sync.Mutex
	currentContainer = containerName
	containerSid     *windows.SID
)

// procThreadAttributeSecurityCapabilities is PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES.
const procThreadAttributeSecurityCapabilities = 0x20009

type securityCapabilities struct {
	AppContainerSid *windows.SID
	Capabilities    *windows.SIDAndAttributes
	CapabilityCount uint32
	Reserved        uint32
}

// ScopeContainer gives each agent folder its own AppContainer, so that resetting one, as every agent
// start does, can never pull the socket from under another agent's running engine on this PC.
func ScopeContainer(home string) {
	sum := sha256.Sum256([]byte(strings.ToLower(home)))
	setContainerName(containerName + "-" + hex.EncodeToString(sum[:4]))
}

func setContainerName(name string) {
	containerMu.Lock()
	defer containerMu.Unlock()
	if currentContainer != name {
		currentContainer, containerSid = name, nil
	}
}

// engineContainer is the AppContainer's SID, creating its per-user profile the first time.
func engineContainer() (*windows.SID, error) {
	containerMu.Lock()
	defer containerMu.Unlock()
	if containerSid != nil {
		return containerSid, nil
	}
	sid, err := createContainerLocked()
	if err != nil {
		return nil, err
	}
	containerSid = sid
	return sid, nil
}

func createContainerLocked() (*windows.SID, error) {
	name, err := windows.UTF16PtrFromString(currentContainer)
	if err != nil {
		return nil, err
	}
	var sid *windows.SID
	r, _, _ := procCreateAppContainer.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(name)), 0, 0, uintptr(unsafe.Pointer(&sid)))
	if r != 0 { // already there, the usual case
		if r2, _, _ := procDeriveAppContainerSid.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&sid))); r2 != 0 {
			return nil, fmt.Errorf("AppContainer profile: create %#x, derive %#x", r, r2)
		}
	}
	return sid, nil
}

// ResetSandbox deletes the AppContainer's profile, with whatever is in it, and makes it again; the SID
// comes from the name, so it stays the same. Everything the container may write lies in that profile, so
// this clears all a model can leave behind. Call it only with no engine running.
func ResetSandbox() error {
	containerMu.Lock()
	defer containerMu.Unlock()
	if err := deleteContainerLocked(); err != nil {
		return err
	}
	sid, err := createContainerLocked()
	if err != nil {
		return err
	}
	containerSid = sid
	return nil
}

// DeleteContainer removes the AppContainer's profile and leaves none behind.
func DeleteContainer() error {
	containerMu.Lock()
	defer containerMu.Unlock()
	containerSid = nil
	return deleteContainerLocked()
}

func deleteContainerLocked() error {
	name, err := windows.UTF16PtrFromString(currentContainer)
	if err != nil {
		return err
	}
	if r, _, _ := procDeleteAppContainer.Call(uintptr(unsafe.Pointer(name))); r != 0 {
		return fmt.Errorf("AppContainer profile: delete %#x", r)
	}
	return nil
}

// containerTemp is the one folder the sandbox can write to, under the container's own profile.
func containerTemp(sid *windows.SID) (string, error) {
	s16, err := windows.UTF16PtrFromString(sid.String())
	if err != nil {
		return "", err
	}
	var p *uint16
	if r, _, _ := procGetContainerFolder.Call(uintptr(unsafe.Pointer(s16)), uintptr(unsafe.Pointer(&p))); r != 0 {
		return "", fmt.Errorf("GetAppContainerFolderPath: %#x", r)
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(p))
	return filepath.Join(windows.UTF16PtrToString(p), "Temp"), nil
}

var (
	modUser32         = windows.NewLazySystemDLL("user32.dll")
	procCreateDesktop = modUser32.NewProc("CreateDesktopW")
	procCloseDesktop  = modUser32.NewProc("CloseDesktop")
)

// privateDesktop makes an empty desktop for one child, on the same window station, never shown. A process
// on it can copy only that desktop's screen: nothing. AppContainers and the host may open it, and its low
// label lets a low-integrity process in. CUDA does not need the host's desktop.
func privateDesktop() (uintptr, *uint16, error) {
	var cur windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &cur); err != nil {
		return 0, nil, err
	}
	defer cur.Close()
	user, err := cur.GetTokenUser()
	if err != nil {
		return 0, nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:(A;;GA;;;" + user.User.Sid.String() + ")(A;;GA;;;AC)(A;;GA;;;SY)S:(ML;;NW;;;LW)")
	if err != nil {
		return 0, nil, err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name := "cr-engine-" + randHex(8)
	n16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, nil, err
	}
	h, _, callErr := procCreateDesktop.Call(uintptr(unsafe.Pointer(n16)), 0, 0, 0, uintptr(windows.GENERIC_ALL), uintptr(unsafe.Pointer(&sa)))
	if h == 0 {
		return 0, nil, callErr
	}
	full, err := windows.UTF16PtrFromString(`WinSta0\` + name)
	if err != nil {
		closeDesktop(h)
		return 0, nil, err
	}
	return h, full, nil
}

func closeDesktop(h uintptr) {
	if h != 0 {
		procCloseDesktop.Call(h)
	}
}

// maxSocketPath is what a sockaddr_un holds, without the terminating NUL.
const maxSocketPath = 107

// newSocketPath picks a fresh socket path. The folder lies in the container's own profile, where a
// model can have left anything: if the socket cannot be made there, the profile is made anew and it
// is tried once more, so that a model cannot break the sandbox for later loads.
func newSocketPath() (string, error) {
	p, err := trySocketPath()
	if err == nil {
		return p, nil
	}
	if rerr := ResetSandbox(); rerr != nil {
		return "", fmt.Errorf("%v; and %w", err, rerr)
	}
	return trySocketPath()
}

func trySocketPath() (string, error) {
	sid, err := engineContainer()
	if err != nil {
		return "", err
	}
	dir, err := containerTemp(sid)
	if err != nil {
		return "", err
	}
	// The agent writes here, in a folder the container controls: never through a link it left.
	if err := confine.NoReparse(filepath.Dir(dir), dir); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "e-"+randHex(4)+".sock")
	if len(p) > maxSocketPath {
		return "", fmt.Errorf("the engine's socket path is too long (%d bytes)", len(p))
	}
	// This Windows must have Unix sockets (10 version 1803 on). Better to know before any child runs.
	l, err := net.Listen("unix", p)
	if err != nil {
		return "", fmt.Errorf("no Unix socket here: %w", err)
	}
	l.Close()
	os.Remove(p)
	return p, nil
}

// allowsRead reports whether path's DACL holds an allow entry for sid. A volume with no permissions
// (exFAT) has no DACL and allows everyone.
func allowsRead(path string, sid *windows.SID) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return true, nil // no DACL at all
	}
	if acl == nil {
		return true, nil
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, i, &ace) != nil {
			continue
		}
		if ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE && ace.Mask&(windows.GENERIC_READ|windows.FILE_READ_DATA) != 0 &&
			windows.EqualSid((*windows.SID)(unsafe.Pointer(&ace.SidStart)), sid) {
			return true, nil
		}
	}
	return false, nil
}

// noLinkOnTheWay refuses a path with a reparse point on it: a grant through a link would land on whatever
// it leads to.
func noLinkOnTheWay(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := confine.NoReparse(filepath.VolumeName(abs)+`\`, abs); err != nil {
		return fmt.Errorf("%s is reached through a link", abs)
	}
	return nil
}

// grantRead adds one read-only entry for sid to a folder, inherited below it, or to one file.
func grantRead(dir string, sid *windows.SID) error {
	inherit := uint32(windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
	if st, err := os.Stat(dir); err == nil && !st.IsDir() {
		inherit = windows.NO_INHERITANCE
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	old, _, err := sd.DACL()
	if err != nil {
		return nil // no DACL: this volume has no permissions to change
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_READ | windows.GENERIC_EXECUTE,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inherit,
		Trustee:           windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_GROUP, TrusteeValue: windows.TrusteeValueFromSID(sid)},
	}}, old)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

// allPackages is ALL APPLICATION PACKAGES, the SID every read grant for the sandbox goes to. Not the
// container's own, for a measured reason: with the container's own SID on a file, a low-integrity process
// no longer gets it (llama-server cannot load such DLLs, and cannot open such a model), and an older
// version of this agent after a rollback is one. The cost: any other AppContainer on this PC can read these
// folders too. They hold the signed engine and models, and no other program gains anything.
func allPackages() (*windows.SID, error) { return windows.StringToSid("S-1-15-2-1") }

func grantEngineFolder(dir string) error {
	all, err := allPackages()
	if err != nil {
		return err
	}
	ok, err := allowsRead(dir, all)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return grantRead(dir, all)
}

// GrantModelFolder lets the engine's sandbox read a model folder, and reports whether that added an
// entry. It is the only change the agent makes to a folder's permissions outside the engine's: the
// caller is the agent, for its own model folder, or the host's click for one the host chose.
func GrantModelFolder(dir string) (added bool, err error) {
	if err := noLinkOnTheWay(dir); err != nil {
		return false, err
	}
	all, err := allPackages()
	if err != nil {
		return false, err
	}
	ok, err := allowsRead(dir, all)
	if err != nil {
		return false, err
	}
	if ok {
		return false, nil
	}
	return true, grantRead(dir, all)
}

// RevokeModelGrant takes away an entry the agent added, from a folder (and what inherited it) or a file.
// The caller knows it added this one: it removes every explicit entry for the SID at that path.
func RevokeModelGrant(path string) error {
	if err := noLinkOnTheWay(path); err != nil {
		return err
	}
	all, err := allPackages()
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	old, _, err := sd.DACL()
	if err != nil {
		return nil // no DACL: this volume has no permissions to change
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessMode: windows.REVOKE_ACCESS,
		Trustee:    windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_GROUP, TrusteeValue: windows.TrusteeValueFromSID(all)},
	}}, old)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

// CanEngineRead reports whether a model folder already lets the sandbox read it.
func CanEngineRead(dir string) bool {
	all, err := allPackages()
	if err != nil {
		return false
	}
	ok, err := allowsRead(dir, all)
	return err == nil && ok
}

// prepareModel makes the model readable inside a folder the agent may grant. A file moved in from
// elsewhere on the volume keeps its own permissions, so what counts is each file beside the model.
// Every entry it adds is reported, so that it can be taken away again.
func prepareModel(folder, model string, added func(string)) error {
	if folder == "" {
		return nil
	}
	if added == nil {
		added = func(string) {}
	}
	if err := noLinkOnTheWay(folder); err != nil {
		return err
	}
	all, err := allPackages()
	if err != nil {
		return err
	}
	ok, err := allowsRead(folder, all)
	if err != nil {
		return err
	}
	if !ok {
		if err := grantRead(folder, all); err != nil {
			return err
		}
		added(folder)
	}
	for _, p := range modelFiles(model) {
		if err := noLinkOnTheWay(p); err != nil {
			return err
		}
		ok, err := allowsRead(p, all)
		if err != nil {
			return err
		}
		if !ok {
			if err := grantRead(p, all); err != nil {
				return err
			}
			added(p)
		}
	}
	return nil
}

// modelFiles is the model and the other parts of its set: every .gguf beside it.
func modelFiles(model string) []string {
	all, _ := filepath.Glob(filepath.Join(filepath.Dir(model), "*.gguf"))
	for _, p := range all {
		if strings.EqualFold(p, model) {
			return all
		}
	}
	return append(all, model)
}

// prepareSandbox is everything the engine's sandbox needs before a start.
func prepareSandbox(engineDir, folder, model string, added func(string)) error {
	if _, err := engineContainer(); err != nil {
		return err
	}
	if err := grantEngineFolder(engineDir); err != nil {
		return fmt.Errorf("engine folder: %w", err)
	}
	if err := prepareModel(folder, model, added); err != nil {
		return fmt.Errorf("model folder: %w", err)
	}
	return nil
}

// sioAFUnixGetPeerPID is SIO_AF_UNIX_GETPEERPID.
const sioAFUnixGetPeerPID = 0x58000100

// socketOwner is the process listening on a Unix socket, B15's check that the answer comes from the engine.
func socketOwner(path string) (uint32, error) {
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	raw, err := c.(*net.UnixConn).SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid, n uint32
	var ioerr error
	if err := raw.Control(func(fd uintptr) {
		ioerr = windows.WSAIoctl(windows.Handle(fd), sioAFUnixGetPeerPID, nil, 0, (*byte)(unsafe.Pointer(&pid)), 4, &n, nil, 0)
	}); err != nil {
		return 0, err
	}
	if ioerr != nil { // this Windows cannot say who is listening: the failed check is the PC's, not a model's
		return 0, fmt.Errorf("%w: %v", ErrSandboxUnavailable, ioerr)
	}
	if pid == 0 {
		return 0, fmt.Errorf("no peer process id (%d bytes)", n)
	}
	return pid, nil
}
