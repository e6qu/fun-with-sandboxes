// fwsroot gives every process of a fwsd container, which the engine loads
// it into through DYLD_INSERT_LIBRARIES, the container's view where an
// unprivileged macOS process can have none of its own: as fakechroot and
// fakeroot do on Linux, it presents, without enforcing,
//
//   - the container's root at /: a path the root holds, or whose parent it
//     holds, so a file can be made there, names the root's; any other, such
//     as the system frameworks and devices, stays the host's (SANDBOX_ROOT);
//   - the container's user and group (FWS_UID, FWS_GID), root by default:
//     chown succeeds and records the owner in an extended attribute, which
//     stat reports, and a file a user other than root makes is recorded as
//     theirs; files the container's processes made report root otherwise;
//   - the users and groups of the container's /etc/passwd and /etc/group;
//   - the container's hostname (FWS_HOSTNAME).
//
// The host still decides every access, by the engine's user's rights.
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <pwd.h>
#include <sys/utsname.h>
#include <limits.h>
#include <spawn.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/attr.h>
#include <sys/sysctl.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/xattr.h>
#include <unistd.h>

#define INTERPOSE(replacement, replacee)                                              \
    __attribute__((used)) static struct {                                            \
        const void *a;                                                               \
        const void *b;                                                               \
    } interpose_##replacement __attribute__((section("__DATA,__interpose"))) = {     \
        (const void *)(unsigned long)&replacement, (const void *)(unsigned long)&replacee}

static char root[PATH_MAX];
static size_t rootlen;

static uid_t fake_uid, fake_euid;
static gid_t fake_gid, fake_egid;
static char hostname[256];

static unsigned long env_number(const char *name) {
    const char *v = getenv(name);
    return v ? strtoul(v, NULL, 10) : 0;
}

__attribute__((constructor)) static void fwsroot_init(void) {
    fake_uid = fake_euid = (uid_t)env_number("FWS_UID");
    fake_gid = fake_egid = (gid_t)env_number("FWS_GID");
    const char *h = getenv("FWS_HOSTNAME");
    if (h) {
        strlcpy(hostname, h, sizeof hostname);
    }
    const char *r = getenv("SANDBOX_ROOT");
    if (r && r[0] == '/' && strlen(r) < sizeof root) {
        strcpy(root, r);
        rootlen = strlen(root);
        while (rootlen > 1 && root[rootlen - 1] == '/') {
            root[--rootlen] = 0;
        }
    }
}

// The host trees a container uses as they are: no image holds them, and
// the dynamic loader and libSystem read them on every start.
static const char *const host_trees[] = {"/System/", "/usr/lib/", "/dev/", "/private/var/db/", "/var/db/", "/Library/Apple/", NULL};

static int in_root(const char *p) {
    return strncmp(p, root, rootlen) == 0 && (p[rootlen] == '/' || p[rootlen] == 0);
}

// fws_map returns the path p names in the container: the root's when the
// root holds it or its parent, else p. buf holds a mapped path.
static const char *fws_map(const char *p, char *buf) {
    if (!rootlen || !p || p[0] != '/' || in_root(p)) {
        return p;
    }
    for (const char *const *t = host_trees; *t; t++) {
        if (strncmp(p, *t, strlen(*t)) == 0 || strncmp(p, *t, strlen(*t) - 1) == 0 && p[strlen(*t) - 1] == 0) {
            return p;
        }
    }
    size_t n = strlen(p);
    if (rootlen + n + 1 > PATH_MAX) {
        return p;
    }
    memcpy(buf, root, rootlen);
    memcpy(buf + rootlen, p, n + 1);
    struct stat st;
    if (lstat(buf, &st) == 0) {
        return buf;
    }
    char *slash = strrchr(buf + rootlen, '/');
    if (slash == buf + rootlen) {
        return buf;
    }
    *slash = 0;
    int parent = lstat(buf, &st) == 0;
    *slash = '/';
    return parent ? buf : p;
}

#define MAP(p) fws_map((p), (char[PATH_MAX]){0})

// unmap turns a host path in the root into the container's path, in place.
static void unmap(char *p) {
    if (!rootlen || !p || !in_root(p)) {
        return;
    }
    if (p[rootlen] == 0) {
        strcpy(p, "/");
    } else {
        memmove(p, p + rootlen, strlen(p + rootlen) + 1);
    }
}

static void made(const char *path, int fd, int nofollow);
static int fake_user(void);

// creating reports whether opening p with flags makes a file a user other
// than root must be recorded as owning.
static int creating(const char *p, int flags) {
    struct stat st;
    return fake_user() && (flags & O_CREAT) && lstat(p, &st) != 0;
}

static int fws_open(const char *path, int flags, ...) {
    mode_t mode = 0;
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode = (mode_t)va_arg(ap, int);
        va_end(ap);
    }
    const char *p = MAP(path);
    int fresh = creating(p, flags);
    int fd = open(p, flags, mode);
    if (fd >= 0 && fresh) made(NULL, fd, 0);
    return fd;
}
INTERPOSE(fws_open, open);

extern int open_nocancel(const char *, int, ...) __asm("_open$NOCANCEL");
static int fws_open_nocancel(const char *path, int flags, ...) {
    mode_t mode = 0;
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode = (mode_t)va_arg(ap, int);
        va_end(ap);
    }
    const char *p = MAP(path);
    int fresh = creating(p, flags);
    int fd = open_nocancel(p, flags, mode);
    if (fd >= 0 && fresh) made(NULL, fd, 0);
    return fd;
}
INTERPOSE(fws_open_nocancel, open_nocancel);

static int fws_openat(int fd, const char *path, int flags, ...) {
    mode_t mode = 0;
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode = (mode_t)va_arg(ap, int);
        va_end(ap);
    }
    const char *p = MAP(path);
    int fresh = (p[0] == '/' || fd == AT_FDCWD) && creating(p, flags);
    int r = openat(fd, p, flags, mode);
    if (r >= 0 && fresh) made(NULL, r, 0);
    return r;
}
INTERPOSE(fws_openat, openat);

static int fws_creat(const char *path, mode_t mode) {
    const char *p = MAP(path);
    int fresh = creating(p, O_CREAT);
    int fd = creat(p, mode);
    if (fd >= 0 && fresh) made(NULL, fd, 0);
    return fd;
}
INTERPOSE(fws_creat, creat);

static int fws_access(const char *path, int mode) { return access(MAP(path), mode); }
INTERPOSE(fws_access, access);
static int fws_faccessat(int fd, const char *path, int mode, int flag) { return faccessat(fd, MAP(path), mode, flag); }
INTERPOSE(fws_faccessat, faccessat);

static uid_t real_uid;
static uid_t getuid_real(void) {
    if (!real_uid) {
        real_uid = getuid();
    }
    return real_uid;
}
static void own(struct stat *st, const char *path, int fd, int nofollow);

static int fws_stat(const char *path, struct stat *st) {
    const char *p = MAP(path);
    int r = stat(p, st);
    if (r == 0) own(st, p, -1, 0);
    return r;
}
INTERPOSE(fws_stat, stat);
static int fws_lstat(const char *path, struct stat *st) {
    const char *p = MAP(path);
    int r = lstat(p, st);
    if (r == 0) own(st, p, -1, 1);
    return r;
}
INTERPOSE(fws_lstat, lstat);
static int fws_fstat(int fd, struct stat *st) {
    int r = fstat(fd, st);
    if (r == 0) own(st, NULL, fd, 0);
    return r;
}
INTERPOSE(fws_fstat, fstat);
static int fws_fstatat(int fd, const char *path, struct stat *st, int flag) {
    const char *p = MAP(path);
    int r = fstatat(fd, p, st, flag);
    if (r == 0) {
        if (p[0] == '/' || fd == AT_FDCWD) {
            own(st, p, -1, flag & AT_SYMLINK_NOFOLLOW);
        } else {
            int f = openat(fd, p, O_RDONLY | ((flag & AT_SYMLINK_NOFOLLOW) ? O_SYMLINK : 0));
            if (f >= 0) {
                own(st, NULL, f, 0);
                close(f);
            }
        }
    }
    return r;
}
INTERPOSE(fws_fstatat, fstatat);

// The 64-bit inode names Go and older programs call.
extern int stat64(const char *, void *);
extern int lstat64(const char *, void *);
extern int fstatat64(int, const char *, void *, int);
// Their struct stat64 is the struct stat this library builds with.
extern int fstat64(int, void *);
static int fws_stat64(const char *path, void *st) {
    const char *p = MAP(path);
    int r = stat64(p, st);
    if (r == 0) own(st, p, -1, 0);
    return r;
}
INTERPOSE(fws_stat64, stat64);
static int fws_lstat64(const char *path, void *st) {
    const char *p = MAP(path);
    int r = lstat64(p, st);
    if (r == 0) own(st, p, -1, 1);
    return r;
}
INTERPOSE(fws_lstat64, lstat64);
static int fws_fstat64(int fd, void *st) {
    int r = fstat64(fd, st);
    if (r == 0) own(st, NULL, fd, 0);
    return r;
}
INTERPOSE(fws_fstat64, fstat64);
static int fws_fstatat64(int fd, const char *path, void *st, int flag) {
    const char *p = MAP(path);
    int r = fstatat64(fd, p, st, flag);
    if (r == 0 && (p[0] == '/' || fd == AT_FDCWD)) own(st, p, -1, flag & AT_SYMLINK_NOFOLLOW);
    return r;
}
INTERPOSE(fws_fstatat64, fstatat64);

static int fws_statfs(const char *path, struct statfs *st) { return statfs(MAP(path), st); }
INTERPOSE(fws_statfs, statfs);
static int fws_getattrlist(const char *path, void *attrs, void *buf, size_t size, unsigned int opts) {
    return getattrlist(MAP(path), attrs, buf, size, opts);
}
INTERPOSE(fws_getattrlist, getattrlist);
static int fws_setattrlist(const char *path, void *attrs, void *buf, size_t size, unsigned int opts) {
    return setattrlist(MAP(path), attrs, buf, size, opts);
}
INTERPOSE(fws_setattrlist, setattrlist);
static long fws_pathconf(const char *path, int name) { return pathconf(MAP(path), name); }
INTERPOSE(fws_pathconf, pathconf);

static int fws_chdir(const char *path) { return chdir(MAP(path)); }
INTERPOSE(fws_chdir, chdir);
static int fws_mkdir(const char *path, mode_t mode) {
    const char *p = MAP(path);
    int r = mkdir(p, mode);
    if (r == 0 && fake_user()) made(p, -1, 0);
    return r;
}
INTERPOSE(fws_mkdir, mkdir);
static int fws_mkdirat(int fd, const char *path, mode_t mode) { return mkdirat(fd, MAP(path), mode); }
INTERPOSE(fws_mkdirat, mkdirat);
static int fws_rmdir(const char *path) { return rmdir(MAP(path)); }
INTERPOSE(fws_rmdir, rmdir);
static int fws_unlink(const char *path) { return unlink(MAP(path)); }
INTERPOSE(fws_unlink, unlink);
static int fws_unlinkat(int fd, const char *path, int flag) { return unlinkat(fd, MAP(path), flag); }
INTERPOSE(fws_unlinkat, unlinkat);
static int fws_rename(const char *from, const char *to) { return rename(MAP(from), MAP(to)); }
INTERPOSE(fws_rename, rename);
static int fws_renameat(int fd1, const char *from, int fd2, const char *to) { return renameat(fd1, MAP(from), fd2, MAP(to)); }
INTERPOSE(fws_renameat, renameat);
static int fws_renamex_np(const char *from, const char *to, unsigned int flags) { return renamex_np(MAP(from), MAP(to), flags); }
INTERPOSE(fws_renamex_np, renamex_np);
static int fws_renameatx_np(int fd1, const char *from, int fd2, const char *to, unsigned int flags) {
    return renameatx_np(fd1, MAP(from), fd2, MAP(to), flags);
}
INTERPOSE(fws_renameatx_np, renameatx_np);
static int fws_link(const char *from, const char *to) { return link(MAP(from), MAP(to)); }
INTERPOSE(fws_link, link);
static int fws_linkat(int fd1, const char *from, int fd2, const char *to, int flag) { return linkat(fd1, MAP(from), fd2, MAP(to), flag); }
INTERPOSE(fws_linkat, linkat);
// A link's target is the link's to keep; only where the link goes maps.
static int fws_symlink(const char *target, const char *path) {
    const char *p = MAP(path);
    int r = symlink(target, p);
    if (r == 0 && fake_user()) made(p, -1, 1);
    return r;
}
INTERPOSE(fws_symlink, symlink);
static int fws_symlinkat(const char *target, int fd, const char *path) { return symlinkat(target, fd, MAP(path)); }
INTERPOSE(fws_symlinkat, symlinkat);
static ssize_t fws_readlink(const char *path, char *buf, size_t size) { return readlink(MAP(path), buf, size); }
INTERPOSE(fws_readlink, readlink);
static ssize_t fws_readlinkat(int fd, const char *path, char *buf, size_t size) { return readlinkat(fd, MAP(path), buf, size); }
INTERPOSE(fws_readlinkat, readlinkat);
static int fws_chmod(const char *path, mode_t mode) { return chmod(MAP(path), mode); }
INTERPOSE(fws_chmod, chmod);
static int fws_fchmodat(int fd, const char *path, mode_t mode, int flag) { return fchmodat(fd, MAP(path), mode, flag); }
INTERPOSE(fws_fchmodat, fchmodat);
static int fws_utimes(const char *path, const struct timeval t[2]) { return utimes(MAP(path), t); }
INTERPOSE(fws_utimes, utimes);
static int fws_lutimes(const char *path, const struct timeval t[2]) { return lutimes(MAP(path), t); }
INTERPOSE(fws_lutimes, lutimes);
static int fws_utimensat(int fd, const char *path, const struct timespec t[2], int flag) { return utimensat(fd, MAP(path), t, flag); }
INTERPOSE(fws_utimensat, utimensat);
static int fws_truncate(const char *path, off_t len) { return truncate(MAP(path), len); }
INTERPOSE(fws_truncate, truncate);
static int fws_mkfifo(const char *path, mode_t mode) {
    const char *p = MAP(path);
    int r = mkfifo(p, mode);
    if (r == 0 && fake_user()) made(p, -1, 0);
    return r;
}
INTERPOSE(fws_mkfifo, mkfifo);
static int fws_chflags(const char *path, unsigned int flags) { return chflags(MAP(path), flags); }
INTERPOSE(fws_chflags, chflags);
static ssize_t fws_getxattr(const char *path, const char *name, void *v, size_t n, uint32_t pos, int o) { return getxattr(MAP(path), name, v, n, pos, o); }
INTERPOSE(fws_getxattr, getxattr);
static int fws_setxattr(const char *path, const char *name, const void *v, size_t n, uint32_t pos, int o) { return setxattr(MAP(path), name, v, n, pos, o); }
INTERPOSE(fws_setxattr, setxattr);
static ssize_t fws_listxattr(const char *path, char *buf, size_t n, int o) { return listxattr(MAP(path), buf, n, o); }
INTERPOSE(fws_listxattr, listxattr);
static int fws_removexattr(const char *path, const char *name, int o) { return removexattr(MAP(path), name, o); }
INTERPOSE(fws_removexattr, removexattr);

// The kernel resolves a script's interpreter on the host. A script whose
// interpreter the root holds runs with the root's, as in a chroot: the
// interpreter, its #! argument, the script, then the script's arguments.
// script_argv returns the arguments to run interp with, allocated, or NULL
// when path runs as it is.
static char **script_argv(const char *path, char *const argv[], char *interp) {
    int fd = open(path, O_RDONLY);
    if (fd < 0) {
        return NULL;
    }
    char head[512];
    ssize_t n = read(fd, head, sizeof head - 1);
    close(fd);
    if (n < 3 || head[0] != '#' || head[1] != '!') {
        return NULL;
    }
    head[n] = 0;
    char *line = head + 2;
    line[strcspn(line, "\n")] = 0;
    while (*line == ' ' || *line == '\t') line++;
    char *arg = line + strcspn(line, " \t");
    if (*arg) {
        *arg++ = 0;
        while (*arg == ' ' || *arg == '\t') arg++;
        arg[strcspn(arg, "\r")] = 0;
    }
    if (line[0] != '/') {
        return NULL;
    }
    const char *mapped = fws_map(line, interp);
    if (mapped != interp) {
        // The host's interpreter: the kernel runs it as the #! line says.
        return NULL;
    }
    size_t argc = 0;
    while (argv && argv[argc]) argc++;
    char **out = calloc(argc + 4, sizeof *out);
    if (!out) {
        return NULL;
    }
    size_t i = 0;
    out[i++] = strdup(line);
    if (*arg) out[i++] = strdup(arg);
    out[i++] = (char *)path;
    for (size_t j = 1; j < argc; j++) out[i++] = argv[j];
    return out;
}

static int fws_execve(const char *path, char *const argv[], char *const envp[]) {
    const char *p = MAP(path);
    char interp[PATH_MAX];
    char **sargv = script_argv(p, argv, interp);
    if (sargv) {
        int r = execve(interp, sargv, envp);
        free(sargv);
        return r;
    }
    return execve(p, argv, envp);
}
INTERPOSE(fws_execve, execve);
static int fws_posix_spawn(pid_t *pid, const char *path, const posix_spawn_file_actions_t *fa, const posix_spawnattr_t *a,
                           char *const argv[], char *const envp[]) {
    const char *p = MAP(path);
    char interp[PATH_MAX];
    char **sargv = script_argv(p, argv, interp);
    if (sargv) {
        int r = posix_spawn(pid, interp, fa, a, sargv, envp);
        free(sargv);
        return r;
    }
    return posix_spawn(pid, p, fa, a, argv, envp);
}
INTERPOSE(fws_posix_spawn, posix_spawn);
// posix_spawnp searches PATH, whose directories name the host's paths of
// the container's, for a file without a slash; the search finds the script
// before the kernel reads its #! line.
static int fws_posix_spawnp(pid_t *pid, const char *file, const posix_spawn_file_actions_t *fa, const posix_spawnattr_t *a,
                            char *const argv[], char *const envp[]) {
    if (strchr(file, '/')) {
        return fws_posix_spawn(pid, file, fa, a, argv, envp);
    }
    const char *path = getenv("PATH");
    char dir[PATH_MAX], full[PATH_MAX];
    while (path && *path) {
        size_t n = strcspn(path, ":");
        if (n > 0 && n < sizeof dir) {
            memcpy(dir, path, n);
            dir[n] = 0;
            if (snprintf(full, sizeof full, "%s/%s", dir, file) < (int)sizeof full && access(fws_map(full, (char[PATH_MAX]){0}), X_OK) == 0) {
                return fws_posix_spawn(pid, full, fa, a, argv, envp);
            }
        }
        path += n;
        if (*path == ':') path++;
    }
    return posix_spawnp(pid, file, fa, a, argv, envp);
}
INTERPOSE(fws_posix_spawnp, posix_spawnp);

static void *fws_dlopen(const char *path, int mode) { return dlopen(MAP(path), mode); }
INTERPOSE(fws_dlopen, dlopen);

static char *fws_getcwd(char *buf, size_t size) {
    char *r = getcwd(buf, size);
    unmap(r);
    return r;
}
INTERPOSE(fws_getcwd, getcwd);

// Identity.

static void export_ids(void) {
    char v[16];
    snprintf(v, sizeof v, "%u", fake_euid);
    setenv("FWS_UID", v, 1);
    snprintf(v, sizeof v, "%u", fake_egid);
    setenv("FWS_GID", v, 1);
}

static uid_t fws_getuid(void) { return fake_uid; }
INTERPOSE(fws_getuid, getuid);
static uid_t fws_geteuid(void) { return fake_euid; }
INTERPOSE(fws_geteuid, geteuid);
static gid_t fws_getgid(void) { return fake_gid; }
INTERPOSE(fws_getgid, getgid);
static gid_t fws_getegid(void) { return fake_egid; }
INTERPOSE(fws_getegid, getegid);
static int fws_getgroups(int n, gid_t *list) {
    if (n == 0) {
        return 1;
    }
    if (n < 1) {
        errno = EINVAL;
        return -1;
    }
    list[0] = fake_egid;
    return 1;
}
INTERPOSE(fws_getgroups, getgroups);

// Only root changes identity, as the kernel would allow; the new identity
// passes to the programs the process executes.
static int may_switch(uid_t to) { return fake_euid == 0 || to == fake_uid || to == fake_euid; }
static int fws_setuid(uid_t u) {
    if (!may_switch(u)) {
        errno = EPERM;
        return -1;
    }
    fake_uid = fake_euid = u;
    export_ids();
    return 0;
}
INTERPOSE(fws_setuid, setuid);
static int fws_seteuid(uid_t u) {
    if (!may_switch(u)) {
        errno = EPERM;
        return -1;
    }
    fake_euid = u;
    export_ids();
    return 0;
}
INTERPOSE(fws_seteuid, seteuid);
static int fws_setreuid(uid_t r, uid_t e) {
    if ((r != (uid_t)-1 && !may_switch(r)) || (e != (uid_t)-1 && !may_switch(e))) {
        errno = EPERM;
        return -1;
    }
    if (r != (uid_t)-1) fake_uid = r;
    if (e != (uid_t)-1) fake_euid = e;
    export_ids();
    return 0;
}
INTERPOSE(fws_setreuid, setreuid);
static int fws_setgid(gid_t g) {
    if (fake_euid != 0 && g != fake_gid && g != fake_egid) {
        errno = EPERM;
        return -1;
    }
    fake_gid = fake_egid = g;
    export_ids();
    return 0;
}
INTERPOSE(fws_setgid, setgid);
static int fws_setegid(gid_t g) {
    if (fake_euid != 0 && g != fake_gid && g != fake_egid) {
        errno = EPERM;
        return -1;
    }
    fake_egid = g;
    export_ids();
    return 0;
}
INTERPOSE(fws_setegid, setegid);
static int fws_setregid(gid_t r, gid_t e) {
    if (fake_euid != 0) {
        errno = EPERM;
        return -1;
    }
    if (r != (gid_t)-1) fake_gid = r;
    if (e != (gid_t)-1) fake_egid = e;
    export_ids();
    return 0;
}
INTERPOSE(fws_setregid, setregid);
static int fws_setgroups(int n, const gid_t *list) {
    (void)n;
    (void)list;
    if (fake_euid != 0) {
        errno = EPERM;
        return -1;
    }
    return 0;
}
INTERPOSE(fws_setgroups, setgroups);
static int fws_initgroups(const char *name, int gid) {
    (void)name;
    (void)gid;
    if (fake_euid != 0) {
        errno = EPERM;
        return -1;
    }
    return 0;
}
INTERPOSE(fws_initgroups, initgroups);

// Ownership, kept in an extended attribute: chown records it, stat reports
// it, and a file the container's processes made reports root until then.

#define OWNER_XATTR "com.fws.owner"

static int read_owner(const char *path, int fd, int nofollow, uid_t *uid, gid_t *gid) {
    char v[32];
    ssize_t n = fd >= 0 ? fgetxattr(fd, OWNER_XATTR, v, sizeof v - 1, 0, 0)
                        : getxattr(path, OWNER_XATTR, v, sizeof v - 1, 0, nofollow ? XATTR_NOFOLLOW : 0);
    if (n <= 0) {
        return 0;
    }
    v[n] = 0;
    unsigned long u, g;
    if (sscanf(v, "%lu:%lu", &u, &g) != 2) {
        return 0;
    }
    *uid = (uid_t)u;
    *gid = (gid_t)g;
    return 1;
}

static void write_owner(const char *path, int fd, int nofollow, uid_t uid, gid_t gid) {
    char v[32];
    int n = snprintf(v, sizeof v, "%u:%u", uid, gid);
    if (fd >= 0) {
        fsetxattr(fd, OWNER_XATTR, v, (size_t)n, 0, 0);
    } else {
        setxattr(path, OWNER_XATTR, v, (size_t)n, 0, nofollow ? XATTR_NOFOLLOW : 0);
    }
}

// own presents a stat result as the container sees it.
static void own(struct stat *st, const char *path, int fd, int nofollow) {
    uid_t u;
    gid_t g;
    if (st->st_uid != getuid_real()) {
        return;
    }
    if (read_owner(path, fd, nofollow, &u, &g)) {
        st->st_uid = u;
        st->st_gid = g;
    } else {
        st->st_uid = 0;
        st->st_gid = 0;
    }
}

static int fake_user(void) { return fake_euid != 0 || fake_egid != 0; }

// made records a file a process of a user other than root made as theirs.
static void made(const char *path, int fd, int nofollow) { write_owner(path, fd, nofollow, fake_euid, fake_egid); }

static int fws_chown(const char *path, uid_t uid, gid_t gid) {
    const char *p = MAP(path);
    struct stat st;
    if (stat(p, &st) != 0) {
        return -1;
    }
    uid_t cu;
    gid_t cg;
    if (!read_owner(p, -1, 0, &cu, &cg)) {
        cu = 0;
        cg = 0;
    }
    if (fake_euid != 0 && (uid != (uid_t)-1 || cu != fake_euid)) {
        errno = EPERM;
        return -1;
    }
    write_owner(p, -1, 0, uid == (uid_t)-1 ? cu : uid, gid == (gid_t)-1 ? cg : gid);
    return 0;
}
INTERPOSE(fws_chown, chown);
static int fws_lchown(const char *path, uid_t uid, gid_t gid) {
    const char *p = MAP(path);
    struct stat st;
    if (lstat(p, &st) != 0) {
        return -1;
    }
    uid_t cu;
    gid_t cg;
    if (!read_owner(p, -1, 1, &cu, &cg)) {
        cu = 0;
        cg = 0;
    }
    if (fake_euid != 0 && (uid != (uid_t)-1 || cu != fake_euid)) {
        errno = EPERM;
        return -1;
    }
    write_owner(p, -1, 1, uid == (uid_t)-1 ? cu : uid, gid == (gid_t)-1 ? cg : gid);
    return 0;
}
INTERPOSE(fws_lchown, lchown);
static int fws_fchown(int fd, uid_t uid, gid_t gid) {
    uid_t cu;
    gid_t cg;
    if (!read_owner(NULL, fd, 0, &cu, &cg)) {
        cu = 0;
        cg = 0;
    }
    if (fake_euid != 0 && (uid != (uid_t)-1 || cu != fake_euid)) {
        errno = EPERM;
        return -1;
    }
    write_owner(NULL, fd, 0, uid == (uid_t)-1 ? cu : uid, gid == (gid_t)-1 ? cg : gid);
    return 0;
}
INTERPOSE(fws_fchown, fchown);
static int fws_fchownat(int fd, const char *path, uid_t uid, gid_t gid, int flag) {
    if (path[0] == '/' || fd == AT_FDCWD) {
        return (flag & AT_SYMLINK_NOFOLLOW) ? fws_lchown(path, uid, gid) : fws_chown(path, uid, gid);
    }
    int f = openat(fd, path, O_RDONLY | ((flag & AT_SYMLINK_NOFOLLOW) ? O_SYMLINK : 0));
    if (f < 0) {
        return -1;
    }
    int r = fws_fchown(f, uid, gid);
    close(f);
    return r;
}
INTERPOSE(fws_fchownat, fchownat);

// Users and groups, from the container's /etc/passwd and /etc/group.

static struct passwd pw;
static char pwbuf[1024];
static struct group gr;
static char grbuf[1024];
static char *grmem[2];

static struct passwd *find_passwd(const char *name, uid_t uid, int byname) {
    char path[PATH_MAX];
    snprintf(path, sizeof path, "%s/etc/passwd", root);
    FILE *f = rootlen ? fopen(path, "r") : NULL;
    if (!f) {
        // An image without /etc/passwd knows root alone.
        if ((byname && strcmp(name, "root") == 0) || (!byname && uid == 0)) {
            strcpy(pwbuf, "root");
            pw = (struct passwd){.pw_name = pwbuf, .pw_passwd = "*", .pw_uid = 0, .pw_gid = 0,
                                 .pw_gecos = "root", .pw_dir = "/root", .pw_shell = "/bin/sh"};
            return &pw;
        }
        return NULL;
    }
    while (fgets(pwbuf, sizeof pwbuf, f)) {
        char *fields[7];
        char *s = pwbuf;
        int n = 0;
        pwbuf[strcspn(pwbuf, "\n")] = 0;
        while (n < 7 && (fields[n] = strsep(&s, ":")) != NULL) {
            n++;
        }
        if (n < 7 || fields[0][0] == '#') {
            continue;
        }
        uid_t u = (uid_t)strtoul(fields[2], NULL, 10);
        if ((byname && strcmp(fields[0], name) == 0) || (!byname && u == uid)) {
            fclose(f);
            pw = (struct passwd){.pw_name = fields[0], .pw_passwd = fields[1], .pw_uid = u,
                                 .pw_gid = (gid_t)strtoul(fields[3], NULL, 10), .pw_gecos = fields[4],
                                 .pw_dir = fields[5], .pw_shell = fields[6]};
            return &pw;
        }
    }
    fclose(f);
    return NULL;
}

static struct group *find_group(const char *name, gid_t gid, int byname) {
    char path[PATH_MAX];
    snprintf(path, sizeof path, "%s/etc/group", root);
    FILE *f = rootlen ? fopen(path, "r") : NULL;
    if (!f) {
        if ((byname && strcmp(name, "root") == 0) || (!byname && gid == 0)) {
            strcpy(grbuf, "root");
            grmem[0] = NULL;
            gr = (struct group){.gr_name = grbuf, .gr_passwd = "*", .gr_gid = 0, .gr_mem = grmem};
            return &gr;
        }
        return NULL;
    }
    while (fgets(grbuf, sizeof grbuf, f)) {
        char *fields[4];
        char *s = grbuf;
        int n = 0;
        grbuf[strcspn(grbuf, "\n")] = 0;
        while (n < 4 && (fields[n] = strsep(&s, ":")) != NULL) {
            n++;
        }
        if (n < 3 || fields[0][0] == '#') {
            continue;
        }
        gid_t g = (gid_t)strtoul(fields[2], NULL, 10);
        if ((byname && strcmp(fields[0], name) == 0) || (!byname && g == gid)) {
            fclose(f);
            grmem[0] = NULL;
            gr = (struct group){.gr_name = fields[0], .gr_passwd = fields[1], .gr_gid = g, .gr_mem = grmem};
            return &gr;
        }
    }
    fclose(f);
    return NULL;
}

static struct passwd *fws_getpwuid(uid_t uid) { return find_passwd(NULL, uid, 0); }
INTERPOSE(fws_getpwuid, getpwuid);
static struct passwd *fws_getpwnam(const char *name) { return find_passwd(name, 0, 1); }
INTERPOSE(fws_getpwnam, getpwnam);
static struct group *fws_getgrgid(gid_t gid) { return find_group(NULL, gid, 0); }
INTERPOSE(fws_getgrgid, getgrgid);
static struct group *fws_getgrnam(const char *name) { return find_group(name, 0, 1); }
INTERPOSE(fws_getgrnam, getgrnam);

static int copy_passwd(struct passwd *found, struct passwd *out, char *buf, size_t size, struct passwd **result) {
    *result = NULL;
    if (!found) {
        return 0;
    }
    const char *src[] = {found->pw_name, found->pw_passwd, found->pw_gecos, found->pw_dir, found->pw_shell};
    char **dst[] = {&out->pw_name, &out->pw_passwd, &out->pw_gecos, &out->pw_dir, &out->pw_shell};
    size_t used = 0;
    *out = *found;
    for (int i = 0; i < 5; i++) {
        size_t n = strlen(src[i]) + 1;
        if (used + n > size) {
            return ERANGE;
        }
        memcpy(buf + used, src[i], n);
        *dst[i] = buf + used;
        used += n;
    }
    *result = out;
    return 0;
}
static int fws_getpwuid_r(uid_t uid, struct passwd *out, char *buf, size_t size, struct passwd **result) {
    return copy_passwd(find_passwd(NULL, uid, 0), out, buf, size, result);
}
INTERPOSE(fws_getpwuid_r, getpwuid_r);
static int fws_getpwnam_r(const char *name, struct passwd *out, char *buf, size_t size, struct passwd **result) {
    return copy_passwd(find_passwd(name, 0, 1), out, buf, size, result);
}
INTERPOSE(fws_getpwnam_r, getpwnam_r);

static int copy_group(struct group *found, struct group *out, char *buf, size_t size, struct group **result) {
    *result = NULL;
    if (!found) {
        return 0;
    }
    size_t a = strlen(found->gr_name) + 1, b = strlen(found->gr_passwd) + 1;
    size_t align = sizeof(char *);
    size_t off = (a + b + align - 1) / align * align;
    if (off + sizeof(char *) > size) {
        return ERANGE;
    }
    *out = *found;
    memcpy(buf, found->gr_name, a);
    memcpy(buf + a, found->gr_passwd, b);
    out->gr_name = buf;
    out->gr_passwd = buf + a;
    out->gr_mem = (char **)(void *)(buf + off);
    out->gr_mem[0] = NULL;
    *result = out;
    return 0;
}
static int fws_getgrgid_r(gid_t gid, struct group *out, char *buf, size_t size, struct group **result) {
    return copy_group(find_group(NULL, gid, 0), out, buf, size, result);
}
INTERPOSE(fws_getgrgid_r, getgrgid_r);
static int fws_getgrnam_r(const char *name, struct group *out, char *buf, size_t size, struct group **result) {
    return copy_group(find_group(name, 0, 1), out, buf, size, result);
}
INTERPOSE(fws_getgrnam_r, getgrnam_r);

// Hostname.

static int fws_gethostname(char *name, size_t len) {
    if (!hostname[0]) {
        return gethostname(name, len);
    }
    if (strlen(hostname) + 1 > len) {
        errno = ENAMETOOLONG;
        return -1;
    }
    strcpy(name, hostname);
    return 0;
}
INTERPOSE(fws_gethostname, gethostname);
static int fws_uname(struct utsname *u) {
    int r = uname(u);
    if (r == 0 && hostname[0]) {
        strlcpy(u->nodename, hostname, sizeof u->nodename);
    }
    return r;
}
INTERPOSE(fws_uname, uname);

// Go and other programs read the hostname through sysctl.
static int fws_sysctl(int *name, u_int namelen, void *old, size_t *oldlen, void *new, size_t newlen) {
    if (hostname[0] && namelen == 2 && name[0] == CTL_KERN && name[1] == KERN_HOSTNAME && new == NULL) {
        size_t n = strlen(hostname) + 1;
        if (old == NULL) {
            *oldlen = n;
            return 0;
        }
        if (*oldlen < n) {
            errno = ENOMEM;
            return -1;
        }
        memcpy(old, hostname, n);
        *oldlen = n;
        return 0;
    }
    return sysctl(name, namelen, old, oldlen, new, newlen);
}
INTERPOSE(fws_sysctl, sysctl);
static int fws_sysctlbyname(const char *name, void *old, size_t *oldlen, void *new, size_t newlen) {
    if (hostname[0] && strcmp(name, "kern.hostname") == 0 && new == NULL) {
        int mib[2] = {CTL_KERN, KERN_HOSTNAME};
        return fws_sysctl(mib, 2, old, oldlen, NULL, 0);
    }
    return sysctlbyname(name, old, oldlen, new, newlen);
}
INTERPOSE(fws_sysctlbyname, sysctlbyname);
