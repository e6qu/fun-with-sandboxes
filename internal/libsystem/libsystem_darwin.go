// Package libsystem calls the libSystem functions the engine needs that
// neither syscall nor golang.org/x/sys/unix offers: posix_spawn with its
// attributes, among them disclaiming responsibility, per-process resource
// usage, and I/O policy. It calls them as x/sys/unix calls libSystem, with
// no cgo.
package libsystem

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// Implemented in the runtime package (runtime/sys_darwin.go).
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname syscall_syscall syscall.syscall
//go:linkname syscall_syscall6 syscall.syscall6

// posix_spawn flags (spawn.h).
const (
	spawnSetPgroup      = 0x0002
	spawnSetSigDefault  = 0x0004
	spawnSetSigMask     = 0x0008
	spawnSetSID         = 0x0400
	spawnCloexecDefault = 0x4000
	allSignals          = ^uint32(0)
	rusageInfoV2        = 2
	rusageInfoV2Uint64s = 18
	rusageUUIDBytes     = 16
)

// SpawnAttr says how Spawn starts a process.
type SpawnAttr struct {
	// Dir is the working directory.
	Dir string
	// Files are the child's descriptors: Files[i] becomes descriptor i.
	// Every other descriptor of the parent is closed in the child.
	Files []uintptr
	// SetSID starts a session; otherwise the child leads a new process
	// group.
	SetSID bool
	// Disclaim makes the child the responsible process for itself, so it
	// does not inherit the privacy (TCC) grants of the process that
	// launched the engine.
	Disclaim bool
}

// Spawn starts path with argv and env through posix_spawn, signals reset
// to their defaults and unblocked, and returns its pid.
func Spawn(path string, argv, env []string, a SpawnAttr) (int, error) {
	cpath, err := syscall.BytePtrFromString(path)
	if err != nil {
		return 0, err
	}
	cargv, err := cStrings(argv)
	if err != nil {
		return 0, err
	}
	cenv, err := cStrings(env)
	if err != nil {
		return 0, err
	}
	var attr, actions uintptr
	if err := call(libc_posix_spawnattr_init_trampoline_addr, uintptr(unsafe.Pointer(&attr))); err != nil {
		return 0, fmt.Errorf("posix_spawnattr_init: %w", err)
	}
	defer call(libc_posix_spawnattr_destroy_trampoline_addr, uintptr(unsafe.Pointer(&attr)))
	if err := call(libc_posix_spawn_file_actions_init_trampoline_addr, uintptr(unsafe.Pointer(&actions))); err != nil {
		return 0, fmt.Errorf("posix_spawn_file_actions_init: %w", err)
	}
	defer call(libc_posix_spawn_file_actions_destroy_trampoline_addr, uintptr(unsafe.Pointer(&actions)))

	flags := spawnSetSigDefault | spawnSetSigMask | spawnCloexecDefault
	if a.SetSID {
		flags |= spawnSetSID
	} else {
		flags |= spawnSetPgroup
		if err := call(libc_posix_spawnattr_setpgroup_trampoline_addr, uintptr(unsafe.Pointer(&attr)), 0); err != nil {
			return 0, fmt.Errorf("posix_spawnattr_setpgroup: %w", err)
		}
	}
	if err := call(libc_posix_spawnattr_setflags_trampoline_addr, uintptr(unsafe.Pointer(&attr)), uintptr(flags)); err != nil {
		return 0, fmt.Errorf("posix_spawnattr_setflags: %w", err)
	}
	all, none := allSignals, uint32(0)
	if err := call(libc_posix_spawnattr_setsigdefault_trampoline_addr, uintptr(unsafe.Pointer(&attr)), uintptr(unsafe.Pointer(&all))); err != nil {
		return 0, fmt.Errorf("posix_spawnattr_setsigdefault: %w", err)
	}
	if err := call(libc_posix_spawnattr_setsigmask_trampoline_addr, uintptr(unsafe.Pointer(&attr)), uintptr(unsafe.Pointer(&none))); err != nil {
		return 0, fmt.Errorf("posix_spawnattr_setsigmask: %w", err)
	}
	if a.Disclaim {
		if err := call(libc_responsibility_spawnattrs_setdisclaim_trampoline_addr, uintptr(unsafe.Pointer(&attr)), 1); err != nil {
			return 0, fmt.Errorf("responsibility_spawnattrs_setdisclaim: %w", err)
		}
	}
	for i, fd := range a.Files {
		if err := call(libc_posix_spawn_file_actions_adddup2_trampoline_addr, uintptr(unsafe.Pointer(&actions)), fd, uintptr(i)); err != nil {
			return 0, fmt.Errorf("posix_spawn_file_actions_adddup2: %w", err)
		}
	}
	if a.Dir != "" {
		cdir, err := syscall.BytePtrFromString(a.Dir)
		if err != nil {
			return 0, err
		}
		if err := call(libc_posix_spawn_file_actions_addchdir_np_trampoline_addr, uintptr(unsafe.Pointer(&actions)), uintptr(unsafe.Pointer(cdir))); err != nil {
			return 0, fmt.Errorf("posix_spawn_file_actions_addchdir_np: %w", err)
		}
	}
	var pid int32
	r, _, _ := syscall_syscall6(libc_posix_spawn_trampoline_addr,
		uintptr(unsafe.Pointer(&pid)), uintptr(unsafe.Pointer(cpath)), uintptr(unsafe.Pointer(&actions)),
		uintptr(unsafe.Pointer(&attr)), uintptr(unsafe.Pointer(&cargv[0])), uintptr(unsafe.Pointer(&cenv[0])))
	if r != 0 {
		return 0, &spawnError{path: path, err: syscall.Errno(r)}
	}
	return int(pid), nil
}

type spawnError struct {
	path string
	err  syscall.Errno
}

func (e *spawnError) Error() string { return "posix_spawn " + e.path + ": " + e.err.Error() }
func (e *spawnError) Unwrap() error { return e.err }

// call runs a libSystem function that returns 0 or an error number.
func call(fn uintptr, args ...uintptr) error {
	var a [3]uintptr
	copy(a[:], args)
	r, _, _ := syscall_syscall(fn, a[0], a[1], a[2])
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func cStrings(ss []string) ([]*byte, error) {
	out := make([]*byte, 0, len(ss)+1)
	for _, s := range ss {
		p, err := syscall.BytePtrFromString(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return append(out, nil), nil
}

// ResponsiblePID is the process macOS holds responsible for pid in privacy
// (TCC) decisions.
func ResponsiblePID(pid int) int {
	r, _, _ := syscall_syscall(libc_responsibility_get_pid_responsible_for_pid_trampoline_addr, uintptr(pid), 0, 0)
	return int(int32(r))
}

// Usage is what the kernel accounts to one process.
type Usage struct {
	// CPU is its user and system time.
	CPU uint64
	// Footprint is its physical footprint in bytes, the measure macOS
	// itself limits memory by.
	Footprint uint64
	// Resident is its resident set size in bytes.
	Resident uint64
	// DiskRead and DiskWritten are its block I/O bytes.
	DiskRead, DiskWritten uint64
}

// ProcUsage reads pid's resource usage (proc_pid_rusage), CPU time in
// nanoseconds.
func ProcUsage(pid int) (Usage, error) {
	var buf struct {
		uuid [rusageUUIDBytes]byte
		v    [rusageInfoV2Uint64s]uint64
	}
	r, _, errno := syscall_syscall(libc_proc_pid_rusage_trampoline_addr, uintptr(pid), rusageInfoV2, uintptr(unsafe.Pointer(&buf)))
	if int32(r) != 0 {
		return Usage{}, errno
	}
	n, d := timebase()
	ticks := buf.v[0] + buf.v[1]
	return Usage{
		CPU:         ticks * uint64(n) / uint64(d),
		Resident:    buf.v[6],
		Footprint:   buf.v[7],
		DiskRead:    buf.v[16],
		DiskWritten: buf.v[17],
	}, nil
}

// timebase is mach_timebase_info: absolute-time ticks times numer over
// denom are nanoseconds.
func timebase() (uint32, uint32) {
	var tb struct{ numer, denom uint32 }
	syscall_syscall(libc_mach_timebase_info_trampoline_addr, uintptr(unsafe.Pointer(&tb)), 0, 0)
	if tb.denom == 0 {
		return 1, 1
	}
	return tb.numer, tb.denom
}

// SetIOPolicy sets the calling process's disk I/O policy, which children
// inherit.
func SetIOPolicy(policy int) error {
	const diskType, processScope = 0, 0
	r, _, errno := syscall_syscall(libc_setiopolicy_np_trampoline_addr, diskType, processScope, uintptr(policy))
	if int32(r) != 0 {
		return errno
	}
	return nil
}

// IOPolicy reads the calling process's disk I/O policy.
func IOPolicy() (int, error) {
	const diskType, processScope = 0, 0
	r, _, errno := syscall_syscall(libc_getiopolicy_np_trampoline_addr, diskType, processScope, 0)
	if int32(r) < 0 {
		return 0, errno
	}
	return int(int32(r)), nil
}

// ShmOpen opens or creates a POSIX shared memory object, returning its
// descriptor.
func ShmOpen(name string, flag int, mode uint32) (int, error) {
	cname, err := syscall.BytePtrFromString(name)
	if err != nil {
		return -1, err
	}
	r, _, errno := syscall_syscall(libc_shm_open_trampoline_addr, uintptr(unsafe.Pointer(cname)), uintptr(flag), uintptr(mode))
	if int32(r) < 0 {
		return -1, errno
	}
	return int(r), nil
}

// ShmUnlink removes a POSIX shared memory object's name.
func ShmUnlink(name string) error {
	cname, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	r, _, errno := syscall_syscall(libc_shm_unlink_trampoline_addr, uintptr(unsafe.Pointer(cname)), 0, 0)
	if int32(r) < 0 {
		return errno
	}
	return nil
}

// SandboxInit confines the calling process, and every program it executes,
// to the Seatbelt profile text, as sandbox-exec does before it executes its
// command.
func SandboxInit(profile string) error {
	cprofile, err := syscall.BytePtrFromString(profile)
	if err != nil {
		return err
	}
	params := [1]uintptr{0}
	var errbuf *byte
	r, _, _ := syscall_syscall6(libsandbox_sandbox_init_with_parameters_trampoline_addr,
		uintptr(unsafe.Pointer(cprofile)), 0, uintptr(unsafe.Pointer(&params[0])), uintptr(unsafe.Pointer(&errbuf)), 0, 0)
	if int32(r) == 0 {
		return nil
	}
	msg := "unknown error"
	if errbuf != nil {
		msg = goString(errbuf)
		syscall_syscall(libsandbox_sandbox_free_error_trampoline_addr, uintptr(unsafe.Pointer(errbuf)), 0, 0)
	}
	return fmt.Errorf("sandbox_init: %s", msg)
}

// sandboxProfile is libsandbox's compiled profile: its kind and its
// bytecode, as the kernel takes it.
type sandboxProfile struct {
	kind     uint32
	bytecode unsafe.Pointer
	length   uintptr
}

// SandboxCompile compiles the Seatbelt profile text into the bytecode the
// kernel enforces, prefixed with its kind, for SandboxApply to apply in
// another process: compiling a container's profile takes milliseconds,
// which a start that applies a compiled one does not pay.
func SandboxCompile(profile string) ([]byte, error) {
	cprofile, err := syscall.BytePtrFromString(profile)
	if err != nil {
		return nil, err
	}
	params, _, _ := syscall_syscall(libsandbox_sandbox_create_params_trampoline_addr, 0, 0, 0)
	var errbuf *byte
	r, _, _ := syscall_syscall(libsandbox_sandbox_compile_string_trampoline_addr,
		uintptr(unsafe.Pointer(cprofile)), params, uintptr(unsafe.Pointer(&errbuf)))
	if r == 0 {
		msg := "unknown error"
		if errbuf != nil {
			msg = goString(errbuf)
			syscall_syscall(libsandbox_sandbox_free_error_trampoline_addr, uintptr(unsafe.Pointer(errbuf)), 0, 0)
		}
		return nil, fmt.Errorf("sandbox_compile_string: %s", msg)
	}
	defer syscall_syscall(libsandbox_sandbox_free_profile_trampoline_addr, r, 0, 0)
	// r is libsandbox's allocation, outside Go's heap.
	p := *(**sandboxProfile)(unsafe.Pointer(&r))
	out := make([]byte, 4+p.length)
	binary.LittleEndian.PutUint32(out, p.kind)
	copy(out[4:], unsafe.Slice((*byte)(p.bytecode), p.length))
	return out, nil
}

// SandboxApply confines the calling process, and every program it
// executes, to a profile SandboxCompile compiled.
func SandboxApply(compiled []byte) error {
	if len(compiled) <= 4 {
		return fmt.Errorf("sandbox_apply: a compiled profile of %d bytes", len(compiled))
	}
	p := sandboxProfile{
		kind:     binary.LittleEndian.Uint32(compiled),
		bytecode: unsafe.Pointer(&compiled[4]),
		length:   uintptr(len(compiled) - 4),
	}
	r, _, errno := syscall_syscall(libsandbox_sandbox_apply_trampoline_addr, uintptr(unsafe.Pointer(&p)), 0, 0)
	runtime.KeepAlive(compiled)
	if int32(r) != 0 {
		return fmt.Errorf("sandbox_apply: %w", errno)
	}
	return nil
}

func goString(p *byte) string {
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

// ProcPidinfo is proc_pidinfo: it fills buf with pid's information of
// flavor and returns how many bytes it wrote.
func ProcPidinfo(pid, flavor int, arg uint64, buf []byte) (int, error) {
	var p unsafe.Pointer
	if len(buf) > 0 {
		p = unsafe.Pointer(&buf[0])
	}
	r, _, errno := syscall_syscall6(libc_proc_pidinfo_trampoline_addr, uintptr(pid), uintptr(flavor), uintptr(arg), uintptr(p), uintptr(len(buf)), 0)
	if n := int(int32(r)); n > 0 {
		return n, nil
	}
	if errno == 0 {
		errno = syscall.ESRCH
	}
	return 0, errno
}

// ProcPidpath is proc_pidpath: the path of pid's executable.
func ProcPidpath(pid int) (string, error) {
	buf := make([]byte, 4*1024)
	r, _, errno := syscall_syscall(libc_proc_pidpath_trampoline_addr, uintptr(pid), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n := int(int32(r)); n > 0 {
		return string(buf[:n]), nil
	}
	if errno == 0 {
		errno = syscall.ESRCH
	}
	return "", errno
}
