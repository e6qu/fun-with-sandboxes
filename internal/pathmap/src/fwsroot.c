// fwsroot gives every process of a fwsd container, which the engine loads
// it into through DYLD_INSERT_LIBRARIES, the container's view where an
// unprivileged macOS process can have none of its own: as fakechroot and
// fakeroot do on Linux, it presents, without enforcing,
//
//   - the container's root at /: a path the root holds, or whose parent it
//     holds, so a file can be made there, names the root's; any other, such
//     as the system frameworks and devices, stays the host's (SANDBOX_ROOT);
//     symbolic links resolve in the container, as under chroot;
//   - the container's user and group (FWS_UID, FWS_GID), root by default:
//     chown succeeds and records the owner in an extended attribute, which
//     stat reports, and a file a user other than root makes is recorded as
//     theirs; files the container's processes made report root otherwise;
//   - the users and groups of the container's /etc/passwd and /etc/group;
//   - the container's hostname (FWS_HOSTNAME);
//   - its volumes and bind mounts at their destinations (FWS_MOUNTS: a
//     destination, a tab and the resolved source, a line each): the engine
//     places a mount as a symlink in the root, which a path through the
//     mount point follows, and which presents the source's own path;
//   - its private IPC namespace (SANDBOX_IPC_PREFIX): POSIX IPC and Mach
//     bootstrap names carry the container's prefix.
//
// The host still decides every access, by the engine's user's rights.
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <pthread.h>
#include <pwd.h>
#include <sys/utsname.h>
#include <limits.h>
#include <mach/mach.h>
#include <semaphore.h>
#include <servers/bootstrap.h>
#include <spawn.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/attr.h>
#include <sys/clonefile.h>
#include <sys/mman.h>
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
// root, resolved.
static char croot[PATH_MAX];
static size_t rootlen;

static uid_t fake_uid, fake_euid;
static gid_t fake_gid, fake_egid;
static char hostname[256];

struct mount {
    char *dest, *src;
    size_t destlen, srclen;
};
static struct mount *mounts;
static size_t nmounts;

static char *trim_slashes(char *p, size_t *len) {
    *len = strlen(p);
    while (*len > 1 && p[*len - 1] == '/') p[--*len] = 0;
    return p;
}

// parse_mounts reads FWS_MOUNTS, longest destination first, so a mount
// inside another mount's destination wins.
static void parse_mounts(const char *v) {
    if (!v || !*v) return;
    char *all = strdup(v);
    if (!all) return;
    size_t cap = 1;
    for (const char *c = all; *c; c++) cap += *c == '\n';
    mounts = calloc(cap, sizeof *mounts);
    if (!mounts) return;
    for (char *line = strtok(all, "\n"); line; line = strtok(NULL, "\n")) {
        char *tab = strchr(line, '\t');
        if (!tab || line[0] != '/' || tab[1] != '/') continue;
        *tab = 0;
        struct mount m;
        m.dest = trim_slashes(line, &m.destlen);
        m.src = trim_slashes(tab + 1, &m.srclen);
        size_t i = nmounts++;
        while (i > 0 && mounts[i - 1].destlen < m.destlen) {
            mounts[i] = mounts[i - 1];
            i--;
        }
        mounts[i] = m;
    }
}

static int under(const char *p, const char *dir, size_t n) {
    return strncmp(p, dir, n) == 0 && (p[n] == '/' || p[n] == 0);
}

static int in_mount(const char *p) {
    for (size_t i = 0; i < nmounts; i++) {
        if (under(p, mounts[i].src, mounts[i].srclen)) return 1;
    }
    return 0;
}

// above reports whether p names a directory that holds dir, other than /.
static int above(const char *p, const char *dir, size_t n) {
    size_t pn = strlen(p);
    while (pn > 1 && p[pn - 1] == '/') pn--;
    return pn > 1 && pn < n && strncmp(p, dir, pn) == 0 && dir[pn] == '/';
}

static unsigned long env_number(const char *name) {
    const char *v = getenv(name);
    return v ? strtoul(v, NULL, 10) : 0;
}

// The variables that make a process the container's, which a program that
// clears or rebuilds its environment must not shed for its children, as
// it cannot leave a chroot by doing so.
static const char *const kept_names[] = {"DYLD_INSERT_LIBRARIES", "SANDBOX_ROOT", "SANDBOX_IPC_PREFIX", "FWS_MOUNTS", "FWS_HOSTNAME", NULL};
static char *kept[sizeof kept_names / sizeof kept_names[0]];

__attribute__((constructor)) static void fwsroot_init(void) {
    for (size_t i = 0; kept_names[i]; i++) {
        const char *v = getenv(kept_names[i]);
        if (v) {
            size_t n = strlen(kept_names[i]) + strlen(v) + 2;
            kept[i] = malloc(n);
            if (kept[i]) snprintf(kept[i], n, "%s=%s", kept_names[i], v);
        }
    }
    fake_uid = fake_euid = (uid_t)env_number("FWS_UID");
    fake_gid = fake_egid = (gid_t)env_number("FWS_GID");
    const char *h = getenv("FWS_HOSTNAME");
    if (h) {
        strlcpy(hostname, h, sizeof hostname);
    }
    parse_mounts(getenv("FWS_MOUNTS"));
    const char *r = getenv("SANDBOX_ROOT");
    if (r && r[0] == '/' && strlen(r) < sizeof root) {
        strcpy(root, r);
        rootlen = strlen(root);
        while (rootlen > 1 && root[rootlen - 1] == '/') {
            root[--rootlen] = 0;
        }
        if (!realpath(root, croot)) {
            strcpy(croot, root);
        }
    }
}

// The host trees a container uses as they are: no image holds them, and
// the dynamic loader and libSystem read them on every start.
static const char *const host_trees[] = {"/System/", "/usr/lib/", "/dev/", "/private/var/db/", "/var/db/", "/Library/Apple/", NULL};

static int in_root(const char *p) {
    return strncmp(p, root, rootlen) == 0 && (p[rootlen] == '/' || p[rootlen] == 0);
}

static int in_host_tree(const char *p) {
    for (const char *const *t = host_trees; *t; t++) {
        size_t n = strlen(*t);
        // The tree, or its top directory itself.
        if (strncmp(p, *t, n) == 0 || (strncmp(p, *t, n - 1) == 0 && p[n - 1] == 0)) {
            return 1;
        }
    }
    return 0;
}

// host_side reports whether p is the host's own path to the root or to a
// mount's source, which a program resolving a path through $SANDBOX_ROOT or
// a mount's symlink walks.
static int host_side(const char *p) {
    if (in_root(p) || above(p, root, rootlen)) {
        return 1;
    }
    for (size_t i = 0; i < nmounts; i++) {
        struct mount *m = &mounts[i];
        if (under(p, m->src, m->srclen) || above(p, m->src, m->srclen)) {
            return 1;
        }
    }
    return 0;
}

static const char *mount_source(const char *p, char *buf) {
    for (size_t i = 0; i < nmounts; i++) {
        struct mount *m = &mounts[i];
        if (under(p, m->dest, m->destlen)) {
            const char *rest = p + m->destlen;
            if (m->srclen + strlen(rest) + 1 > PATH_MAX) return NULL;
            memcpy(buf, m->src, m->srclen);
            strcpy(buf + m->srclen, rest);
            return buf;
        }
    }
    return NULL;
}

// host_of returns the host path that container path p names, whether or not
// it exists, or NULL for a path of the host's trees.
static const char *host_of(const char *p, char *buf) {
    const char *m = mount_source(p, buf);
    if (m) {
        return m;
    }
    if (in_host_tree(p) || rootlen + strlen(p) + 1 > PATH_MAX) {
        return NULL;
    }
    memcpy(buf, root, rootlen);
    strcpy(buf + rootlen, p);
    return buf;
}

// place returns the path container path p names, its symbolic links
// resolved: a mount's source under a mount's destination, the root's when
// the root holds it or its parent, else p.
static const char *place(const char *p, char *buf) {
    const char *m = mount_source(p, buf);
    if (m) {
        return m;
    }
    if (in_host_tree(p)) {
        return p;
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

// linked reports whether the kernel, resolving container path p in the
// root, would meet a symbolic link, which it would resolve on the host.
static int linked(const char *p) {
    char h[PATH_MAX], c[PATH_MAX];
    const char *q = host_of(p, h);
    if (!q) {
        return 0;
    }
    // The root's own path may pass through links, as /var does; its
    // resolved form names the same files without them.
    if (in_root(q) && snprintf(c, sizeof c, "%s%s", croot, q + rootlen) < (int)sizeof c) {
        q = c;
    }
    struct stat st;
    return fstatat(AT_FDCWD, q, &st, AT_SYMLINK_NOFOLLOW_ANY) == -1 && errno == ELOOP;
}

#define MAXLINKS 32

// resolve writes to out container path p with its symbolic links resolved
// in the container, as the kernel resolves them under chroot: an absolute
// target starts again at the container's /, and .. stops there. A link the
// final component names is followed if follow is set. Resolution stops at
// a component that does not exist, or that the host's trees hold.
static int resolve(const char *p, int follow, char *out) {
    char rest[PATH_MAX], next[PATH_MAX], cur[PATH_MAX], h[PATH_MAX], target[PATH_MAX];
    size_t curlen = 0;
    int links = 0;
    cur[0] = 0;
    if (strlcpy(rest, p, sizeof rest) >= sizeof rest) {
        return 0;
    }
    const char *r = rest;
    for (;;) {
        const char *slashes = r;
        while (*r == '/') r++;
        if (!*r) {
            // A trailing slash stays, so a file it follows is not a directory.
            r = slashes;
            break;
        }
        size_t n = strcspn(r, "/");
        if (n == 1 && r[0] == '.') {
            r += n;
            continue;
        }
        if (n == 2 && r[0] == '.' && r[1] == '.') {
            char *s = strrchr(cur, '/');
            if (s) {
                *s = 0;
                curlen = (size_t)(s - cur);
            }
            r += n;
            continue;
        }
        size_t before = curlen;
        if (curlen + n + 2 > sizeof cur) {
            return 0;
        }
        cur[curlen++] = '/';
        memcpy(cur + curlen, r, n);
        curlen += n;
        cur[curlen] = 0;
        r += n;
        // A trailing slash follows the final component's link.
        if (!*r && !follow) break;
        const char *q = host_of(cur, h);
        if (!q) break;
        ssize_t tn = readlink(q, target, sizeof target - 1);
        if (tn < 0) {
            if (errno == EINVAL) continue;
            break;
        }
        if (++links > MAXLINKS) {
            return 0;
        }
        target[tn] = 0;
        // r is empty or starts with a slash.
        if (snprintf(next, sizeof next, "%s%s", target, r) >= (int)sizeof next) {
            return 0;
        }
        memcpy(rest, next, strlen(next) + 1);
        r = rest;
        curlen = target[0] == '/' ? 0 : before;
        cur[curlen] = 0;
    }
    return snprintf(out, PATH_MAX, "%s%s", curlen || *r ? cur : "/", r) < PATH_MAX;
}

static const char *at_map(int fd, const char *path, char *buf, int follow);

// fws_map returns the host path that p names in the container. A link the
// final component names is followed if follow is set, as the call it maps
// follows one. buf holds a mapped path.
static const char *fws_map(const char *p, char *buf, int follow) {
    if (!rootlen || !p) {
        return p;
    }
    if (p[0] != '/') {
        return at_map(AT_FDCWD, p, buf, follow);
    }
    if (host_side(p)) {
        return p;
    }
    if (strstr(p, "/..") || linked(p)) {
        char c[PATH_MAX];
        if (resolve(p, follow, c)) {
            const char *q = place(c, buf);
            if (q == c) {
                strcpy(buf, c);
                return buf;
            }
            return q;
        }
    }
    return place(p, buf);
}

#define MAP(p) fws_map((p), (char[PATH_MAX]){0}, 1)
#define LMAP(p) fws_map((p), (char[PATH_MAX]){0}, 0)
#define MAPF(p, follow) fws_map((p), (char[PATH_MAX]){0}, (follow))

static void unmap(char *p, size_t size);

// at_map maps path relative to directory fd as fws_map maps an absolute
// path, where the kernel would resolve it otherwise: onto a mount, through
// a symbolic link, or above the root.
static const char *at_map(int fd, const char *path, char *buf, int follow) {
    if (!rootlen || !path) {
        return path;
    }
    if (path[0] == '/') {
        return fws_map(path, buf, follow);
    }
    struct stat st;
    if (!nmounts && !strstr(path, "..") && !(fstatat(fd, path, &st, AT_SYMLINK_NOFOLLOW_ANY) == -1 && errno == ELOOP)) {
        return path;
    }
    char dir[PATH_MAX];
    if (fd == AT_FDCWD ? getcwd(dir, sizeof dir) == NULL : fcntl(fd, F_GETPATH, dir) == -1) {
        return path;
    }
    if (!in_root(dir) && !in_mount(dir)) {
        return path;
    }
    unmap(dir, sizeof dir);
    char full[PATH_MAX];
    if (snprintf(full, sizeof full, "%s/%s", strcmp(dir, "/") ? dir : "", path) >= (int)sizeof full) {
        return path;
    }
    const char *q = fws_map(full, buf, follow);
    if (q == full) {
        strcpy(buf, full);
        return buf;
    }
    return q;
}
#define AT_MAP(fd, p) at_map((fd), (p), (char[PATH_MAX]){0}, 1)
#define AT_MAPF(fd, p, follow) at_map((fd), (p), (char[PATH_MAX]){0}, (follow))

// unmap turns a host path in the root or a mount's source into the
// container's path, in place.
static void unmap(char *p, size_t size) {
    if (!rootlen || !p) {
        return;
    }
    for (size_t i = 0; i < nmounts; i++) {
        struct mount *m = &mounts[i];
        if (under(p, m->src, m->srclen)) {
            size_t rest = strlen(p + m->srclen);
            if (m->destlen + rest + 1 > size) return;
            memmove(p + m->destlen, p + m->srclen, rest + 1);
            memcpy(p, m->dest, m->destlen);
            return;
        }
    }
    if (!in_root(p)) {
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

// follows reports whether an open with flags follows a link its path's
// final component names: O_EXCL with O_CREAT makes the file, not its target.
static int follows(int flags) {
    return !(flags & (O_NOFOLLOW | O_SYMLINK)) && (flags & (O_CREAT | O_EXCL)) != (O_CREAT | O_EXCL);
}

static int fws_open(const char *path, int flags, ...) {
    mode_t mode = 0;
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode = (mode_t)va_arg(ap, int);
        va_end(ap);
    }
    const char *p = MAPF(path, follows(flags));
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
    const char *p = MAPF(path, follows(flags));
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
    const char *p = AT_MAPF(fd, path, follows(flags));
    int fresh = (p[0] == '/' || fd == AT_FDCWD) && creating(p, flags);
    int r = openat(fd, p, flags, mode);
    if (r >= 0 && fresh) made(NULL, r, 0);
    return r;
}
INTERPOSE(fws_openat, openat);

// Bun, among others, opens files through the cancellation-point-free
// variants.
extern int openat_nocancel(int, const char *, int, ...) __asm("_openat$NOCANCEL");
static int fws_openat_nocancel(int fd, const char *path, int flags, ...) {
    mode_t mode = 0;
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode = (mode_t)va_arg(ap, int);
        va_end(ap);
    }
    const char *p = AT_MAPF(fd, path, follows(flags));
    int fresh = (p[0] == '/' || fd == AT_FDCWD) && creating(p, flags);
    int r = openat_nocancel(fd, p, flags, mode);
    if (r >= 0 && fresh) made(NULL, r, 0);
    return r;
}
INTERPOSE(fws_openat_nocancel, openat_nocancel);

static int fws_clonefile(const char *src, const char *dst, uint32_t flags) { return clonefile(MAPF(src, !(flags & CLONE_NOFOLLOW)), LMAP(dst), flags); }
INTERPOSE(fws_clonefile, clonefile);
static int fws_clonefileat(int sfd, const char *src, int dfd, const char *dst, uint32_t flags) {
    return clonefileat(sfd, AT_MAPF(sfd, src, !(flags & CLONE_NOFOLLOW)), dfd, AT_MAPF(dfd, dst, 0), flags);
}
INTERPOSE(fws_clonefileat, clonefileat);
static int fws_fclonefileat(int sfd, int dfd, const char *dst, uint32_t flags) { return fclonefileat(sfd, dfd, AT_MAPF(dfd, dst, 0), flags); }
INTERPOSE(fws_fclonefileat, fclonefileat);

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
static int fws_faccessat(int fd, const char *path, int mode, int flag) { return faccessat(fd, AT_MAPF(fd, path, !(flag & AT_SYMLINK_NOFOLLOW)), mode, flag); }
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
    const char *p = LMAP(path);
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
    const char *p = AT_MAPF(fd, path, !(flag & AT_SYMLINK_NOFOLLOW));
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
    const char *p = LMAP(path);
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
    const char *p = AT_MAPF(fd, path, !(flag & AT_SYMLINK_NOFOLLOW));
    int r = fstatat64(fd, p, st, flag);
    if (r == 0 && (p[0] == '/' || fd == AT_FDCWD)) own(st, p, -1, flag & AT_SYMLINK_NOFOLLOW);
    return r;
}
INTERPOSE(fws_fstatat64, fstatat64);

static int fws_statfs(const char *path, struct statfs *st) { return statfs(MAP(path), st); }
INTERPOSE(fws_statfs, statfs);
static int fws_getattrlist(const char *path, void *attrs, void *buf, size_t size, unsigned int opts) {
    return getattrlist(MAPF(path, !(opts & FSOPT_NOFOLLOW)), attrs, buf, size, opts);
}
INTERPOSE(fws_getattrlist, getattrlist);
static int fws_setattrlist(const char *path, void *attrs, void *buf, size_t size, unsigned int opts) {
    return setattrlist(MAPF(path, !(opts & FSOPT_NOFOLLOW)), attrs, buf, size, opts);
}
INTERPOSE(fws_setattrlist, setattrlist);
static long fws_pathconf(const char *path, int name) { return pathconf(MAP(path), name); }
INTERPOSE(fws_pathconf, pathconf);

static int fws_chdir(const char *path) { return chdir(MAP(path)); }
INTERPOSE(fws_chdir, chdir);
static int fws_mkdir(const char *path, mode_t mode) {
    const char *p = LMAP(path);
    int r = mkdir(p, mode);
    if (r == 0 && fake_user()) made(p, -1, 0);
    return r;
}
INTERPOSE(fws_mkdir, mkdir);
static int fws_mkdirat(int fd, const char *path, mode_t mode) { return mkdirat(fd, AT_MAPF(fd, path, 0), mode); }
INTERPOSE(fws_mkdirat, mkdirat);
static int fws_rmdir(const char *path) { return rmdir(LMAP(path)); }
INTERPOSE(fws_rmdir, rmdir);
static int fws_unlink(const char *path) { return unlink(LMAP(path)); }
INTERPOSE(fws_unlink, unlink);
static int fws_unlinkat(int fd, const char *path, int flag) { return unlinkat(fd, AT_MAPF(fd, path, 0), flag); }
INTERPOSE(fws_unlinkat, unlinkat);
static int fws_rename(const char *from, const char *to) { return rename(LMAP(from), LMAP(to)); }
INTERPOSE(fws_rename, rename);
static int fws_renameat(int fd1, const char *from, int fd2, const char *to) { return renameat(fd1, AT_MAPF(fd1, from, 0), fd2, AT_MAPF(fd2, to, 0)); }
INTERPOSE(fws_renameat, renameat);
static int fws_renamex_np(const char *from, const char *to, unsigned int flags) { return renamex_np(LMAP(from), LMAP(to), flags); }
INTERPOSE(fws_renamex_np, renamex_np);
static int fws_renameatx_np(int fd1, const char *from, int fd2, const char *to, unsigned int flags) {
    return renameatx_np(fd1, AT_MAPF(fd1, from, 0), fd2, AT_MAPF(fd2, to, 0), flags);
}
INTERPOSE(fws_renameatx_np, renameatx_np);
static int fws_link(const char *from, const char *to) { return link(LMAP(from), LMAP(to)); }
INTERPOSE(fws_link, link);
static int fws_linkat(int fd1, const char *from, int fd2, const char *to, int flag) { return linkat(fd1, AT_MAPF(fd1, from, flag & AT_SYMLINK_FOLLOW), fd2, AT_MAPF(fd2, to, 0), flag); }
INTERPOSE(fws_linkat, linkat);
// A link's target is the link's to keep; only where the link goes maps.
static int fws_symlink(const char *target, const char *path) {
    const char *p = LMAP(path);
    int r = symlink(target, p);
    if (r == 0 && fake_user()) made(p, -1, 1);
    return r;
}
INTERPOSE(fws_symlink, symlink);
static int fws_symlinkat(const char *target, int fd, const char *path) { return symlinkat(target, fd, AT_MAPF(fd, path, 0)); }
INTERPOSE(fws_symlinkat, symlinkat);
static ssize_t fws_readlink(const char *path, char *buf, size_t size) { return readlink(LMAP(path), buf, size); }
INTERPOSE(fws_readlink, readlink);
static ssize_t fws_readlinkat(int fd, const char *path, char *buf, size_t size) { return readlinkat(fd, AT_MAPF(fd, path, 0), buf, size); }
INTERPOSE(fws_readlinkat, readlinkat);
static int fws_chmod(const char *path, mode_t mode) { return chmod(MAP(path), mode); }
INTERPOSE(fws_chmod, chmod);
static int fws_fchmodat(int fd, const char *path, mode_t mode, int flag) { return fchmodat(fd, AT_MAPF(fd, path, !(flag & AT_SYMLINK_NOFOLLOW)), mode, flag); }
INTERPOSE(fws_fchmodat, fchmodat);
static int fws_utimes(const char *path, const struct timeval t[2]) { return utimes(MAP(path), t); }
INTERPOSE(fws_utimes, utimes);
static int fws_lutimes(const char *path, const struct timeval t[2]) { return lutimes(LMAP(path), t); }
INTERPOSE(fws_lutimes, lutimes);
static int fws_utimensat(int fd, const char *path, const struct timespec t[2], int flag) { return utimensat(fd, AT_MAPF(fd, path, !(flag & AT_SYMLINK_NOFOLLOW)), t, flag); }
INTERPOSE(fws_utimensat, utimensat);
static int fws_truncate(const char *path, off_t len) { return truncate(MAP(path), len); }
INTERPOSE(fws_truncate, truncate);
static int fws_mkfifo(const char *path, mode_t mode) {
    const char *p = LMAP(path);
    int r = mkfifo(p, mode);
    if (r == 0 && fake_user()) made(p, -1, 0);
    return r;
}
INTERPOSE(fws_mkfifo, mkfifo);
static int fws_chflags(const char *path, unsigned int flags) { return chflags(MAP(path), flags); }
INTERPOSE(fws_chflags, chflags);
static ssize_t fws_getxattr(const char *path, const char *name, void *v, size_t n, uint32_t pos, int o) { return getxattr(MAPF(path, !(o & XATTR_NOFOLLOW)), name, v, n, pos, o); }
INTERPOSE(fws_getxattr, getxattr);
static int fws_setxattr(const char *path, const char *name, const void *v, size_t n, uint32_t pos, int o) { return setxattr(MAPF(path, !(o & XATTR_NOFOLLOW)), name, v, n, pos, o); }
INTERPOSE(fws_setxattr, setxattr);
static ssize_t fws_listxattr(const char *path, char *buf, size_t n, int o) { return listxattr(MAPF(path, !(o & XATTR_NOFOLLOW)), buf, n, o); }
INTERPOSE(fws_listxattr, listxattr);
static int fws_removexattr(const char *path, const char *name, int o) { return removexattr(MAPF(path, !(o & XATTR_NOFOLLOW)), name, o); }
INTERPOSE(fws_removexattr, removexattr);

// find_on_path finds the executable file, a name without a slash, in a
// directory of PATH, whose directories name the host's paths of the
// container's, and leaves its path in full.
static int find_on_path(const char *file, char *full) {
    const char *path = getenv("PATH");
    char dir[PATH_MAX];
    while (path && *path) {
        size_t n = strcspn(path, ":");
        if (n > 0 && n < sizeof dir) {
            memcpy(dir, path, n);
            dir[n] = 0;
            if (snprintf(full, PATH_MAX, "%s/%s", dir, file) < PATH_MAX && access(MAP(full), X_OK) == 0) {
                return 1;
            }
        }
        path += n;
        if (*path == ':') path++;
    }
    return 0;
}

// env_argv runs a script whose #! line is /usr/bin/env, which the root does
// not hold, as env would: the program its argument names, found on PATH,
// with the argument's other words under -S, then the script and its
// arguments. Apple's env does not run with the container library loaded.
static char **env_argv(char *arg, const char *path, char *const argv[], char *interp) {
    char *words[64];
    size_t nw = 0;
    if (strncmp(arg, "-S", 2) == 0 && (arg[2] == ' ' || arg[2] == '\t')) {
        for (char *w = strtok(arg + 3, " \t"); w && nw < 64; w = strtok(NULL, " \t")) words[nw++] = w;
    } else if (*arg) {
        words[nw++] = arg;
    }
    if (nw == 0 || words[0][0] == '-' || strchr(words[0], '=')) {
        return NULL;
    }
    if (strchr(words[0], '/')) {
        const char *m = fws_map(words[0], interp, 1);
        if (m != interp) strlcpy(interp, m, PATH_MAX);
    } else if (!find_on_path(words[0], interp)) {
        return NULL;
    }
    size_t argc = 0;
    while (argv && argv[argc]) argc++;
    char **out = calloc(nw + argc + 2, sizeof *out);
    if (!out) {
        return NULL;
    }
    size_t i = 0;
    for (size_t j = 0; j < nw; j++) out[i++] = strdup(words[j]);
    out[i++] = (char *)path;
    for (size_t j = 1; j < argc; j++) out[i++] = argv[j];
    return out;
}

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
    const char *mapped = fws_map(line, interp, 1);
    if (mapped != interp && strcmp(line, "/usr/bin/env") == 0) {
        return env_argv(arg, path, argv, interp);
    }
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

static int has_var(char *const envp[], const char *kv) {
    size_t n = strcspn(kv, "=") + 1;
    for (size_t i = 0; envp && envp[i]; i++) {
        if (strncmp(envp[i], kv, n) == 0) return 1;
    }
    return 0;
}

// child_env is envp for a program the process starts: with the identity
// the process holds now, which setuid changed without the program's own
// copy of its environment knowing, and the container's variables.
// Allocated; the caller frees the array.
static char **child_env(char *const envp[], char *ids) {
    size_t n = 0;
    while (envp && envp[n]) n++;
    char **out = calloc(n + 8, sizeof *out);
    if (!out) return NULL;
    size_t j = 0;
    for (size_t i = 0; i < n; i++) {
        if (strncmp(envp[i], "FWS_UID=", 8) == 0 || strncmp(envp[i], "FWS_GID=", 8) == 0) continue;
        out[j++] = envp[i];
    }
    int a = snprintf(ids, 64, "FWS_UID=%u", fake_euid);
    snprintf(ids + a + 1, 64 - a - 1, "FWS_GID=%u", fake_egid);
    out[j++] = ids;
    out[j++] = ids + a + 1;
    for (size_t i = 0; kept_names[i]; i++) {
        if (kept[i] && !has_var(envp, kept[i])) out[j++] = kept[i];
    }
    return out;
}

static int fws_execve(const char *path, char *const argv[], char *const envp[]) {
    char ids[64];
    char **env = child_env(envp, ids);
    if (env) envp = env;
    const char *p = MAP(path);
    char interp[PATH_MAX];
    char **sargv = script_argv(p, argv, interp);
    int r = sargv ? execve(interp, sargv, envp) : execve(p, argv, envp);
    free(sargv);
    free(env);
    return r;
}
INTERPOSE(fws_execve, execve);
static int fws_posix_spawn(pid_t *pid, const char *path, const posix_spawn_file_actions_t *fa, const posix_spawnattr_t *a,
                           char *const argv[], char *const envp[]) {
    char ids[64];
    char **env = child_env(envp, ids);
    if (env) envp = env;
    const char *p = MAP(path);
    char interp[PATH_MAX];
    char **sargv = script_argv(p, argv, interp);
    int r = sargv ? posix_spawn(pid, interp, fa, a, sargv, envp) : posix_spawn(pid, p, fa, a, argv, envp);
    free(sargv);
    free(env);
    return r;
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
    char full[PATH_MAX];
    if (find_on_path(file, full)) {
        return fws_posix_spawn(pid, full, fa, a, argv, envp);
    }
    char ids[64];
    char **env = child_env(envp, ids);
    int r = posix_spawnp(pid, file, fa, a, argv, env ? env : envp);
    free(env);
    return r;
}
INTERPOSE(fws_posix_spawnp, posix_spawnp);

static void *fws_dlopen(const char *path, int mode) { return dlopen(MAP(path), mode); }
INTERPOSE(fws_dlopen, dlopen);

static char *fws_getcwd(char *buf, size_t size) {
    char *r = getcwd(buf, size);
    // getcwd allocates the buffer when given none, as large as the path.
    unmap(r, r && !buf ? strlen(r) + 1 : size);
    return r;
}

static char *fws_realpath(const char *path, char *resolved) {
    char *r = realpath(MAP(path), resolved);
    unmap(r, PATH_MAX);
    return r;
}
INTERPOSE(fws_realpath, realpath);
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
    if (path && path[0] == '/' && !in_root(path) && !in_mount(path)) {
        // The host's directories above the root and the mounts hold no
        // owner of the container's; the sandbox refuses their attributes.
        st->st_uid = 0;
        st->st_gid = 0;
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
    const char *p = LMAP(path);
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
                                 .pw_gecos = "root", .pw_dir = "/root", .pw_shell = "/bin/sh", .pw_class = ""};
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
                                 .pw_dir = fields[5], .pw_shell = fields[6], .pw_class = ""};
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

// The lookups share one buffer, which a lock keeps whole for the reentrant
// variants; a user or group that does not exist leaves errno as it was, as
// libSystem's lookups do.
static pthread_mutex_t db_lock = PTHREAD_MUTEX_INITIALIZER;

static struct passwd *lookup_passwd(const char *name, uid_t uid, int byname) {
    int saved = errno;
    struct passwd *p = find_passwd(name, uid, byname);
    errno = saved;
    return p;
}
static struct group *lookup_group(const char *name, gid_t gid, int byname) {
    int saved = errno;
    struct group *g = find_group(name, gid, byname);
    errno = saved;
    return g;
}
static struct passwd *fws_getpwuid(uid_t uid) { return lookup_passwd(NULL, uid, 0); }
INTERPOSE(fws_getpwuid, getpwuid);
static struct passwd *fws_getpwnam(const char *name) { return lookup_passwd(name, 0, 1); }
INTERPOSE(fws_getpwnam, getpwnam);
static struct group *fws_getgrgid(gid_t gid) { return lookup_group(NULL, gid, 0); }
INTERPOSE(fws_getgrgid, getgrgid);
static struct group *fws_getgrnam(const char *name) { return lookup_group(name, 0, 1); }
INTERPOSE(fws_getgrnam, getgrnam);

// getlogin names the container's user, not the host session's.
static char login[256];
static char *fws_getlogin(void) {
    pthread_mutex_lock(&db_lock);
    struct passwd *p = lookup_passwd(NULL, fake_uid, 0);
    char *r = NULL;
    if (p) {
        strlcpy(login, p->pw_name, sizeof login);
        r = login;
    }
    pthread_mutex_unlock(&db_lock);
    return r;
}
INTERPOSE(fws_getlogin, getlogin);
static int fws_getlogin_r(char *buf, size_t size) {
    pthread_mutex_lock(&db_lock);
    struct passwd *p = lookup_passwd(NULL, fake_uid, 0);
    int r = ENOENT;
    if (p) {
        r = strlcpy(buf, p->pw_name, size) < size ? 0 : ERANGE;
    }
    pthread_mutex_unlock(&db_lock);
    return r;
}
INTERPOSE(fws_getlogin_r, getlogin_r);

static int copy_passwd(struct passwd *found, struct passwd *out, char *buf, size_t size, struct passwd **result) {
    *result = NULL;
    if (!found) {
        return 0;
    }
    const char *src[] = {found->pw_name, found->pw_passwd, found->pw_gecos, found->pw_dir, found->pw_shell, found->pw_class};
    char **dst[] = {&out->pw_name, &out->pw_passwd, &out->pw_gecos, &out->pw_dir, &out->pw_shell, &out->pw_class};
    size_t used = 0;
    *out = *found;
    for (int i = 0; i < 6; i++) {
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
    pthread_mutex_lock(&db_lock);
    int r = copy_passwd(lookup_passwd(NULL, uid, 0), out, buf, size, result);
    pthread_mutex_unlock(&db_lock);
    return r;
}
INTERPOSE(fws_getpwuid_r, getpwuid_r);
static int fws_getpwnam_r(const char *name, struct passwd *out, char *buf, size_t size, struct passwd **result) {
    pthread_mutex_lock(&db_lock);
    int r = copy_passwd(lookup_passwd(name, 0, 1), out, buf, size, result);
    pthread_mutex_unlock(&db_lock);
    return r;
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
    pthread_mutex_lock(&db_lock);
    int r = copy_group(lookup_group(NULL, gid, 0), out, buf, size, result);
    pthread_mutex_unlock(&db_lock);
    return r;
}
INTERPOSE(fws_getgrgid_r, getgrgid_r);
static int fws_getgrnam_r(const char *name, struct group *out, char *buf, size_t size, struct group **result) {
    pthread_mutex_lock(&db_lock);
    int r = copy_group(lookup_group(name, 0, 1), out, buf, size, result);
    pthread_mutex_unlock(&db_lock);
    return r;
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

// IOKit. The sandbox denies a container IOKit's user clients, and the host's
// power management above all; a program that registers for power
// notifications gets a registration that never fires, as a container sees
// no sleep or wake of its own. The library binds IOKit weakly, so a process
// that does not load IOKit itself does not load it for the library.
typedef unsigned int fws_io_object_t;
typedef struct IONotificationPort *fws_notify_port_t;
typedef void (*fws_interest_cb)(void *, fws_io_object_t, uint32_t, void *);
extern fws_io_object_t IORegisterForSystemPower(void *, fws_notify_port_t *, fws_interest_cb, fws_io_object_t *)
    __attribute__((weak_import));
extern fws_notify_port_t IONotificationPortCreate(unsigned int) __attribute__((weak_import));

static fws_io_object_t fws_IORegisterForSystemPower(void *refcon, fws_notify_port_t *port, fws_interest_cb cb,
                                                     fws_io_object_t *notifier) {
    fws_io_object_t r = IORegisterForSystemPower(refcon, port, cb, notifier);
    if (r != 0) {
        return r;
    }
    fws_notify_port_t p = IONotificationPortCreate(0);
    if (!p) {
        return 0;
    }
    // A port of the process's own, which nothing sends to, stands for the
    // connection.
    mach_port_t conn = MACH_PORT_NULL;
    if (mach_port_allocate(mach_task_self(), MACH_PORT_RIGHT_RECEIVE, &conn) != KERN_SUCCESS) {
        return 0;
    }
    *port = p;
    if (notifier) *notifier = 0;
    return conn;
}
INTERPOSE(fws_IORegisterForSystemPower, IORegisterForSystemPower);

// Mach services. As POSIX IPC names are, the bootstrap names a container
// registers carry its IPC prefix (SANDBOX_IPC_PREFIX), which the sandbox
// lets it register and look up and no other container sees; a lookup finds
// the container's own service first, then the host's.
static int ipc_name(const char *name, char *out) {
    const char *prefix = getenv("SANDBOX_IPC_PREFIX");
    if (!prefix || !*prefix || strncmp(name, prefix, strlen(prefix)) == 0) {
        return 0;
    }
    return snprintf(out, sizeof(name_t), "%s%s", prefix, name) < (int)sizeof(name_t);
}

static kern_return_t fws_bootstrap_check_in(mach_port_t bp, const name_t name, mach_port_t *sp) {
    name_t own;
    return bootstrap_check_in(bp, ipc_name(name, own) ? own : name, sp);
}
INTERPOSE(fws_bootstrap_check_in, bootstrap_check_in);

static kern_return_t fws_bootstrap_look_up(mach_port_t bp, const name_t name, mach_port_t *sp) {
    name_t own;
    if (ipc_name(name, own)) {
        kern_return_t r = bootstrap_look_up(bp, own, sp);
        if (r != BOOTSTRAP_UNKNOWN_SERVICE) {
            return r;
        }
    }
    return bootstrap_look_up(bp, name, sp);
}
INTERPOSE(fws_bootstrap_look_up, bootstrap_look_up);

// POSIX shared memory and semaphores. Under private IPC a container's names
// carry its prefix, which the sandbox confines it to; the library gives a
// name the prefix, so a program names its objects as it would on its own
// host. macOS caps the names at 31 bytes: a name too long to carry the
// prefix becomes the prefix and a hash of the name.
#define POSIX_IPC_NAME_MAX 31

static const char *posix_ipc_name(const char *name, char *out) {
    const char *prefix = getenv("SANDBOX_IPC_PREFIX");
    if (!prefix || !*prefix || !name) {
        return name;
    }
    const char *slash = name[0] == '/' ? "/" : "";
    const char *n = name + strlen(slash);
    if (strncmp(n, prefix, strlen(prefix)) == 0) {
        return name;
    }
    if (snprintf(out, POSIX_IPC_NAME_MAX + 1, "%s%s%s", slash, prefix, n) <= POSIX_IPC_NAME_MAX) {
        return out;
    }
    uint64_t h = 14695981039346656037ULL;
    for (const unsigned char *c = (const unsigned char *)n; *c; c++) {
        h = (h ^ *c) * 1099511628211ULL;
    }
    snprintf(out, POSIX_IPC_NAME_MAX + 1, "%s%s%016llx", slash, prefix, (unsigned long long)h);
    return out;
}

static int fws_shm_open(const char *name, int flags, ...) {
    mode_t mode = 0;
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode = (mode_t)va_arg(ap, int);
        va_end(ap);
    }
    char own[POSIX_IPC_NAME_MAX + 1];
    return shm_open(posix_ipc_name(name, own), flags, mode);
}
INTERPOSE(fws_shm_open, shm_open);

static int fws_shm_unlink(const char *name) {
    char own[POSIX_IPC_NAME_MAX + 1];
    return shm_unlink(posix_ipc_name(name, own));
}
INTERPOSE(fws_shm_unlink, shm_unlink);

static sem_t *fws_sem_open(const char *name, int flags, ...) {
    char own[POSIX_IPC_NAME_MAX + 1];
    const char *n = posix_ipc_name(name, own);
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode_t mode = (mode_t)va_arg(ap, int);
        unsigned value = va_arg(ap, unsigned);
        va_end(ap);
        return sem_open(n, flags, mode, value);
    }
    return sem_open(n, flags);
}
INTERPOSE(fws_sem_open, sem_open);

static int fws_sem_unlink(const char *name) {
    char own[POSIX_IPC_NAME_MAX + 1];
    return sem_unlink(posix_ipc_name(name, own));
}
INTERPOSE(fws_sem_unlink, sem_unlink);
