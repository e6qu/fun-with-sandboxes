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
#include <signal.h>
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
#include <sys/ipc.h>
#include <sys/msg.h>
#include <sys/sem.h>
#include <sys/shm.h>
#include <sys/sysctl.h>
#include <sys/param.h>
#include <libproc.h>
#include <sys/proc_info.h>
#include <sys/mount.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <netdb.h>
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
// The host pid of the container's main process (FWS_INIT), its pid 1.
static pid_t init_pid;
// The engine's helper binary's name (FWS_HELPER), which the container's
// exec anchor runs as.
static char helper_comm[MAXCOMLEN + 1];

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

// join writes a, b and c to out, a buffer of size bytes, and reports
// whether they fit. Path mapping formats nothing: libSystem calls it from
// setlocale, which holds the lock snprintf takes to read the locale.
static int join(char *out, size_t size, const char *a, const char *b, const char *c) {
    size_t na = strlen(a), nb = strlen(b), nc = strlen(c);
    if (na + nb + nc + 1 > size) {
        return 0;
    }
    memmove(out, a, na);
    memmove(out + na, b, nb);
    memmove(out + na + nb, c, nc + 1);
    return 1;
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

// The container's ports (FWS_PORTS: "80/tcp=30001,53/udp=30002"): the host
// port behind each container port, which the sandbox lets the container
// bind on loopback, its only interface.
struct port_map {
    int type;
    unsigned short cport, hport;
};
static struct port_map port_maps[512];
static size_t nport_maps;

static void parse_ports(const char *v) {
    while (v && *v && nport_maps < sizeof port_maps / sizeof port_maps[0]) {
        char *end;
        unsigned long c = strtoul(v, &end, 10);
        int type = strncmp(end, "/tcp=", 5) == 0 ? SOCK_STREAM : strncmp(end, "/udp=", 5) == 0 ? SOCK_DGRAM : 0;
        if (!type) {
            return;
        }
        unsigned long h = strtoul(end + 5, &end, 10);
        if (c && c < 65536 && h && h < 65536) {
            port_maps[nport_maps++] = (struct port_map){type, (unsigned short)c, (unsigned short)h};
        }
        if (*end != ',') {
            return;
        }
        v = end + 1;
    }
}

// num parses the decimal number at p into v and returns the end, or NULL.
// The library parses numbers itself where libSystem may hold the locale's
// lock.
static const char *num(const char *p, unsigned long *v) {
    if (*p < '0' || *p > '9') {
        return NULL;
    }
    unsigned long n = 0;
    while (*p >= '0' && *p <= '9') {
        n = n * 10 + (unsigned long)(*p++ - '0');
        if (n > 0xffffffffUL) return NULL;
    }
    *v = n;
    return p;
}

static int parse_v4(const char *p, const char **end, in_addr_t *out) {
    uint32_t v = 0;
    for (int i = 0; i < 4; i++) {
        unsigned long n;
        if (!(p = num(p, &n)) || n > 255) return 0;
        v = v << 8 | (uint32_t)n;
        if (i < 3) {
            if (*p != '.') return 0;
            p++;
        }
    }
    *end = p;
    *out = htonl(v);
    return 1;
}

// The stack's members (FWS_NETWORK): a file the engine rewrites as members
// come and go, of a line per member holding its address, the names it
// answers to, and its ports as FWS_PORTS lists them, tab-separated. Its
// addresses lie in 127.88.0.0/16, which the kernel never sees: the library
// reaches a member's ports through their host ports.
static char network_path[PATH_MAX];
static in_addr_t own_address;

// The container's TCP ports that have no host port: the library listens on
// one with a Unix socket in the port directory (FWS_PORTDIR), and a
// client reaches it through the router of the container that listens
// (FWS_ROUTER for this one's; package router).
static char port_dir[PATH_MAX];
static unsigned short router_port;

static void parse_routing(const char *dir, const char *router) {
    unsigned long v;
    const char *e;
    if (dir && dir[0] == '/' && strlen(dir) + 16 < sizeof port_dir) {
        strcpy(port_dir, dir);
    }
    if (router && (e = num(router, &v)) && !*e && v < 65536) {
        router_port = (unsigned short)v;
    }
}

static void parse_network(const char *path, const char *address) {
    const char *end;
    if (path && path[0] == '/' && strlen(path) < sizeof network_path) {
        strcpy(network_path, path);
    }
    if (address && !(parse_v4(address, &end, &own_address) && !*end)) {
        own_address = 0;
    }
}

// The variables that make a process the container's, which a program that
// clears or rebuilds its environment must not shed for its children, as
// it cannot leave a chroot by doing so.
static const char *const kept_names[] = {"DYLD_INSERT_LIBRARIES", "SANDBOX_ROOT", "SANDBOX_IPC_PREFIX", "FWS_MOUNTS", "FWS_HOSTNAME", "FWS_PORTS", "FWS_NETWORK", "FWS_ADDRESS", "FWS_HELPER", "FWS_INIT", "FWS_PORTDIR", "FWS_ROUTER", NULL};
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
    init_pid = (pid_t)env_number("FWS_INIT");
    const char *hc = getenv("FWS_HELPER");
    if (hc) {
        strlcpy(helper_comm, hc, sizeof helper_comm);
    }
    const char *h = getenv("FWS_HOSTNAME");
    if (h) {
        strlcpy(hostname, h, sizeof hostname);
    }
    parse_mounts(getenv("FWS_MOUNTS"));
    parse_ports(getenv("FWS_PORTS"));
    parse_network(getenv("FWS_NETWORK"), getenv("FWS_ADDRESS"));
    parse_routing(getenv("FWS_PORTDIR"), getenv("FWS_ROUTER"));
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

// meets_link reports whether the kernel, resolving path relative to fd,
// would meet a symbolic link: AT_SYMLINK_NOFOLLOW_ANY refuses one on the
// way, and reports one the final component names as lstat does.
static int meets_link(int fd, const char *path) {
    struct stat st;
    if (fstatat(fd, path, &st, AT_SYMLINK_NOFOLLOW_ANY) == -1) {
        return errno == ELOOP;
    }
    return S_ISLNK(st.st_mode);
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
    if (in_root(q) && join(c, sizeof c, croot, q + rootlen, "")) {
        q = c;
    }
    return meets_link(AT_FDCWD, q);
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
        if (!join(next, sizeof next, target, r, "")) {
            return 0;
        }
        memcpy(rest, next, strlen(next) + 1);
        r = rest;
        curlen = target[0] == '/' ? 0 : before;
        cur[curlen] = 0;
    }
    return join(out, PATH_MAX, curlen || *r ? cur : "/", r, "");
}

static const char *at_map(int fd, const char *path, char *buf, int follow);

// cwd writes the working directory's host path to dir. libSystem's getcwd
// walks .. through calls this library interposes; the library's own calls
// stay the host's, and O_EVTONLY needs no right to read the directory.
static int cwd(char *dir) {
    int fd = open(".", O_EVTONLY | O_DIRECTORY | O_CLOEXEC);
    if (fd < 0) {
        return 0;
    }
    int ok = fcntl(fd, F_GETPATH, dir) != -1;
    close(fd);
    return ok;
}

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
    // An empty path names nothing, whatever the directory.
    if (!rootlen || !path || !*path) {
        return path;
    }
    if (path[0] == '/') {
        return fws_map(path, buf, follow);
    }
    if (!nmounts && !strstr(path, "..") && !meets_link(fd, path)) {
        return path;
    }
    char dir[PATH_MAX];
    if (!(fd == AT_FDCWD ? cwd(dir) : fcntl(fd, F_GETPATH, dir) != -1)) {
        return path;
    }
    if (!in_root(dir) && !in_mount(dir)) {
        return path;
    }
    unmap(dir, sizeof dir);
    char full[PATH_MAX];
    if (!join(full, sizeof full, strcmp(dir, "/") ? dir : "", "/", path)) {
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
            if (join(full, PATH_MAX, dir, "/", file) && access(MAP(full), X_OK) == 0) {
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
    if (buf && size == 0) {
        errno = EINVAL;
        return NULL;
    }
    char dir[PATH_MAX];
    if (!cwd(dir)) {
        return NULL;
    }
    unmap(dir, sizeof dir);
    size_t n = strlen(dir) + 1;
    // getcwd allocates the buffer when given none: size bytes, or as many
    // as the path takes.
    if (!buf) {
        buf = malloc(size > n ? size : n);
        if (!buf) {
            return NULL;
        }
        size = size > n ? size : n;
    }
    if (n > size) {
        errno = ERANGE;
        return NULL;
    }
    memcpy(buf, dir, n);
    return buf;
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

// Pid 1. The container's main process (FWS_INIT, its host pid) is pid 1 to
// the container, as in a PID namespace, so a program that checks it runs
// as init does, and `kill -HUP 1` reaches it; every other process keeps
// its host pid, which the container sees as it is.

static pid_t to_host(pid_t p) { return init_pid && p == 1 ? init_pid : p; }
static pid_t to_container(pid_t p) { return init_pid && p == init_pid ? 1 : p; }

static pid_t fws_getpid(void) { return to_container(getpid()); }
INTERPOSE(fws_getpid, getpid);

// Init's parent is outside the container: 0, as in a PID namespace.
static pid_t fws_getppid(void) { return init_pid && getpid() == init_pid ? 0 : to_container(getppid()); }
INTERPOSE(fws_getppid, getppid);

static int fws_kill(pid_t pid, int sig) {
    if (pid < -1) {
        return kill(-to_host(-pid), sig);
    }
    return kill(pid == -1 ? pid : to_host(pid), sig);
}
INTERPOSE(fws_kill, kill);

static int fws_killpg(pid_t pgrp, int sig) { return killpg(to_host(pgrp), sig); }
INTERPOSE(fws_killpg, killpg);

static pid_t fws_getpgrp(void) { return to_container(getpgrp()); }
INTERPOSE(fws_getpgrp, getpgrp);

static pid_t fws_getpgid(pid_t pid) {
    pid_t r = getpgid(to_host(pid));
    return r < 0 ? r : to_container(r);
}
INTERPOSE(fws_getpgid, getpgid);

static pid_t fws_getsid(pid_t pid) {
    pid_t r = getsid(to_host(pid));
    return r < 0 ? r : to_container(r);
}
INTERPOSE(fws_getsid, getsid);

static int fws_setpgid(pid_t pid, pid_t pgid) { return setpgid(to_host(pid), to_host(pgid)); }
INTERPOSE(fws_setpgid, setpgid);

static int fws_proc_pidinfo(int pid, int flavor, uint64_t arg, void *buf, int size) {
    int r = proc_pidinfo(to_host(pid), flavor, arg, buf, size);
    if (flavor == PROC_PIDTBSDINFO && r == (int)sizeof(struct proc_bsdinfo)) {
        struct proc_bsdinfo *b = buf;
        b->pbi_ppid = (uint32_t)(b->pbi_pid == (uint32_t)init_pid && init_pid ? 0 : to_container((pid_t)b->pbi_ppid));
        b->pbi_pid = (uint32_t)to_container((pid_t)b->pbi_pid);
        b->pbi_pgid = (uint32_t)to_container((pid_t)b->pbi_pgid);
    } else if (flavor == PROC_PIDT_SHORTBSDINFO && r == (int)sizeof(struct proc_bsdshortinfo)) {
        struct proc_bsdshortinfo *b = buf;
        b->pbsi_ppid = (uint32_t)(b->pbsi_pid == (uint32_t)init_pid && init_pid ? 0 : to_container((pid_t)b->pbsi_ppid));
        b->pbsi_pid = (uint32_t)to_container((pid_t)b->pbsi_pid);
        b->pbsi_pgid = (uint32_t)to_container((pid_t)b->pbsi_pgid);
    }
    return r;
}
INTERPOSE(fws_proc_pidinfo, proc_pidinfo);

static int fws_proc_pidpath(int pid, void *buf, uint32_t size) { return proc_pidpath(to_host(pid), buf, size); }
INTERPOSE(fws_proc_pidpath, proc_pidpath);

static int fws_proc_name(int pid, void *buf, uint32_t size) { return proc_name(to_host(pid), buf, size); }
INTERPOSE(fws_proc_name, proc_name);

static int fws_proc_pid_rusage(int pid, int flavor, rusage_info_t *buf) { return proc_pid_rusage(to_host(pid), flavor, buf); }
INTERPOSE(fws_proc_pid_rusage, proc_pid_rusage);

// Processes. The container's processes are those of its sandbox, whose
// information the sandbox lets them read, as a PID namespace holds a
// container's processes; a listing leaves out the others, and the anchor
// that starts the container's execs.
static int visible(pid_t pid, const char *comm) {
    if (pid == getpid()) {
        return 1;
    }
    if (helper_comm[0] && strncmp(comm, helper_comm, MAXCOMLEN) == 0) {
        return 0;
    }
    struct proc_bsdshortinfo info;
    return proc_pidinfo(pid, PROC_PIDT_SHORTBSDINFO, 0, &info, sizeof info) == (int)sizeof info;
}

// own_procs keeps the container's processes of a kinfo_proc array of n
// bytes, in place, and returns its new size.
static size_t own_procs(struct kinfo_proc *kp, size_t n) {
    size_t count = n / sizeof *kp, kept = 0;
    for (size_t i = 0; i < count; i++) {
        if (visible(kp[i].kp_proc.p_pid, kp[i].kp_proc.p_comm)) {
            if (kept != i) memmove(&kp[kept], &kp[i], sizeof *kp);
            struct kinfo_proc *k = &kp[kept];
            k->kp_eproc.e_ppid = init_pid && k->kp_proc.p_pid == init_pid ? 0 : to_container(k->kp_eproc.e_ppid);
            k->kp_eproc.e_pgid = to_container(k->kp_eproc.e_pgid);
            k->kp_proc.p_pid = to_container(k->kp_proc.p_pid);
            kept++;
        }
    }
    return kept * sizeof *kp;
}

// list_pids writes to buf, of size bytes, the container's processes the
// process table's listing op of arg holds, those whose parent is ppid when
// ppid is set, and returns how many it wrote; with no buffer, how many
// there may be. libproc's listings build on it: the sandbox lets a
// container read the process table, not list another sandbox's pids.
static int list_pids(int op, int arg, pid_t ppid, pid_t *buf, int size) {
    int mib[4] = {CTL_KERN, KERN_PROC, op, arg};
    u_int miblen = op == KERN_PROC_ALL ? 3 : 4;
    size_t len = 0;
    if (sysctl(mib, miblen, NULL, &len, NULL, 0) != 0) {
        return -1;
    }
    // Room for processes started since.
    len += 32 * sizeof(struct kinfo_proc);
    if (!buf) {
        return (int)(len / sizeof(struct kinfo_proc));
    }
    struct kinfo_proc *kp = malloc(len);
    if (!kp) {
        errno = ENOMEM;
        return -1;
    }
    if (sysctl(mib, miblen, kp, &len, NULL, 0) != 0) {
        free(kp);
        return -1;
    }
    int n = 0, cap = size / (int)sizeof(pid_t);
    for (size_t i = 0; i < len / sizeof *kp && n < cap; i++) {
        if (ppid > 0 && kp[i].kp_eproc.e_ppid != to_host(ppid)) continue;
        if (visible(kp[i].kp_proc.p_pid, kp[i].kp_proc.p_comm)) buf[n++] = to_container(kp[i].kp_proc.p_pid);
    }
    free(kp);
    return n;
}

static int fws_proc_listpids(uint32_t type, uint32_t typeinfo, void *buf, int size) {
    int n;
    switch (type) {
    case PROC_ALL_PIDS:
        n = list_pids(KERN_PROC_ALL, 0, 0, buf, size);
        break;
    case PROC_PGRP_ONLY:
        n = list_pids(KERN_PROC_PGRP, to_host((pid_t)typeinfo), 0, buf, size);
        break;
    case PROC_TTY_ONLY:
        n = list_pids(KERN_PROC_TTY, (int)typeinfo, 0, buf, size);
        break;
    case PROC_UID_ONLY:
        n = list_pids(KERN_PROC_UID, (int)typeinfo, 0, buf, size);
        break;
    case PROC_RUID_ONLY:
        n = list_pids(KERN_PROC_RUID, (int)typeinfo, 0, buf, size);
        break;
    case PROC_PPID_ONLY:
        n = list_pids(KERN_PROC_ALL, 0, (pid_t)typeinfo, buf, size);
        break;
    default:
        return proc_listpids(type, typeinfo, buf, size);
    }
    return n < 0 ? 0 : n * (int)sizeof(pid_t);
}
INTERPOSE(fws_proc_listpids, proc_listpids);

static int fws_proc_listallpids(void *buf, int size) { return list_pids(KERN_PROC_ALL, 0, 0, buf, size); }
INTERPOSE(fws_proc_listallpids, proc_listallpids);

static int fws_proc_listpgrppids(pid_t pgid, void *buf, int size) { return list_pids(KERN_PROC_PGRP, to_host(pgid), 0, buf, size); }
INTERPOSE(fws_proc_listpgrppids, proc_listpgrppids);

static int fws_proc_listchildpids(pid_t ppid, void *buf, int size) { return list_pids(KERN_PROC_ALL, 0, ppid, buf, size); }
INTERPOSE(fws_proc_listchildpids, proc_listchildpids);

// Go and other programs read the hostname, and ps the process table,
// through sysctl.
static int fws_sysctl(int *name, u_int namelen, void *old, size_t *oldlen, void *new, size_t newlen) {
    if (namelen == 3 && name[0] == CTL_KERN && (name[1] == KERN_PROCARGS || name[1] == KERN_PROCARGS2) && init_pid) {
        int mib[3] = {name[0], name[1], to_host(name[2])};
        return sysctl(mib, 3, old, oldlen, new, newlen);
    }
    if (namelen >= 3 && name[0] == CTL_KERN && name[1] == KERN_PROC && old && oldlen && !new) {
        // A listing of one process, group or session names it as the
        // container does.
        int mib[CTL_MAXNAME];
        u_int n = namelen < CTL_MAXNAME ? namelen : CTL_MAXNAME;
        memcpy(mib, name, n * sizeof *mib);
        if (n == 4 && (mib[2] == KERN_PROC_PID || mib[2] == KERN_PROC_PGRP || mib[2] == KERN_PROC_SESSION)) {
            mib[3] = to_host(mib[3]);
        }
        int r = sysctl(mib, n, old, oldlen, new, newlen);
        if (r == 0) *oldlen = own_procs(old, *oldlen);
        return r;
    }
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
    if (strncmp(name, "kern.proc.", 10) == 0 && new == NULL) {
        int mib[CTL_MAXNAME];
        size_t n = CTL_MAXNAME;
        if (sysctlnametomib(name, mib, &n) == 0) {
            return fws_sysctl(mib, (u_int)n, old, oldlen, NULL, 0);
        }
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

// System V IPC. Seatbelt cannot tell System V keys apart, so under
// private IPC the library gives a container a key space of its own, as an
// IPC namespace does: a key becomes a hash of the container's IPC prefix
// and the key, so two containers using one key, as two PostgreSQL servers
// on one port do, get objects of their own. IPC_PRIVATE stays private.
static key_t sysv_key(key_t key) {
    const char *prefix = getenv("SANDBOX_IPC_PREFIX");
    if (key == IPC_PRIVATE || !prefix || !*prefix) {
        return key;
    }
    uint32_t h = 2166136261u;
    for (const unsigned char *c = (const unsigned char *)prefix; *c; c++) {
        h = (h ^ *c) * 16777619u;
    }
    for (int i = 0; i < 4; i++) {
        h = (h ^ ((uint32_t)key >> (8 * i) & 0xff)) * 16777619u;
    }
    h &= 0x7fffffff;
    return (key_t)(h ? h : 1);
}

static int fws_shmget(key_t key, size_t size, int flags) { return shmget(sysv_key(key), size, flags); }
INTERPOSE(fws_shmget, shmget);
static int fws_semget(key_t key, int n, int flags) { return semget(sysv_key(key), n, flags); }
INTERPOSE(fws_semget, semget);
static int fws_msgget(key_t key, int flags) { return msgget(sysv_key(key), flags); }
INTERPOSE(fws_msgget, msgget);

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
    if (join(out, POSIX_IPC_NAME_MAX + 1, slash, prefix, n)) {
        return out;
    }
    uint64_t h = 14695981039346656037ULL;
    for (const unsigned char *c = (const unsigned char *)n; *c; c++) {
        h = (h ^ *c) * 1099511628211ULL;
    }
    char hex[17];
    for (int i = 15; i >= 0; i--, h >>= 4) {
        hex[i] = "0123456789abcdef"[h & 15];
    }
    hex[16] = 0;
    join(out, POSIX_IPC_NAME_MAX + 1, slash, prefix, hex);
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

// Networking. The container's one interface is the host's loopback, where
// the sandbox lets it bind its ports' host ports alone, and Seatbelt matches
// loopback as 127.0.0.1 and ::1 but not as the IPv4-mapped ::ffff:127.0.0.1
// a dual-stack socket uses. So, as the container's network namespace would:
//
//   - a bind to the wildcard or loopback address on a container port binds
//     loopback on its host port, and getsockname reports what was bound;
//   - a connection to a loopback container port reaches its host port, and
//     getpeername reports the container port;
//   - an IPv6 socket bound or connected to an IPv4 address, or bound to the
//     dual-stack wildcard, becomes an IPv4 socket in the same descriptor,
//     whose addresses the program sees as IPv4-mapped IPv6 ones;
//   - an IPv6 socket connected to ::1 where nothing holds the port on IPv6
//     loopback reaches the dual-stack listener the library bound on IPv4,
//     as one IPv4 socket whose loopback addresses the program sees as ::1.

static int member_block(in_addr_t a);
static unsigned short member_host_port(in_addr_t addr, int type, unsigned short cport, int *member);
static unsigned short member_route(in_addr_t addr, int type, unsigned short cport, int *member, unsigned short *router);
static int member_of_host_port(int type, unsigned short hport, in_addr_t *addr, unsigned short *cport);

static unsigned short host_port(int type, unsigned short cport) {
    for (size_t i = 0; i < nport_maps; i++) {
        if (port_maps[i].type == type && port_maps[i].cport == cport) return port_maps[i].hport;
    }
    return 0;
}

static unsigned short container_port(int type, unsigned short hport) {
    for (size_t i = 0; i < nport_maps; i++) {
        if (port_maps[i].type == type && port_maps[i].hport == hport) return port_maps[i].cport;
    }
    return 0;
}

// What the library did to each descriptor: NET_V6 for an IPv6 socket it
// replaced with an IPv4 one, NET_ANY for one bound to the wildcard address.
#define NET_FDS 65536
#define NET_V6 1
#define NET_ANY 2
// NET_UNIX: a TCP socket on a port without a host port, which the library
// made a Unix one; NET_ROUTED: a connection through a router; NET_LO6: an
// IPv6 socket connected to ::1, whose IPv4 loopback reads as ::1.
#define NET_UNIX 4
#define NET_ROUTED 8
#define NET_LO6 16
// What the router puts before a datagram on a Unix one: the IPv4 address
// and port it came from, or goes to.
#define DGRAM_HEADER 6
// The most iovecs a datagram takes, one fewer than the kernel's UIO_MAXIOV
// for the header's.
#define DGRAM_IOV 1023
static unsigned char net_state[NET_FDS];
// For NET_UNIX and NET_ROUTED sockets: the container port, and for
// NET_ROUTED ones the peer's address.
static unsigned short net_port[NET_FDS];
static in_addr_t net_peer[NET_FDS];

static unsigned char state_of(int fd) { return fd >= 0 && fd < NET_FDS ? net_state[fd] : 0; }

static void set_state(int fd, unsigned char st) {
    if (fd >= 0 && fd < NET_FDS) net_state[fd] = st;
}

static int sock_type(int fd) {
    int t = 0;
    socklen_t n = sizeof t;
    return getsockopt(fd, SOL_SOCKET, SO_TYPE, &t, &n) == 0 ? t : 0;
}

static int v4_local(in_addr_t a) {
    uint32_t h = ntohl(a);
    return h == INADDR_ANY || (h >> 24) == 127;
}

static in_addr_t mapped_v4(const struct in6_addr *a) {
    in_addr_t v;
    memcpy(&v, &a->s6_addr[12], sizeof v);
    return v;
}

static struct sockaddr_in v4_addr(in_addr_t a, unsigned short port) {
    struct sockaddr_in s;
    memset(&s, 0, sizeof s);
    s.sin_len = sizeof s;
    s.sin_family = AF_INET;
    s.sin_port = htons(port);
    s.sin_addr.s_addr = a;
    return s;
}

// to_ipv4 puts an IPv4 socket of fd's type, with its settings, in its place.
static int to_ipv4(int fd, int type) {
    int s = socket(AF_INET, type, 0);
    if (s < 0) {
        return -1;
    }
    static const int ints[] = {SO_REUSEADDR, SO_REUSEPORT, SO_KEEPALIVE, SO_RCVBUF, SO_SNDBUF, SO_BROADCAST, SO_NOSIGPIPE, SO_OOBINLINE, SO_RCVLOWAT, SO_SNDLOWAT};
    for (size_t i = 0; i < sizeof ints / sizeof ints[0]; i++) {
        int v;
        socklen_t n = sizeof v;
        if (getsockopt(fd, SOL_SOCKET, ints[i], &v, &n) == 0) setsockopt(s, SOL_SOCKET, ints[i], &v, n);
    }
    struct linger l;
    socklen_t ln = sizeof l;
    if (getsockopt(fd, SOL_SOCKET, SO_LINGER, &l, &ln) == 0) setsockopt(s, SOL_SOCKET, SO_LINGER, &l, ln);
    static const int times[] = {SO_RCVTIMEO, SO_SNDTIMEO};
    for (size_t i = 0; i < 2; i++) {
        struct timeval tv;
        socklen_t n = sizeof tv;
        if (getsockopt(fd, SOL_SOCKET, times[i], &tv, &n) == 0) setsockopt(s, SOL_SOCKET, times[i], &tv, n);
    }
    if (type == SOCK_STREAM) {
        static const int tcp[] = {TCP_NODELAY, TCP_KEEPALIVE, TCP_KEEPINTVL, TCP_KEEPCNT};
        for (size_t i = 0; i < sizeof tcp / sizeof tcp[0]; i++) {
            int v;
            socklen_t n = sizeof v;
            if (getsockopt(fd, IPPROTO_TCP, tcp[i], &v, &n) == 0) setsockopt(s, IPPROTO_TCP, tcp[i], &v, n);
        }
    }
    int fl = fcntl(fd, F_GETFL);
    int fdfl = fcntl(fd, F_GETFD);
    if (fl != -1) fcntl(s, F_SETFL, fl);
    if (dup2(s, fd) == -1) {
        int e = errno;
        close(s);
        errno = e;
        return -1;
    }
    close(s);
    if (fdfl != -1) fcntl(fd, F_SETFD, fdfl);
    set_state(fd, NET_V6);
    return 0;
}

// local returns the address a bind to sa binds, in out, converting fd first
// where an IPv4 address goes through an IPv6 socket.
static const struct sockaddr *local(int fd, const struct sockaddr *sa, socklen_t *len, struct sockaddr_storage *out, unsigned char *any) {
    *any = 0;
    if (!sa) {
        return sa;
    }
    int type = sock_type(fd);
    if (sa->sa_family == AF_INET && *len >= (socklen_t)sizeof(struct sockaddr_in)) {
        const struct sockaddr_in *a = (const void *)sa;
        // The container's own member address is one of loopback's.
        in_addr_t at = own_address && a->sin_addr.s_addr == own_address ? htonl(INADDR_LOOPBACK) : a->sin_addr.s_addr;
        if (!v4_local(at)) {
            return sa;
        }
        unsigned short port = ntohs(a->sin_port), h = host_port(type, port);
        *any = at == htonl(INADDR_ANY);
        struct sockaddr_in b = v4_addr(*any ? htonl(INADDR_LOOPBACK) : at, h ? h : port);
        memcpy(out, &b, sizeof b);
        *len = sizeof b;
        return (const struct sockaddr *)out;
    }
    if (sa->sa_family == AF_INET6 && *len >= (socklen_t)sizeof(struct sockaddr_in6)) {
        const struct sockaddr_in6 *a = (const void *)sa;
        unsigned short port = ntohs(a->sin6_port), h = host_port(type, port);
        int wild = IN6_IS_ADDR_UNSPECIFIED(&a->sin6_addr);
        int v6only = 0;
        socklen_t n = sizeof v6only;
        getsockopt(fd, IPPROTO_IPV6, IPV6_V6ONLY, &v6only, &n);
        if (IN6_IS_ADDR_V4MAPPED(&a->sin6_addr) || (wild && !v6only)) {
            in_addr_t v4 = wild ? htonl(INADDR_ANY) : mapped_v4(&a->sin6_addr);
            if (own_address && v4 == own_address) v4 = htonl(INADDR_LOOPBACK);
            if (!v4_local(v4) || to_ipv4(fd, type) == -1) {
                return sa;
            }
            *any = v4 == htonl(INADDR_ANY);
            struct sockaddr_in b = v4_addr(*any ? htonl(INADDR_LOOPBACK) : v4, h ? h : port);
            memcpy(out, &b, sizeof b);
            *len = sizeof b;
            return (const struct sockaddr *)out;
        }
        if (wild || IN6_IS_ADDR_LOOPBACK(&a->sin6_addr)) {
            *any = wild;
            struct sockaddr_in6 b = *a;
            b.sin6_addr = in6addr_loopback;
            b.sin6_port = htons(h ? h : port);
            memcpy(out, &b, sizeof b);
            *len = sizeof b;
            return (const struct sockaddr *)out;
        }
    }
    return sa;
}

// A UDP port without a host port has a flow port, a loopback port of the
// stack's block where its container's router relays its datagrams. flows
// caches what routers answered, for the life of the process, as a router
// keeps each flow port while it runs; addr is the member the flow port
// presents as, loopback for the container's own.
#define MAX_FLOWS 256
static struct flow {
    unsigned short router, port, flow;
    in_addr_t addr;
    int ready;
} flows[MAX_FLOWS];
static int nflows;

// ask_router asks the router at loopback port router for UDP port port's
// flow port.
static unsigned short ask_router(unsigned short router, unsigned short port) {
    int s = socket(AF_INET, SOCK_STREAM, 0);
    if (s < 0) {
        return 0;
    }
    struct sockaddr_in r = v4_addr(htonl(INADDR_LOOPBACK), router);
    unsigned char req[4] = {0, 0, (unsigned char)(port >> 8), (unsigned char)port}, ans[3] = {1, 0, 0};
    int ok = connect(s, (struct sockaddr *)&r, sizeof r) == 0 && write(s, req, 4) == 4 && read(s, ans, 1) == 1 && ans[0] == 0 &&
             read(s, ans + 1, 2) == 2;
    close(s);
    return ok ? (unsigned short)(ans[1] << 8 | ans[2]) : 0;
}

static int port_path(int type, unsigned short port, struct sockaddr_un *u);

static unsigned short flow_port(unsigned short router, unsigned short port, in_addr_t addr) {
    int n = __atomic_load_n(&nflows, __ATOMIC_ACQUIRE);
    for (int i = 0; i < n && i < MAX_FLOWS; i++) {
        if (__atomic_load_n(&flows[i].ready, __ATOMIC_ACQUIRE) && flows[i].router == router && flows[i].port == port) {
            return flows[i].flow;
        }
    }
    unsigned short f = ask_router(router, port);
    if (!f) {
        return 0;
    }
    int i = __atomic_fetch_add(&nflows, 1, __ATOMIC_ACQ_REL);
    if (i < MAX_FLOWS) {
        flows[i].router = router;
        flows[i].port = port;
        flows[i].flow = f;
        flows[i].addr = addr;
        __atomic_store_n(&flows[i].ready, 1, __ATOMIC_RELEASE);
    }
    return f;
}

// flow_of reports the member address and port loopback port flow presents.
static int flow_of(unsigned short flow, in_addr_t *addr, unsigned short *port) {
    int n = __atomic_load_n(&nflows, __ATOMIC_ACQUIRE);
    for (int i = 0; i < n && i < MAX_FLOWS; i++) {
        if (__atomic_load_n(&flows[i].ready, __ATOMIC_ACQUIRE) && flows[i].flow == flow) {
            *addr = flows[i].addr;
            *port = flows[i].port;
            return 1;
        }
    }
    return 0;
}

// own_listener reports whether the container listens on port of type, a
// port without a host port, with a Unix socket.
static int own_listener(int type, unsigned short port) {
    struct sockaddr_un u;
    struct stat st;
    return port_dir[0] && port_path(type, port, &u) == 0 && stat(u.sun_path, &st) == 0 && S_ISSOCK(st.st_mode);
}

// reach finds where a connection or datagram of type to IPv4 address v4,
// port port, goes: a member's or the container's port reaches its host
// port, or a UDP port without one its flow port. It fails with errno set
// where nothing listens.
static int reach(int type, in_addr_t v4, unsigned short port, in_addr_t *a, unsigned short *p) {
    if (member_block(v4)) {
        // A member's port is its host port; one it does not expose is
        // closed, and an address no member holds is unreachable.
        int member;
        unsigned short router = 0, mh = member_route(v4, type, port, &member, &router);
        if (!mh && member && type == SOCK_DGRAM && router) mh = flow_port(router, port, v4);
        if (!mh) {
            errno = member ? ECONNREFUSED : EHOSTUNREACH;
            return -1;
        }
        *a = htonl(INADDR_LOOPBACK);
        *p = mh;
        return 0;
    }
    unsigned short h = v4_local(v4) ? host_port(type, port) : 0;
    if (!h && v4_local(v4) && type == SOCK_DGRAM && router_port && own_listener(SOCK_DGRAM, port)) {
        h = flow_port(router_port, port, htonl(INADDR_LOOPBACK));
    }
    *a = v4 == htonl(INADDR_ANY) ? htonl(INADDR_LOOPBACK) : v4;
    *p = h ? h : port;
    return 0;
}

// bound_v6 reports whether a socket of type holds port on IPv6 loopback, by
// trying to bind it, which, unlike a connection, no listener sees.
static int bound_v6(int type, unsigned short port) {
    int s = socket(AF_INET6, type, 0);
    if (s < 0) {
        return 1;
    }
    int on = 1;
    setsockopt(s, IPPROTO_IPV6, IPV6_V6ONLY, &on, sizeof on);
    struct sockaddr_in6 b;
    memset(&b, 0, sizeof b);
    b.sin6_len = sizeof b;
    b.sin6_family = AF_INET6;
    b.sin6_port = htons(port);
    b.sin6_addr = in6addr_loopback;
    int r = bind(s, (struct sockaddr *)&b, sizeof b), e = errno;
    close(s);
    return r == -1 && e == EADDRINUSE;
}

// remote returns the address a connection or datagram to sa reaches, in
// out, converting fd first where an IPv4 address goes through an IPv6
// socket, or NULL with errno set where nothing listens.
static const struct sockaddr *remote(int fd, const struct sockaddr *sa, socklen_t *len, struct sockaddr_storage *out) {
    if (!sa) {
        return sa;
    }
    int type = sock_type(fd);
    in_addr_t v4;
    unsigned short port;
    if (sa->sa_family == AF_INET && *len >= (socklen_t)sizeof(struct sockaddr_in)) {
        const struct sockaddr_in *a = (const void *)sa;
        v4 = a->sin_addr.s_addr;
        port = ntohs(a->sin_port);
    } else if (sa->sa_family == AF_INET6 && *len >= (socklen_t)sizeof(struct sockaddr_in6)) {
        const struct sockaddr_in6 *a = (const void *)sa;
        port = ntohs(a->sin6_port);
        if (IN6_IS_ADDR_V4MAPPED(&a->sin6_addr) || (state_of(fd) & NET_V6)) {
            // The IPv4 socket in an IPv6 one's place reaches ::1 as 127.0.0.1.
            v4 = IN6_IS_ADDR_V4MAPPED(&a->sin6_addr) ? mapped_v4(&a->sin6_addr)
                 : IN6_IS_ADDR_LOOPBACK(&a->sin6_addr) ? htonl(INADDR_LOOPBACK)
                                                       : 0;
            if (!v4 || (!(state_of(fd) & NET_V6) && to_ipv4(fd, type) == -1)) {
                return sa;
            }
        } else {
            unsigned short h = host_port(type, port);
            if (!h || !(IN6_IS_ADDR_LOOPBACK(&a->sin6_addr) || IN6_IS_ADDR_UNSPECIFIED(&a->sin6_addr))) {
                return sa;
            }
            if (bound_v6(type, h)) {
                struct sockaddr_in6 b = *a;
                b.sin6_addr = in6addr_loopback;
                b.sin6_port = htons(h);
                memcpy(out, &b, sizeof b);
                *len = sizeof b;
                return (const struct sockaddr *)out;
            }
            if (to_ipv4(fd, type) == -1) {
                return sa;
            }
            set_state(fd, state_of(fd) | NET_LO6);
            v4 = htonl(INADDR_LOOPBACK);
        }
    } else {
        return sa;
    }
    in_addr_t ra;
    unsigned short rp;
    if (reach(type, v4, port, &ra, &rp) == -1) {
        return NULL;
    }
    struct sockaddr_in b = v4_addr(ra, rp);
    memcpy(out, &b, sizeof b);
    *len = sizeof b;
    return (const struct sockaddr *)out;
}

// present writes the kernel's address k to the program's buffer as the
// program sees it: an IPv4-mapped IPv6 address on a socket it made for
// IPv6, a container port for a loopback host port, and, for a socket's own
// address, the wildcard it bound.
// present_inet writes address a, port port to the program's buffer, as
// IPv4 or, on a socket it made for IPv6, IPv4-mapped IPv6, with IPv4's
// wildcard as IPv6's.
static void present_inet(unsigned char st, in_addr_t a, unsigned short port, struct sockaddr *out, socklen_t *outlen) {
    struct sockaddr_storage v;
    socklen_t vn;
    memset(&v, 0, sizeof v);
    if (st & NET_V6) {
        struct sockaddr_in6 *b = (struct sockaddr_in6 *)&v;
        b->sin6_len = sizeof *b;
        b->sin6_family = AF_INET6;
        b->sin6_port = htons(port);
        if ((st & NET_LO6) && a == htonl(INADDR_LOOPBACK)) {
            b->sin6_addr = in6addr_loopback;
        } else if (a != htonl(INADDR_ANY)) {
            b->sin6_addr.s6_addr[10] = 0xff;
            b->sin6_addr.s6_addr[11] = 0xff;
            memcpy(&b->sin6_addr.s6_addr[12], &a, 4);
        }
        vn = sizeof *b;
    } else {
        struct sockaddr_in b4 = v4_addr(a, port);
        memcpy(&v, &b4, sizeof b4);
        vn = sizeof b4;
    }
    socklen_t n = *outlen < vn ? *outlen : vn;
    memcpy(out, &v, n);
    *outlen = vn;
}

static void present(int fd, const struct sockaddr_storage *k, socklen_t kn, struct sockaddr *out, socklen_t *outlen, int own) {
    if (!out || !outlen) {
        return;
    }
    unsigned char st = state_of(fd);
    if ((st & NET_UNIX) && k->ss_family == AF_UNIX) {
        // A listener on a port without a host port, or a connection it
        // accepted, whose clients came through a router from loopback.
        in_addr_t a = own ? ((st & NET_ANY) ? htonl(INADDR_ANY) : htonl(INADDR_LOOPBACK)) : htonl(INADDR_LOOPBACK);
        present_inet(st, a, own ? net_port[fd] : 0, out, outlen);
        return;
    }
    if ((st & NET_ROUTED) && !own) {
        present_inet(st, net_peer[fd], net_port[fd], out, outlen);
        return;
    }
    struct sockaddr_storage v;
    memcpy(&v, k, sizeof v);
    socklen_t vn = kn;
    int type = sock_type(fd);
    if (k->ss_family == AF_INET) {
        struct sockaddr_in *a = (struct sockaddr_in *)&v;
        unsigned short c = v4_local(a->sin_addr.s_addr) ? container_port(type, ntohs(a->sin_port)) : 0;
        in_addr_t ma;
        if (c) {
            a->sin_port = htons(c);
        } else if (!own && type == SOCK_DGRAM && a->sin_addr.s_addr == htonl(INADDR_LOOPBACK) && flow_of(ntohs(a->sin_port), &ma, &c)) {
            // A flow port is the UDP port of the member it relays for.
            a->sin_addr.s_addr = ma;
            a->sin_port = htons(c);
        } else if (!own && a->sin_addr.s_addr == htonl(INADDR_LOOPBACK) && member_of_host_port(type, ntohs(a->sin_port), &ma, &c)) {
            // A peer's host port is the member's own port at its address.
            a->sin_addr.s_addr = ma;
            a->sin_port = htons(c);
        }
        if (own && (st & NET_ANY)) a->sin_addr.s_addr = htonl(INADDR_ANY);
        if (st & NET_V6) {
            struct sockaddr_in6 b;
            memset(&b, 0, sizeof b);
            b.sin6_len = sizeof b;
            b.sin6_family = AF_INET6;
            b.sin6_port = a->sin_port;
            // IPv4's wildcard reads as IPv6's, not as ::ffff:0.0.0.0.
            if ((st & NET_LO6) && a->sin_addr.s_addr == htonl(INADDR_LOOPBACK)) {
                b.sin6_addr = in6addr_loopback;
            } else if (!(own && (st & NET_ANY)) && a->sin_addr.s_addr != htonl(INADDR_ANY)) {
                b.sin6_addr.s6_addr[10] = 0xff;
                b.sin6_addr.s6_addr[11] = 0xff;
                memcpy(&b.sin6_addr.s6_addr[12], &a->sin_addr.s_addr, 4);
            }
            memcpy(&v, &b, sizeof b);
            vn = sizeof b;
        }
    } else if (k->ss_family == AF_INET6) {
        struct sockaddr_in6 *a = (struct sockaddr_in6 *)&v;
        unsigned short c = IN6_IS_ADDR_LOOPBACK(&a->sin6_addr) ? container_port(type, ntohs(a->sin6_port)) : 0;
        if (c) a->sin6_port = htons(c);
        if (own && (st & NET_ANY)) a->sin6_addr = in6addr_any;
    }
    socklen_t n = *outlen < vn ? *outlen : vn;
    memcpy(out, &v, n);
    *outlen = vn;
}

static int fws_socket(int domain, int type, int protocol) {
    int fd = socket(domain, type, protocol);
    set_state(fd, 0);
    return fd;
}
INTERPOSE(fws_socket, socket);

// port_path writes where the library listens on port port of type, TCP or
// UDP.
static int port_path(int type, unsigned short port, struct sockaddr_un *u) {
    char digits[8], *d = digits + sizeof digits;
    *--d = 0;
    do {
        *--d = (char)('0' + port % 10);
        port /= 10;
    } while (port);
    memset(u, 0, sizeof *u);
    u->sun_family = AF_UNIX;
    if (!join(u->sun_path, sizeof u->sun_path, port_dir, type == SOCK_DGRAM ? "/udp-" : "/tcp-", d)) {
        errno = ENAMETOOLONG;
        return -1;
    }
    u->sun_len = (unsigned char)SUN_LEN(u);
    return 0;
}

// unbacked reports the TCP or UDP port sa names, on loopback, the wildcard
// or the container's own address, when it has no host port and the
// container can listen on it with a Unix socket; any is whether sa is the
// wildcard.
static unsigned short unbacked(int fd, const struct sockaddr *sa, socklen_t len, int *any, int *v6) {
    int type = sa && port_dir[0] ? sock_type(fd) : 0;
    if (type != SOCK_STREAM && type != SOCK_DGRAM) {
        return 0;
    }
    in_addr_t a;
    unsigned short port;
    *v6 = sa->sa_family == AF_INET6;
    if (sa->sa_family == AF_INET && len >= (socklen_t)sizeof(struct sockaddr_in)) {
        const struct sockaddr_in *s4 = (const void *)sa;
        a = s4->sin_addr.s_addr;
        port = ntohs(s4->sin_port);
    } else if (*v6 && len >= (socklen_t)sizeof(struct sockaddr_in6)) {
        const struct sockaddr_in6 *s6 = (const void *)sa;
        port = ntohs(s6->sin6_port);
        if (IN6_IS_ADDR_UNSPECIFIED(&s6->sin6_addr)) {
            a = htonl(INADDR_ANY);
        } else if (IN6_IS_ADDR_LOOPBACK(&s6->sin6_addr)) {
            a = htonl(INADDR_LOOPBACK);
        } else if (IN6_IS_ADDR_V4MAPPED(&s6->sin6_addr)) {
            a = mapped_v4(&s6->sin6_addr);
        } else {
            return 0;
        }
    } else {
        return 0;
    }
    if (own_address && a == own_address) a = htonl(INADDR_LOOPBACK);
    // A container port with a host port binds it, and so does a program
    // that binds the host port itself, as the container contract has it.
    if (!port || !v4_local(a) || host_port(type, port) || container_port(type, port)) {
        return 0;
    }
    *any = a == htonl(INADDR_ANY);
    return port;
}

// listen_unix puts a Unix socket of fd's type in its place and binds it
// where the library listens on port.
static int listen_unix(int fd, unsigned short port, int any, int v6) {
    int type = sock_type(fd);
    struct sockaddr_un u;
    if (port_path(type, port, &u) == -1) {
        return -1;
    }
    int s = socket(AF_UNIX, type, 0);
    if (s < 0) {
        return -1;
    }
    if (type == SOCK_DGRAM) {
        // A Unix datagram is bounded by its sender's buffer, and the router
        // sends UDP's largest with its header.
        int size = 4 * (65535 + DGRAM_HEADER);
        setsockopt(s, SOL_SOCKET, SO_RCVBUF, &size, sizeof size);
        setsockopt(s, SOL_SOCKET, SO_SNDBUF, &size, sizeof size);
    }
    int fl = fcntl(fd, F_GETFL), fdfl = fcntl(fd, F_GETFD);
    if (fl != -1) fcntl(s, F_SETFL, fl);
    if (dup2(s, fd) == -1) {
        int e = errno;
        close(s);
        errno = e;
        return -1;
    }
    close(s);
    if (fdfl != -1) fcntl(fd, F_SETFD, fdfl);
    set_state(fd, NET_UNIX | (v6 ? NET_V6 : 0) | (any ? NET_ANY : 0));
    if (fd < NET_FDS) net_port[fd] = port;
    int r = bind(fd, (struct sockaddr *)&u, u.sun_len);
    if (r == -1 && errno == EADDRINUSE) {
        // A socket no listener holds is a stale one, which a listener that
        // ended left behind.
        int t = socket(AF_UNIX, type, 0);
        int live = t >= 0 && connect(t, (struct sockaddr *)&u, u.sun_len) == 0;
        if (t >= 0) close(t);
        if (live) {
            errno = EADDRINUSE;
            return -1;
        }
        unlink(u.sun_path);
        r = bind(fd, (struct sockaddr *)&u, u.sun_len);
    }
    return r;
}

static int fws_bind(int fd, const struct sockaddr *sa, socklen_t len) {
    int uany, uv6;
    unsigned short uport = unbacked(fd, sa, len, &uany, &uv6);
    if (uport) {
        return listen_unix(fd, uport, uany, uv6);
    }
    struct sockaddr_storage b;
    unsigned char any;
    const struct sockaddr *to = local(fd, sa, &len, &b, &any);
    int r = bind(fd, to, len);
    if (r == 0 && any) set_state(fd, state_of(fd) | NET_ANY);
    return r;
}
INTERPOSE(fws_bind, bind);

// route connects fd through the router at loopback port router to the
// container port port of the member at addr, and reports whether it did:
// at once, as a connection to loopback is made, whether or not fd blocks.
static int route(int fd, in_addr_t addr, unsigned short router, unsigned short port, int v6) {
    if (v6 && !(state_of(fd) & NET_V6) && to_ipv4(fd, SOCK_STREAM) == -1) {
        return -1;
    }
    int fl = fcntl(fd, F_GETFL);
    if (fl != -1 && (fl & O_NONBLOCK)) fcntl(fd, F_SETFL, fl & ~O_NONBLOCK);
    struct sockaddr_in r = v4_addr(htonl(INADDR_LOOPBACK), router);
    unsigned char req[2] = {(unsigned char)(port >> 8), (unsigned char)port}, ans = 1;
    int ok = connect(fd, (struct sockaddr *)&r, sizeof r) == 0 && write(fd, req, 2) == 2 && read(fd, &ans, 1) == 1;
    int e = errno;
    if (fl != -1 && (fl & O_NONBLOCK)) fcntl(fd, F_SETFL, fl);
    if (!ok) {
        errno = e;
        return -1;
    }
    if (ans != 0) {
        errno = ECONNREFUSED;
        return -1;
    }
    set_state(fd, (state_of(fd) & NET_V6) | NET_ROUTED);
    if (fd < NET_FDS) {
        net_peer[fd] = addr;
        net_port[fd] = port;
    }
    return 0;
}

// routed connects fd to sa through a router when sa names a container port
// without a host port: its own container's, which it listens on, or a
// member's. It reports whether it did, with *r the result.
static int routed(int fd, const struct sockaddr *sa, socklen_t len, int *r) {
    if (!sa || sock_type(fd) != SOCK_STREAM) {
        return 0;
    }
    in_addr_t a;
    unsigned short port;
    int v6 = sa->sa_family == AF_INET6;
    if (sa->sa_family == AF_INET && len >= (socklen_t)sizeof(struct sockaddr_in)) {
        const struct sockaddr_in *s4 = (const void *)sa;
        a = s4->sin_addr.s_addr;
        port = ntohs(s4->sin_port);
    } else if (v6 && len >= (socklen_t)sizeof(struct sockaddr_in6)) {
        const struct sockaddr_in6 *s6 = (const void *)sa;
        port = ntohs(s6->sin6_port);
        if (IN6_IS_ADDR_V4MAPPED(&s6->sin6_addr)) {
            a = mapped_v4(&s6->sin6_addr);
        } else if (IN6_IS_ADDR_LOOPBACK(&s6->sin6_addr)) {
            a = htonl(INADDR_LOOPBACK);
        } else {
            return 0;
        }
    } else {
        return 0;
    }
    if (!port) {
        return 0;
    }
    if (member_block(a) && !(own_address && a == own_address)) {
        int member;
        unsigned short router = 0;
        if (member_route(a, SOCK_STREAM, port, &member, &router) || !member || !router) {
            return 0;
        }
        *r = route(fd, a, router, port, v6);
        return 1;
    }
    if ((v4_local(a) || (own_address && a == own_address)) && router_port && !host_port(SOCK_STREAM, port)) {
        // Loopback reaches the container's own listener when it has one;
        // any other loopback port is the host's.
        struct sockaddr_un u;
        struct stat st;
        if (port_path(SOCK_STREAM, port, &u) == -1 || stat(u.sun_path, &st) != 0 || !S_ISSOCK(st.st_mode)) {
            return 0;
        }
        *r = route(fd, own_address && a == own_address ? a : htonl(INADDR_LOOPBACK), router_port, port, v6);
        if (*r == 0 && v6 && IN6_IS_ADDR_LOOPBACK(&((const struct sockaddr_in6 *)sa)->sin6_addr)) {
            set_state(fd, state_of(fd) | NET_LO6);
        }
        return 1;
    }
    return 0;
}

// A UDP socket on a port without a host port sends each datagram with the
// router's header, which a connected one's send and write would leave off.
static int dgram_unix(int fd);

static int fws_connect(int fd, const struct sockaddr *sa, socklen_t len) {
    if (dgram_unix(fd)) {
        errno = EOPNOTSUPP;
        return -1;
    }
    int rr;
    if (routed(fd, sa, len, &rr)) {
        return rr;
    }
    struct sockaddr_storage b;
    const struct sockaddr *to = remote(fd, sa, &len, &b);
    if (sa && !to) {
        return -1;
    }
    return connect(fd, to, len);
}
INTERPOSE(fws_connect, connect);

extern int connect_nocancel(int, const struct sockaddr *, socklen_t) __asm("_connect$NOCANCEL") __attribute__((weak_import));
static int fws_connect_nocancel(int fd, const struct sockaddr *sa, socklen_t len) {
    if (dgram_unix(fd)) {
        errno = EOPNOTSUPP;
        return -1;
    }
    int rr;
    if (routed(fd, sa, len, &rr)) {
        return rr;
    }
    struct sockaddr_storage b;
    const struct sockaddr *to = remote(fd, sa, &len, &b);
    if (sa && !to) {
        return -1;
    }
    return connect_nocancel(fd, to, len);
}
INTERPOSE(fws_connect_nocancel, connect_nocancel);

// A UDP socket on a port without a host port is a Unix datagram one, whose
// datagrams the router relays with a header: the destination, as reach
// finds it, on the way out, and the source on the way in.
static int dgram_unix(int fd) { return (state_of(fd) & NET_UNIX) && sock_type(fd) == SOCK_DGRAM; }

typedef ssize_t (*sendmsg_fn)(int, const struct msghdr *, int);
typedef ssize_t (*recvmsg_fn)(int, struct msghdr *, int);

static ssize_t dgram_send(int fd, const struct iovec *iov, int iovlen, int flags, const struct sockaddr *sa, socklen_t len, sendmsg_fn send) {
    if (!sa) {
        errno = EDESTADDRREQ;
        return -1;
    }
    if (iovlen < 0 || iovlen > DGRAM_IOV) {
        errno = EMSGSIZE;
        return -1;
    }
    in_addr_t v4;
    unsigned short port;
    if (sa->sa_family == AF_INET && len >= (socklen_t)sizeof(struct sockaddr_in)) {
        const struct sockaddr_in *a = (const void *)sa;
        v4 = a->sin_addr.s_addr;
        port = ntohs(a->sin_port);
    } else if (sa->sa_family == AF_INET6 && len >= (socklen_t)sizeof(struct sockaddr_in6)) {
        const struct sockaddr_in6 *a = (const void *)sa;
        port = ntohs(a->sin6_port);
        if (IN6_IS_ADDR_V4MAPPED(&a->sin6_addr)) {
            v4 = mapped_v4(&a->sin6_addr);
        } else if (IN6_IS_ADDR_LOOPBACK(&a->sin6_addr) || IN6_IS_ADDR_UNSPECIFIED(&a->sin6_addr)) {
            v4 = htonl(INADDR_LOOPBACK);
        } else {
            errno = ENETUNREACH;
            return -1;
        }
    } else {
        errno = EAFNOSUPPORT;
        return -1;
    }
    in_addr_t ra;
    unsigned short rp;
    if (reach(SOCK_DGRAM, v4, port, &ra, &rp) == -1) {
        return -1;
    }
    unsigned char hdr[DGRAM_HEADER];
    memcpy(hdr, &ra, 4);
    hdr[4] = (unsigned char)(rp >> 8);
    hdr[5] = (unsigned char)rp;
    struct iovec v[DGRAM_IOV + 1];
    v[0].iov_base = hdr;
    v[0].iov_len = sizeof hdr;
    if (iovlen) memcpy(&v[1], iov, (size_t)iovlen * sizeof *iov);
    struct sockaddr_un u;
    memset(&u, 0, sizeof u);
    u.sun_family = AF_UNIX;
    if (!join(u.sun_path, sizeof u.sun_path, port_dir, "/udp", "")) {
        errno = ENAMETOOLONG;
        return -1;
    }
    u.sun_len = (unsigned char)SUN_LEN(&u);
    struct msghdr m;
    memset(&m, 0, sizeof m);
    m.msg_name = &u;
    m.msg_namelen = u.sun_len;
    m.msg_iov = v;
    m.msg_iovlen = iovlen + 1;
    ssize_t r = send(fd, &m, flags);
    return r < 0 ? r : r - (ssize_t)sizeof hdr;
}

static ssize_t dgram_recv(int fd, const struct iovec *iov, int iovlen, int flags, struct sockaddr *sa, socklen_t *len, int *mflags, recvmsg_fn recv) {
    if (iovlen < 0 || iovlen > DGRAM_IOV) {
        errno = EMSGSIZE;
        return -1;
    }
    unsigned char hdr[DGRAM_HEADER];
    struct iovec v[DGRAM_IOV + 1];
    v[0].iov_base = hdr;
    v[0].iov_len = sizeof hdr;
    if (iovlen) memcpy(&v[1], iov, (size_t)iovlen * sizeof *iov);
    struct msghdr m;
    for (;;) {
        memset(&m, 0, sizeof m);
        m.msg_iov = v;
        m.msg_iovlen = iovlen + 1;
        ssize_t r = recv(fd, &m, flags);
        if (r < 0) {
            return r;
        }
        // Only the router sends here, and always with its header.
        if (r < (ssize_t)sizeof hdr) {
            if (flags & MSG_PEEK) recv(fd, &m, flags & ~MSG_PEEK);
            continue;
        }
        if (mflags) *mflags = m.msg_flags & ~MSG_CTRUNC;
        if (sa && len) {
            in_addr_t a;
            memcpy(&a, hdr, 4);
            struct sockaddr_in k4 = v4_addr(a, (unsigned short)(hdr[4] << 8 | hdr[5]));
            struct sockaddr_storage k;
            memset(&k, 0, sizeof k);
            memcpy(&k, &k4, sizeof k4);
            present(fd, &k, sizeof k4, sa, len, 0);
        } else if (len) {
            *len = 0;
        }
        return r - (ssize_t)sizeof hdr;
    }
}

static ssize_t plain_sendmsg(int fd, const struct msghdr *m, int flags) { return sendmsg(fd, m, flags); }
static ssize_t plain_recvmsg(int fd, struct msghdr *m, int flags) { return recvmsg(fd, m, flags); }

static ssize_t fws_sendto(int fd, const void *buf, size_t n, int flags, const struct sockaddr *sa, socklen_t len) {
    if (dgram_unix(fd)) {
        struct iovec v = {(void *)buf, n};
        return dgram_send(fd, &v, 1, flags, sa, len, plain_sendmsg);
    }
    struct sockaddr_storage b;
    const struct sockaddr *to = remote(fd, sa, &len, &b);
    if (sa && !to) {
        return -1;
    }
    return sendto(fd, buf, n, flags, to, len);
}
INTERPOSE(fws_sendto, sendto);

static ssize_t fws_sendmsg(int fd, const struct msghdr *m, int flags) {
    if (m && dgram_unix(fd)) {
        return dgram_send(fd, m->msg_iov, m->msg_iovlen, flags, m->msg_name, m->msg_namelen, plain_sendmsg);
    }
    if (!m || !m->msg_name) {
        return sendmsg(fd, m, flags);
    }
    struct msghdr c = *m;
    struct sockaddr_storage b;
    socklen_t len = c.msg_namelen;
    c.msg_name = (void *)remote(fd, m->msg_name, &len, &b);
    if (!c.msg_name) {
        return -1;
    }
    c.msg_namelen = len;
    return sendmsg(fd, &c, flags);
}
INTERPOSE(fws_sendmsg, sendmsg);

static ssize_t fws_recvfrom(int fd, void *buf, size_t n, int flags, struct sockaddr *sa, socklen_t *len) {
    if (dgram_unix(fd)) {
        struct iovec v = {buf, n};
        return dgram_recv(fd, &v, 1, flags, sa, len, NULL, plain_recvmsg);
    }
    if (!sa || !len) {
        return recvfrom(fd, buf, n, flags, sa, len);
    }
    struct sockaddr_storage k;
    socklen_t kn = sizeof k;
    ssize_t r = recvfrom(fd, buf, n, flags, (struct sockaddr *)&k, &kn);
    if (r >= 0) {
        if (kn) {
            present(fd, &k, kn, sa, len, 0);
        } else {
            *len = 0;
        }
    }
    return r;
}
INTERPOSE(fws_recvfrom, recvfrom);

static ssize_t fws_recvmsg(int fd, struct msghdr *m, int flags) {
    if (m && dgram_unix(fd)) {
        m->msg_controllen = 0;
        return dgram_recv(fd, m->msg_iov, m->msg_iovlen, flags, m->msg_name, m->msg_name ? &m->msg_namelen : NULL, &m->msg_flags, plain_recvmsg);
    }
    if (!m || !m->msg_name) {
        return recvmsg(fd, m, flags);
    }
    void *name = m->msg_name;
    socklen_t cap = m->msg_namelen;
    struct sockaddr_storage k;
    m->msg_name = &k;
    m->msg_namelen = sizeof k;
    ssize_t r = recvmsg(fd, m, flags);
    socklen_t kn = m->msg_namelen;
    m->msg_name = name;
    m->msg_namelen = cap;
    if (r >= 0) {
        if (kn) {
            present(fd, &k, kn, name, &m->msg_namelen, 0);
        } else {
            m->msg_namelen = 0;
        }
    }
    return r;
}
INTERPOSE(fws_recvmsg, recvmsg);

static ssize_t fws_recv(int fd, void *buf, size_t n, int flags) {
    if (dgram_unix(fd)) {
        struct iovec v = {buf, n};
        return dgram_recv(fd, &v, 1, flags, NULL, NULL, NULL, plain_recvmsg);
    }
    return recv(fd, buf, n, flags);
}
INTERPOSE(fws_recv, recv);

static ssize_t fws_read(int fd, void *buf, size_t n) {
    if (dgram_unix(fd)) {
        struct iovec v = {buf, n};
        return dgram_recv(fd, &v, 1, 0, NULL, NULL, NULL, plain_recvmsg);
    }
    return read(fd, buf, n);
}
INTERPOSE(fws_read, read);

static int fws_accept(int fd, struct sockaddr *sa, socklen_t *len) {
    struct sockaddr_storage k;
    socklen_t kn = sizeof k;
    int c = accept(fd, (struct sockaddr *)&k, &kn);
    if (c >= 0) {
        set_state(c, state_of(fd) & (NET_V6 | NET_UNIX));
        if (c < NET_FDS && fd < NET_FDS) net_port[c] = net_port[fd];
        present(c, &k, kn, sa, len, 0);
    }
    return c;
}
INTERPOSE(fws_accept, accept);

static int fws_getsockname(int fd, struct sockaddr *sa, socklen_t *len) {
    struct sockaddr_storage k;
    socklen_t kn = sizeof k;
    int r = getsockname(fd, (struct sockaddr *)&k, &kn);
    if (r == 0) present(fd, &k, kn, sa, len, 1);
    return r;
}
INTERPOSE(fws_getsockname, getsockname);

static int fws_getpeername(int fd, struct sockaddr *sa, socklen_t *len) {
    struct sockaddr_storage k;
    socklen_t kn = sizeof k;
    int r = getpeername(fd, (struct sockaddr *)&k, &kn);
    if (r == 0) present(fd, &k, kn, sa, len, 0);
    return r;
}
INTERPOSE(fws_getpeername, getpeername);

// The IPv6 options of a socket the library made IPv4 are its IPv4 ones,
// and it stays dual-stack.
static int v4_option(int name) {
    switch (name) {
    case IPV6_TCLASS:
        return IP_TOS;
    case IPV6_UNICAST_HOPS:
        return IP_TTL;
    }
    return -1;
}

static int fws_setsockopt(int fd, int level, int name, const void *v, socklen_t n) {
    // A Unix stream has no IP or TCP options to set: it delays no segment
    // and has no packets to tune.
    if ((state_of(fd) & NET_UNIX) && (level == IPPROTO_TCP || level == IPPROTO_IP || level == IPPROTO_IPV6)) {
        return 0;
    }
    if (level == IPPROTO_IPV6 && (state_of(fd) & NET_V6)) {
        if (name == IPV6_V6ONLY) {
            int on = n >= (socklen_t)sizeof(int) ? *(const int *)v : 0;
            if (on) {
                errno = EINVAL;
                return -1;
            }
            return 0;
        }
        int o = v4_option(name);
        if (o != -1) return setsockopt(fd, IPPROTO_IP, o, v, n);
    }
    return setsockopt(fd, level, name, v, n);
}
INTERPOSE(fws_setsockopt, setsockopt);

static int fws_getsockopt(int fd, int level, int name, void *v, socklen_t *n) {
    if ((state_of(fd) & NET_UNIX) && (level == IPPROTO_TCP || level == IPPROTO_IP || level == IPPROTO_IPV6) && v && n &&
        *n >= (socklen_t)sizeof(int)) {
        *(int *)v = level == IPPROTO_TCP && name == TCP_NODELAY;
        *n = sizeof(int);
        return 0;
    }
    if (level == IPPROTO_IPV6 && (state_of(fd) & NET_V6)) {
        if (name == IPV6_V6ONLY && v && n && *n >= (socklen_t)sizeof(int)) {
            *(int *)v = 0;
            *n = sizeof(int);
            return 0;
        }
        int o = v4_option(name);
        if (o != -1) return getsockopt(fd, IPPROTO_IP, o, v, n);
    }
    return getsockopt(fd, level, name, v, n);
}
INTERPOSE(fws_getsockopt, getsockopt);

static void copy_state(int from, int to) {
    set_state(to, state_of(from));
    if (from >= 0 && from < NET_FDS && to >= 0 && to < NET_FDS) {
        net_port[to] = net_port[from];
        net_peer[to] = net_peer[from];
    }
}

static int fws_dup(int fd) {
    int c = dup(fd);
    copy_state(fd, c);
    return c;
}
INTERPOSE(fws_dup, dup);

static int fws_dup2(int fd, int to) {
    int c = dup2(fd, to);
    if (c >= 0) copy_state(fd, c);
    return c;
}
INTERPOSE(fws_dup2, dup2);

// The cancellation-point-free variants, which libSystem's own wrappers and
// some runtimes call.
extern ssize_t sendto_nocancel(int, const void *, size_t, int, const struct sockaddr *, socklen_t) __asm("_sendto$NOCANCEL") __attribute__((weak_import));
static ssize_t nocancel_sendmsg(int fd, const struct msghdr *m, int flags);
static ssize_t nocancel_recvmsg(int fd, struct msghdr *m, int flags);

static ssize_t fws_sendto_nocancel(int fd, const void *buf, size_t n, int flags, const struct sockaddr *sa, socklen_t len) {
    if (dgram_unix(fd)) {
        struct iovec v = {(void *)buf, n};
        return dgram_send(fd, &v, 1, flags, sa, len, nocancel_sendmsg);
    }
    struct sockaddr_storage b;
    const struct sockaddr *to = remote(fd, sa, &len, &b);
    if (sa && !to) {
        return -1;
    }
    return sendto_nocancel(fd, buf, n, flags, to, len);
}
INTERPOSE(fws_sendto_nocancel, sendto_nocancel);

extern ssize_t sendmsg_nocancel(int, const struct msghdr *, int) __asm("_sendmsg$NOCANCEL") __attribute__((weak_import));
static ssize_t nocancel_sendmsg(int fd, const struct msghdr *m, int flags) { return sendmsg_nocancel(fd, m, flags); }

static ssize_t fws_sendmsg_nocancel(int fd, const struct msghdr *m, int flags) {
    if (m && dgram_unix(fd)) {
        return dgram_send(fd, m->msg_iov, m->msg_iovlen, flags, m->msg_name, m->msg_namelen, nocancel_sendmsg);
    }
    if (!m || !m->msg_name) {
        return sendmsg_nocancel(fd, m, flags);
    }
    struct msghdr c = *m;
    struct sockaddr_storage b;
    socklen_t len = c.msg_namelen;
    c.msg_name = (void *)remote(fd, m->msg_name, &len, &b);
    if (!c.msg_name) {
        return -1;
    }
    c.msg_namelen = len;
    return sendmsg_nocancel(fd, &c, flags);
}
INTERPOSE(fws_sendmsg_nocancel, sendmsg_nocancel);

extern ssize_t recvfrom_nocancel(int, void *, size_t, int, struct sockaddr *, socklen_t *) __asm("_recvfrom$NOCANCEL") __attribute__((weak_import));
static ssize_t fws_recvfrom_nocancel(int fd, void *buf, size_t n, int flags, struct sockaddr *sa, socklen_t *len) {
    if (dgram_unix(fd)) {
        struct iovec v = {buf, n};
        return dgram_recv(fd, &v, 1, flags, sa, len, NULL, nocancel_recvmsg);
    }
    if (!sa || !len) {
        return recvfrom_nocancel(fd, buf, n, flags, sa, len);
    }
    struct sockaddr_storage k;
    socklen_t kn = sizeof k;
    ssize_t r = recvfrom_nocancel(fd, buf, n, flags, (struct sockaddr *)&k, &kn);
    if (r >= 0) {
        if (kn) {
            present(fd, &k, kn, sa, len, 0);
        } else {
            *len = 0;
        }
    }
    return r;
}
INTERPOSE(fws_recvfrom_nocancel, recvfrom_nocancel);

extern ssize_t recvmsg_nocancel(int, struct msghdr *, int) __asm("_recvmsg$NOCANCEL") __attribute__((weak_import));
static ssize_t nocancel_recvmsg(int fd, struct msghdr *m, int flags) { return recvmsg_nocancel(fd, m, flags); }

static ssize_t fws_recvmsg_nocancel(int fd, struct msghdr *m, int flags) {
    if (m && dgram_unix(fd)) {
        m->msg_controllen = 0;
        return dgram_recv(fd, m->msg_iov, m->msg_iovlen, flags, m->msg_name, m->msg_name ? &m->msg_namelen : NULL, &m->msg_flags, nocancel_recvmsg);
    }
    if (!m || !m->msg_name) {
        return recvmsg_nocancel(fd, m, flags);
    }
    void *name = m->msg_name;
    socklen_t cap = m->msg_namelen;
    struct sockaddr_storage k;
    m->msg_name = &k;
    m->msg_namelen = sizeof k;
    ssize_t r = recvmsg_nocancel(fd, m, flags);
    socklen_t kn = m->msg_namelen;
    m->msg_name = name;
    m->msg_namelen = cap;
    if (r >= 0) {
        if (kn) {
            present(fd, &k, kn, name, &m->msg_namelen, 0);
        } else {
            m->msg_namelen = 0;
        }
    }
    return r;
}
INTERPOSE(fws_recvmsg_nocancel, recvmsg_nocancel);

extern int accept_nocancel(int, struct sockaddr *, socklen_t *) __asm("_accept$NOCANCEL") __attribute__((weak_import));
static int fws_accept_nocancel(int fd, struct sockaddr *sa, socklen_t *len) {
    struct sockaddr_storage k;
    socklen_t kn = sizeof k;
    int c = accept_nocancel(fd, (struct sockaddr *)&k, &kn);
    if (c >= 0) {
        set_state(c, state_of(fd) & (NET_V6 | NET_UNIX));
        if (c < NET_FDS && fd < NET_FDS) net_port[c] = net_port[fd];
        present(c, &k, kn, sa, len, 0);
    }
    return c;
}
INTERPOSE(fws_accept_nocancel, accept_nocancel);

// Members.

static pthread_mutex_t members_lock = PTHREAD_MUTEX_INITIALIZER;
static char *members;
static struct stat members_st;

static int member_block(in_addr_t a) { return (ntohl(a) >> 16) == (127u << 8 | 88u); }

// members_text returns the network file's text, read again when it changed;
// members_lock is held.
static const char *members_text(void) {
    struct stat st;
    if (!network_path[0] || stat(network_path, &st) != 0) {
        return NULL;
    }
    if (members && st.st_ino == members_st.st_ino && st.st_size == members_st.st_size &&
        st.st_mtimespec.tv_sec == members_st.st_mtimespec.tv_sec && st.st_mtimespec.tv_nsec == members_st.st_mtimespec.tv_nsec) {
        return members;
    }
    int fd = open(network_path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) {
        return NULL;
    }
    char *b = malloc((size_t)st.st_size + 1);
    ssize_t n = b ? read(fd, b, (size_t)st.st_size) : -1;
    close(fd);
    if (n < 0) {
        free(b);
        return NULL;
    }
    b[n] = 0;
    free(members);
    members = b;
    members_st = st;
    return members;
}

struct member {
    in_addr_t addr;
    const char *names, *names_end, *ports, *ports_end;
    unsigned short router;
};

static const char *next_member(const char *p, struct member *m) {
    while (p && *p) {
        const char *nl = strchr(p, '\n');
        const char *end = nl ? nl : p + strlen(p);
        const char *t1 = memchr(p, '\t', (size_t)(end - p));
        const char *t2 = t1 ? memchr(t1 + 1, '\t', (size_t)(end - t1 - 1)) : NULL;
        const char *after;
        if (t2 && parse_v4(p, &after, &m->addr) && after == t1) {
            const char *t3 = memchr(t2 + 1, '\t', (size_t)(end - t2 - 1));
            unsigned long r = 0;
            m->names = t1 + 1;
            m->names_end = t2;
            m->ports = t2 + 1;
            m->ports_end = t3 ? t3 : end;
            m->router = t3 && num(t3 + 1, &r) && r < 65536 ? (unsigned short)r : 0;
            return nl ? nl + 1 : end;
        }
        p = nl ? nl + 1 : end;
    }
    return NULL;
}

static int lower(int c) { return c >= 'A' && c <= 'Z' ? c + 32 : c; }

// answers reports whether a member's space-separated names hold name, as
// DNS compares names: in any case, and with or without a final dot.
static int answers(const struct member *m, const char *name) {
    size_t n = strlen(name);
    if (n && name[n - 1] == '.') n--;
    for (const char *p = m->names; p < m->names_end;) {
        const char *e = memchr(p, ' ', (size_t)(m->names_end - p));
        if (!e) e = m->names_end;
        if ((size_t)(e - p) == n) {
            size_t i = 0;
            while (i < n && lower(p[i]) == lower(name[i])) i++;
            if (i == n) return 1;
        }
        p = e + 1;
    }
    return 0;
}

// member_ports finds in a member's ports the host port behind container
// port port, or with reverse set, the container port behind host port port.
static unsigned short member_ports(const struct member *m, int type, unsigned short port, int reverse) {
    for (const char *p = m->ports; p < m->ports_end;) {
        unsigned long c, h;
        const char *e = num(p, &c);
        int t = 0;
        if (e && strncmp(e, "/tcp=", 5) == 0) t = SOCK_STREAM;
        if (e && strncmp(e, "/udp=", 5) == 0) t = SOCK_DGRAM;
        if (!t || !(e = num(e + 5, &h))) return 0;
        if (t == type && (reverse ? h : c) == port) return (unsigned short)(reverse ? c : h);
        if (*e != ',') return 0;
        p = e + 1;
    }
    return 0;
}

static int member_address(const char *name, in_addr_t *out) {
    int found = 0;
    pthread_mutex_lock(&members_lock);
    struct member m;
    for (const char *p = members_text(); (p = next_member(p, &m));) {
        if (answers(&m, name)) {
            *out = m.addr;
            found = 1;
            break;
        }
    }
    pthread_mutex_unlock(&members_lock);
    return found;
}

// member_host_port returns the host port behind a member's container port,
// and whether addr is a member at all.
static unsigned short member_host_port(in_addr_t addr, int type, unsigned short cport, int *member) {
    return member_route(addr, type, cport, member, NULL);
}

// member_route returns the host port behind a member's container port, and
// whether addr is a member, and its router's port.
static unsigned short member_route(in_addr_t addr, int type, unsigned short cport, int *member, unsigned short *router) {
    unsigned short h = 0;
    *member = 0;
    pthread_mutex_lock(&members_lock);
    struct member m;
    for (const char *p = members_text(); (p = next_member(p, &m));) {
        if (m.addr == addr) {
            *member = 1;
            h = member_ports(&m, type, cport, 0);
            if (router) *router = m.router;
            break;
        }
    }
    pthread_mutex_unlock(&members_lock);
    return h;
}

static int member_of_host_port(int type, unsigned short hport, in_addr_t *addr, unsigned short *cport) {
    int found = 0;
    pthread_mutex_lock(&members_lock);
    struct member m;
    for (const char *p = members_text(); (p = next_member(p, &m));) {
        unsigned short c = member_ports(&m, type, hport, 1);
        if (c) {
            *addr = m.addr;
            *cport = c;
            found = 1;
            break;
        }
    }
    pthread_mutex_unlock(&members_lock);
    return found;
}

static struct addrinfo *member_info(in_addr_t a, int family, int type, unsigned short port) {
    size_t alen = family == AF_INET6 ? sizeof(struct sockaddr_in6) : sizeof(struct sockaddr_in);
    // freeaddrinfo frees each entry, its address and its canonical name.
    struct addrinfo *ai = calloc(1, sizeof *ai);
    struct sockaddr *sa = calloc(1, alen);
    if (!ai || !sa) {
        free(ai);
        free(sa);
        return NULL;
    }
    ai->ai_family = family;
    ai->ai_socktype = type;
    ai->ai_protocol = type == SOCK_DGRAM ? IPPROTO_UDP : IPPROTO_TCP;
    ai->ai_addrlen = (socklen_t)alen;
    ai->ai_addr = sa;
    if (family == AF_INET) {
        struct sockaddr_in s = v4_addr(a, 0);
        s.sin_port = port;
        memcpy(ai->ai_addr, &s, sizeof s);
    } else {
        struct sockaddr_in6 s;
        memset(&s, 0, sizeof s);
        s.sin6_len = sizeof s;
        s.sin6_family = AF_INET6;
        s.sin6_port = port;
        s.sin6_addr.s6_addr[10] = 0xff;
        s.sin6_addr.s6_addr[11] = 0xff;
        memcpy(&s.sin6_addr.s6_addr[12], &a, 4);
        memcpy(ai->ai_addr, &s, sizeof s);
    }
    return ai;
}

// A member's name resolves to its member address, as Docker's DNS resolves
// a container on the network; any other name to what the host resolves.
static int fws_getaddrinfo(const char *node, const char *service, const struct addrinfo *hints, struct addrinfo **res) {
    int flags = hints ? hints->ai_flags : 0;
    in_addr_t a;
    if (!node || (flags & AI_NUMERICHOST) || !member_address(node, &a)) {
        return getaddrinfo(node, service, hints, res);
    }
    int family = hints ? hints->ai_family : AF_UNSPEC;
    if (family == AF_UNSPEC) {
        family = AF_INET;
    } else if (family == AF_INET6) {
        // Members have IPv4 addresses alone, which an IPv6 caller takes
        // mapped only when it asks to.
        if (!(flags & AI_V4MAPPED)) return EAI_NONAME;
    } else if (family != AF_INET) {
        return EAI_FAMILY;
    }
    unsigned short port = 0;
    if (service && *service) {
        unsigned long v;
        const char *e = num(service, &v);
        if (e && !*e && v < 65536) {
            port = htons((unsigned short)v);
        } else if (flags & AI_NUMERICSERV) {
            return EAI_NONAME;
        } else {
            struct servent *se = getservbyname(service, NULL);
            if (!se) return EAI_SERVICE;
            port = (unsigned short)se->s_port;
        }
    }
    int types[2] = {SOCK_STREAM, SOCK_DGRAM}, ntypes = 2;
    if (hints && hints->ai_socktype) {
        types[0] = hints->ai_socktype;
        ntypes = 1;
    }
    struct addrinfo *head = NULL, **tail = &head;
    for (int i = 0; i < ntypes; i++) {
        struct addrinfo *ai = member_info(a, family, types[i], port);
        if (!ai) {
            freeaddrinfo(head);
            return EAI_MEMORY;
        }
        *tail = ai;
        tail = &ai->ai_next;
    }
    if (flags & AI_CANONNAME) {
        head->ai_canonname = strdup(node);
        if (!head->ai_canonname) {
            freeaddrinfo(head);
            return EAI_MEMORY;
        }
    }
    *res = head;
    return 0;
}
INTERPOSE(fws_getaddrinfo, getaddrinfo);

static struct hostent member_host;
static char member_host_name[256];
static char member_host_addr[4];
static char *member_host_addrs[2];
static char *member_host_aliases[1];

static struct hostent *member_hostent(const char *name, in_addr_t a) {
    strlcpy(member_host_name, name, sizeof member_host_name);
    memcpy(member_host_addr, &a, 4);
    member_host_addrs[0] = member_host_addr;
    member_host_addrs[1] = NULL;
    member_host_aliases[0] = NULL;
    member_host = (struct hostent){member_host_name, member_host_aliases, AF_INET, 4, member_host_addrs};
    return &member_host;
}

static struct hostent *fws_gethostbyname(const char *name) {
    in_addr_t a;
    return name && member_address(name, &a) ? member_hostent(name, a) : gethostbyname(name);
}
INTERPOSE(fws_gethostbyname, gethostbyname);

static struct hostent *fws_gethostbyname2(const char *name, int af) {
    in_addr_t a;
    return af == AF_INET && name && member_address(name, &a) ? member_hostent(name, a) : gethostbyname2(name, af);
}
INTERPOSE(fws_gethostbyname2, gethostbyname2);
