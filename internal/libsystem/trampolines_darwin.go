//go:build darwin

package libsystem

var libc_posix_spawn_trampoline_addr uintptr

//go:cgo_import_dynamic libc_posix_spawn posix_spawn "/usr/lib/libSystem.B.dylib"

var libc_posix_spawnattr_init_trampoline_addr uintptr

//go:cgo_import_dynamic libc_posix_spawnattr_init posix_spawnattr_init "/usr/lib/libSystem.B.dylib"

var libc_posix_spawnattr_destroy_trampoline_addr uintptr

//go:cgo_import_dynamic libc_posix_spawnattr_destroy posix_spawnattr_destroy "/usr/lib/libSystem.B.dylib"

var libc_posix_spawnattr_setflags_trampoline_addr uintptr

//go:cgo_import_dynamic libc_posix_spawnattr_setflags posix_spawnattr_setflags "/usr/lib/libSystem.B.dylib"

var libc_posix_spawnattr_setpgroup_trampoline_addr uintptr

//go:cgo_import_dynamic libc_posix_spawnattr_setpgroup posix_spawnattr_setpgroup "/usr/lib/libSystem.B.dylib"

var libc_posix_spawnattr_setsigdefault_trampoline_addr uintptr

//go:cgo_import_dynamic libc_posix_spawnattr_setsigdefault posix_spawnattr_setsigdefault "/usr/lib/libSystem.B.dylib"

var libc_posix_spawnattr_setsigmask_trampoline_addr uintptr

//go:cgo_import_dynamic libc_posix_spawnattr_setsigmask posix_spawnattr_setsigmask "/usr/lib/libSystem.B.dylib"

var libc_posix_spawn_file_actions_init_trampoline_addr uintptr

//go:cgo_import_dynamic libc_posix_spawn_file_actions_init posix_spawn_file_actions_init "/usr/lib/libSystem.B.dylib"

var libc_posix_spawn_file_actions_destroy_trampoline_addr uintptr

//go:cgo_import_dynamic libc_posix_spawn_file_actions_destroy posix_spawn_file_actions_destroy "/usr/lib/libSystem.B.dylib"

var libc_posix_spawn_file_actions_adddup2_trampoline_addr uintptr

//go:cgo_import_dynamic libc_posix_spawn_file_actions_adddup2 posix_spawn_file_actions_adddup2 "/usr/lib/libSystem.B.dylib"

var libc_posix_spawn_file_actions_addchdir_np_trampoline_addr uintptr

//go:cgo_import_dynamic libc_posix_spawn_file_actions_addchdir_np posix_spawn_file_actions_addchdir_np "/usr/lib/libSystem.B.dylib"

var libc_responsibility_spawnattrs_setdisclaim_trampoline_addr uintptr

//go:cgo_import_dynamic libc_responsibility_spawnattrs_setdisclaim responsibility_spawnattrs_setdisclaim "/usr/lib/libSystem.B.dylib"

var libc_responsibility_get_pid_responsible_for_pid_trampoline_addr uintptr

//go:cgo_import_dynamic libc_responsibility_get_pid_responsible_for_pid responsibility_get_pid_responsible_for_pid "/usr/lib/libSystem.B.dylib"

var libc_proc_pid_rusage_trampoline_addr uintptr

//go:cgo_import_dynamic libc_proc_pid_rusage proc_pid_rusage "/usr/lib/libSystem.B.dylib"

var libc_setiopolicy_np_trampoline_addr uintptr

//go:cgo_import_dynamic libc_setiopolicy_np setiopolicy_np "/usr/lib/libSystem.B.dylib"

var libc_getiopolicy_np_trampoline_addr uintptr

//go:cgo_import_dynamic libc_getiopolicy_np getiopolicy_np "/usr/lib/libSystem.B.dylib"

var libc_mach_timebase_info_trampoline_addr uintptr

//go:cgo_import_dynamic libc_mach_timebase_info mach_timebase_info "/usr/lib/libSystem.B.dylib"
var libc_shm_open_trampoline_addr uintptr

//go:cgo_import_dynamic libc_shm_open shm_open "/usr/lib/libSystem.B.dylib"

var libc_shm_unlink_trampoline_addr uintptr

//go:cgo_import_dynamic libc_shm_unlink shm_unlink "/usr/lib/libSystem.B.dylib"
