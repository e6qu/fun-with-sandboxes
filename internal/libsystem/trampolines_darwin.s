// Trampolines into libSystem, as golang.org/x/sys/unix calls it without cgo.

//go:build darwin && (amd64 || arm64)

#include "textflag.h"

TEXT libc_posix_spawn_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn(SB)
GLOBL	·libc_posix_spawn_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawn_trampoline_addr(SB)/8, $libc_posix_spawn_trampoline<>(SB)

TEXT libc_posix_spawnattr_init_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_init(SB)
GLOBL	·libc_posix_spawnattr_init_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_init_trampoline_addr(SB)/8, $libc_posix_spawnattr_init_trampoline<>(SB)

TEXT libc_posix_spawnattr_destroy_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_destroy(SB)
GLOBL	·libc_posix_spawnattr_destroy_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_destroy_trampoline_addr(SB)/8, $libc_posix_spawnattr_destroy_trampoline<>(SB)

TEXT libc_posix_spawnattr_setflags_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setflags(SB)
GLOBL	·libc_posix_spawnattr_setflags_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_setflags_trampoline_addr(SB)/8, $libc_posix_spawnattr_setflags_trampoline<>(SB)

TEXT libc_posix_spawnattr_setpgroup_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setpgroup(SB)
GLOBL	·libc_posix_spawnattr_setpgroup_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_setpgroup_trampoline_addr(SB)/8, $libc_posix_spawnattr_setpgroup_trampoline<>(SB)

TEXT libc_posix_spawnattr_setsigdefault_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setsigdefault(SB)
GLOBL	·libc_posix_spawnattr_setsigdefault_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_setsigdefault_trampoline_addr(SB)/8, $libc_posix_spawnattr_setsigdefault_trampoline<>(SB)

TEXT libc_posix_spawnattr_setsigmask_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setsigmask(SB)
GLOBL	·libc_posix_spawnattr_setsigmask_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_setsigmask_trampoline_addr(SB)/8, $libc_posix_spawnattr_setsigmask_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_init_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_init(SB)
GLOBL	·libc_posix_spawn_file_actions_init_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawn_file_actions_init_trampoline_addr(SB)/8, $libc_posix_spawn_file_actions_init_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_destroy_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_destroy(SB)
GLOBL	·libc_posix_spawn_file_actions_destroy_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawn_file_actions_destroy_trampoline_addr(SB)/8, $libc_posix_spawn_file_actions_destroy_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_adddup2_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_adddup2(SB)
GLOBL	·libc_posix_spawn_file_actions_adddup2_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawn_file_actions_adddup2_trampoline_addr(SB)/8, $libc_posix_spawn_file_actions_adddup2_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_addchdir_np_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_addchdir_np(SB)
GLOBL	·libc_posix_spawn_file_actions_addchdir_np_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawn_file_actions_addchdir_np_trampoline_addr(SB)/8, $libc_posix_spawn_file_actions_addchdir_np_trampoline<>(SB)

TEXT libc_responsibility_spawnattrs_setdisclaim_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_responsibility_spawnattrs_setdisclaim(SB)
GLOBL	·libc_responsibility_spawnattrs_setdisclaim_trampoline_addr(SB), RODATA, $8
DATA	·libc_responsibility_spawnattrs_setdisclaim_trampoline_addr(SB)/8, $libc_responsibility_spawnattrs_setdisclaim_trampoline<>(SB)

TEXT libc_responsibility_get_pid_responsible_for_pid_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_responsibility_get_pid_responsible_for_pid(SB)
GLOBL	·libc_responsibility_get_pid_responsible_for_pid_trampoline_addr(SB), RODATA, $8
DATA	·libc_responsibility_get_pid_responsible_for_pid_trampoline_addr(SB)/8, $libc_responsibility_get_pid_responsible_for_pid_trampoline<>(SB)

TEXT libc_proc_pid_rusage_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_proc_pid_rusage(SB)
GLOBL	·libc_proc_pid_rusage_trampoline_addr(SB), RODATA, $8
DATA	·libc_proc_pid_rusage_trampoline_addr(SB)/8, $libc_proc_pid_rusage_trampoline<>(SB)

TEXT libc_setiopolicy_np_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_setiopolicy_np(SB)
GLOBL	·libc_setiopolicy_np_trampoline_addr(SB), RODATA, $8
DATA	·libc_setiopolicy_np_trampoline_addr(SB)/8, $libc_setiopolicy_np_trampoline<>(SB)

TEXT libc_getiopolicy_np_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_getiopolicy_np(SB)
GLOBL	·libc_getiopolicy_np_trampoline_addr(SB), RODATA, $8
DATA	·libc_getiopolicy_np_trampoline_addr(SB)/8, $libc_getiopolicy_np_trampoline<>(SB)

TEXT libc_mach_timebase_info_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_timebase_info(SB)
GLOBL	·libc_mach_timebase_info_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_timebase_info_trampoline_addr(SB)/8, $libc_mach_timebase_info_trampoline<>(SB)
TEXT libc_shm_open_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_shm_open(SB)
GLOBL	·libc_shm_open_trampoline_addr(SB), RODATA, $8
DATA	·libc_shm_open_trampoline_addr(SB)/8, $libc_shm_open_trampoline<>(SB)

TEXT libc_shm_unlink_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_shm_unlink(SB)
GLOBL	·libc_shm_unlink_trampoline_addr(SB), RODATA, $8
DATA	·libc_shm_unlink_trampoline_addr(SB)/8, $libc_shm_unlink_trampoline<>(SB)


TEXT libsandbox_sandbox_init_with_parameters_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libsandbox_sandbox_init_with_parameters(SB)
GLOBL	·libsandbox_sandbox_init_with_parameters_trampoline_addr(SB), RODATA, $8
DATA	·libsandbox_sandbox_init_with_parameters_trampoline_addr(SB)/8, $libsandbox_sandbox_init_with_parameters_trampoline<>(SB)

TEXT libsandbox_sandbox_free_error_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libsandbox_sandbox_free_error(SB)
GLOBL	·libsandbox_sandbox_free_error_trampoline_addr(SB), RODATA, $8
DATA	·libsandbox_sandbox_free_error_trampoline_addr(SB)/8, $libsandbox_sandbox_free_error_trampoline<>(SB)
