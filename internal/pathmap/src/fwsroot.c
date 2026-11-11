// fwsroot gives every process of a fwsd container, which the engine loads
// it into through DYLD_INSERT_LIBRARIES, the container's view where an
// unprivileged macOS process can have none of its own: as fakechroot and
// fakeroot do on Linux, it presents, without enforcing,
//
//   - the container's root at /: a path the root holds, or whose parent it
//     holds, so a file can be made there, names the root's; any other, such
//     as the system frameworks and devices, stays the host's (SANDBOX_ROOT);
//     symbolic links resolve in the container, as under chroot;
//   - the container's user and group (FWS_UID, FWS_GID), root by default,
//     and its supplementary groups (FWS_GROUPS, comma-separated):
//     chown succeeds and records the owner in an extended attribute, which
//     stat reports, and a file a user other than root makes is recorded as
//     theirs; files the container's processes made report root otherwise;
//   - the users and groups of the container's /etc/passwd and /etc/group;
//   - the container's hostname (FWS_HOSTNAME) and NIS domain name
//     (FWS_DOMAINNAME);
//   - its volumes and bind mounts at their destinations (FWS_MOUNTS: a
//     destination, a tab and the resolved source, a line each): the engine
//     places a mount as a symlink in the root, which a path through the
//     mount point follows, and which presents the source's own path;
//   - its private IPC namespace (SANDBOX_IPC_PREFIX): POSIX IPC and Mach
//     bootstrap names carry the container's prefix, and its POSIX shared
//     memory stays within the size of its /dev/shm (FWS_SHM: a ledger of
//     its objects' sizes, a tab and the size).
//
// The host still decides every access, by the engine's user's rights.
// IPV6_RECVPKTINFO and IPV6_PKTINFO name RFC 3542's options only with this.
#define __APPLE_USE_RFC_3542
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <poll.h>
#include <os/lock.h>
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
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/attr.h>
#include <sys/clonefile.h>
#include <sys/file.h>
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
#include <net/if.h>
#include <net/if_dl.h>
#include <net/if_types.h>
#include <net/route.h>
#include <ifaddrs.h>
#include <sys/ioctl.h>
#include <sys/sockio.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <arpa/inet.h>
#include <netdb.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/uio.h>
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
static gid_t fake_groups[NGROUPS_MAX];
static int fake_ngroups;
// FWS_GROUPS as a program the process starts gets it.
static char groups_var[sizeof "FWS_GROUPS=" + NGROUPS_MAX * 11];
static char hostname[256];
static char domainname[256];
// The host pid of the container's main process (FWS_INIT), its pid 1.
static pid_t init_pid;
// On the host's PID namespace (FWS_PIDNS=host) there is no pid 1 to
// present and every process is listed; root_tag, the variable the engine
// tags the container's processes with, tells the container's apart.
static int host_pids;
static char root_tag[PATH_MAX + 16];
// A PID namespace's file (FWS_PIDNSFILE), which lists the containers that
// share it, and the socket of the container's shim (FWS_PROCS), which
// lists, signals and reads the processes of the others' sandboxes, which
// Seatbelt keeps from this one (package pidns).
static char pidns_file[PATH_MAX];
static char procs_path[104];
// The engine's helper binary's name (FWS_HELPER), which the container's
// exec anchor runs as.
static char helper_comm[MAXCOMLEN + 1];

// The file of the container's pids limit (FWS_PIDS), as a pids cgroup's
// pids.max holds it: a number, or "max" for none. The engine rewrites it
// as the limit changes.
static char pids_path[PATH_MAX];

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
// answers to, its ports as FWS_PORTS lists them, its router's port, the
// networks it is on, whether it is another stack's, that stack, the
// aliases links give it, and its addresses on networks with subnets of
// their own, tab-separated (package members). Its addresses lie in
// 127.88.0.0/16 and those subnets, which the kernel never sees: the
// library reaches a member's ports through their host ports, and another
// stack's member through the container's own router. The container sees
// the members it shares a network with, at their addresses on the network.
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
static const char *const kept_names[] = {"DYLD_INSERT_LIBRARIES", "SANDBOX_ROOT", "SANDBOX_IPC_PREFIX", "FWS_MOUNTS", "FWS_HOSTNAME", "FWS_DOMAINNAME", "FWS_SHM", "FWS_PORTS", "FWS_NETWORK", "FWS_ADDRESS", "FWS_HELPER", "FWS_INIT", "FWS_PORTDIR", "FWS_ROUTER", "FWS_PIDS", "FWS_PIDNS", "FWS_PIDNSFILE", "FWS_PROCS", NULL};
static char *kept[sizeof kept_names / sizeof kept_names[0]];

static void set_groups_var(void) {
    size_t o = (size_t)snprintf(groups_var, sizeof groups_var, "FWS_GROUPS=");
    for (int i = 0; i < fake_ngroups && o < sizeof groups_var; i++) {
        o += (size_t)snprintf(groups_var + o, sizeof groups_var - o, i ? ",%u" : "%u", fake_groups[i]);
    }
}

static void parse_shm(const char *v);

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
    const char *gs = getenv("FWS_GROUPS");
    if (gs) {
        unsigned long g;
        while (*gs && fake_ngroups < NGROUPS_MAX && (gs = num(gs, &g)) != NULL) {
            fake_groups[fake_ngroups++] = (gid_t)g;
            if (*gs != ',') break;
            gs++;
        }
    } else {
        fake_groups[fake_ngroups++] = fake_gid;
    }
    set_groups_var();
    const char *ns = getenv("FWS_PIDNS");
    host_pids = ns && strcmp(ns, "host") == 0;
    // On another container's PID namespace, FWS_PIDNS is its pid 1.
    unsigned long nsinit;
    const char *nse;
    if (host_pids) {
        init_pid = 0;
    } else if (ns && (nse = num(ns, &nsinit)) && !*nse && nsinit > 0) {
        init_pid = (pid_t)nsinit;
    } else {
        init_pid = (pid_t)env_number("FWS_INIT");
    }
    const char *nf = getenv("FWS_PIDNSFILE");
    if (nf && nf[0] == '/') {
        strlcpy(pidns_file, nf, sizeof pidns_file);
    }
    const char *pp = getenv("FWS_PROCS");
    if (pp && pp[0] == '/' && strlen(pp) < sizeof procs_path) {
        strcpy(procs_path, pp);
    }
    const char *pm = getenv("FWS_PIDS");
    if (pm && pm[0] == '/') {
        strlcpy(pids_path, pm, sizeof pids_path);
    }
    const char *hc = getenv("FWS_HELPER");
    if (hc) {
        strlcpy(helper_comm, hc, sizeof helper_comm);
    }
    const char *h = getenv("FWS_HOSTNAME");
    if (h) {
        strlcpy(hostname, h, sizeof hostname);
    }
    const char *d = getenv("FWS_DOMAINNAME");
    if (d) {
        strlcpy(domainname, d, sizeof domainname);
    }
    parse_mounts(getenv("FWS_MOUNTS"));
    parse_shm(getenv("FWS_SHM"));
    parse_ports(getenv("FWS_PORTS"));
    parse_network(getenv("FWS_NETWORK"), getenv("FWS_ADDRESS"));
    parse_routing(getenv("FWS_PORTDIR"), getenv("FWS_ROUTER"));
    const char *r = getenv("SANDBOX_ROOT");
    if (r && r[0] == '/' && strlen(r) < sizeof root) {
        snprintf(root_tag, sizeof root_tag, "SANDBOX_ROOT=%s", r);
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
    // The parent is what comes before the last component, past any
    // trailing slashes: mkdir("/a/b/") makes b in /a, as the kernel does.
    char *end = buf + rootlen + n;
    while (end > buf + rootlen + 1 && end[-1] == '/') end--;
    char *slash = end - 1;
    while (slash > buf + rootlen && *slash != '/') slash--;
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
    if (!strstr(p, "/..")) {
        // One lookup settles the common case, a file the container holds
        // with no link on its way, which place would otherwise look up
        // again after linked.
        const char *q = host_of(p, buf);
        if (!q) {
            return p;
        }
        char c[PATH_MAX];
        const char *probe = in_root(q) && join(c, sizeof c, croot, q + rootlen, "") ? c : q;
        struct stat st;
        if (fstatat(AT_FDCWD, probe, &st, AT_SYMLINK_NOFOLLOW_ANY) == 0) {
            if (!S_ISLNK(st.st_mode)) {
                return q;
            }
        } else if (errno != ELOOP) {
            return place(p, buf);
        }
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

// direct writes to buf container path p's host path in the root, for a
// call that the kernel then resolves refusing any link on its way: the
// common case, a file the container holds, costs no lookup of the
// library's. It reports 0 for a path the mapping must handle: relative,
// on the host's side, on a mount, in a host tree, or through "..".
static int direct(const char *p, char *buf) {
    if (!rootlen || !p || p[0] != '/' || host_side(p) || in_host_tree(p) || strstr(p, "/..")) {
        return 0;
    }
    for (size_t i = 0; i < nmounts; i++) {
        if (under(p, mounts[i].dest, mounts[i].destlen)) {
            return 0;
        }
    }
    size_t cl = strlen(croot), n = strlen(p);
    if (cl + n + 1 > PATH_MAX) {
        return 0;
    }
    memcpy(buf, croot, cl);
    memcpy(buf + cl, p, n + 1);
    return 1;
}

// rooted is container path p's host path under the root as the library
// names it, which own matches, given d, its path under the resolved root.
static const char *rooted(const char *p, char *d) {
    if (strcmp(root, croot) != 0) {
        memcpy(d, root, rootlen);
        strcpy(d + rootlen, p);
    }
    return d;
}

// missing settles a direct call that found no file at host path d: the
// container's answer is ENOENT where the root holds d's parent, as the
// mapping finds. It reports 0 where the mapping must decide, as the host
// may hold the path.
static int missing(char *d) {
    char *slash = strrchr(d, '/');
    if (!slash) {
        return 0;
    }
    struct stat st;
    *slash = 0;
    int parent = lstat(d, &st) == 0;
    *slash = '/';
    if (!parent) {
        return 0;
    }
    errno = ENOENT;
    return 1;
}

static void own(struct stat *st, const char *path, int fd, int nofollow);

// settle_stat makes a stat, or with nofollow an lstat, of container path
// path directly where it can, and reports whether it did, with *r its
// result.
static int settle_stat(const char *path, struct stat *st, int nofollow, int *r) {
    char d[PATH_MAX];
    if (!direct(path, d)) {
        return 0;
    }
    if (fstatat(AT_FDCWD, d, st, AT_SYMLINK_NOFOLLOW_ANY) == 0) {
        // A link the final component names is lstat's answer, and stat's
        // to resolve in the container.
        if (S_ISLNK(st->st_mode) && !nofollow) {
            return 0;
        }
        own(st, rooted(path, d), -1, nofollow);
        *r = 0;
        return 1;
    }
    if (errno == ENOENT && missing(d)) {
        *r = -1;
        return 1;
    }
    return 0;
}

// settle_open opens container path path directly where it can, for an
// open that follows links and makes no file, and reports whether it did,
// with *fd the result. nocancel opens without a cancellation point.
static int settle_open(const char *path, int flags, int nocancel, int *fd);

static int fws_open(const char *path, int flags, ...) {
    mode_t mode = 0;
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode = (mode_t)va_arg(ap, int);
        va_end(ap);
    }
    int sfd;
    if (settle_open(path, flags, 0, &sfd)) return sfd;
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
    int sfd;
    if (settle_open(path, flags, 1, &sfd)) return sfd;
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
    int sfd;
    if (settle_open(path, flags, 0, &sfd)) return sfd;
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
static int settle_open(const char *path, int flags, int nocancel, int *fd) {
    char d[PATH_MAX];
    if ((flags & O_CREAT) || !follows(flags) || !direct(path, d)) {
        return 0;
    }
    int r = nocancel ? openat_nocancel(AT_FDCWD, d, flags | O_NOFOLLOW_ANY) : openat(AT_FDCWD, d, flags | O_NOFOLLOW_ANY);
    if (r >= 0) {
        *fd = r;
        return 1;
    }
    if (errno == ENOENT && missing(d)) {
        *fd = -1;
        return 1;
    }
    return 0;
}
static int fws_openat_nocancel(int fd, const char *path, int flags, ...) {
    mode_t mode = 0;
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode = (mode_t)va_arg(ap, int);
        va_end(ap);
    }
    int sfd;
    if (settle_open(path, flags, 1, &sfd)) return sfd;
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
    int sr;
    if (settle_stat(path, st, 0, &sr)) return sr;
    const char *p = MAP(path);
    int r = stat(p, st);
    if (r == 0) own(st, p, -1, 0);
    return r;
}
INTERPOSE(fws_stat, stat);
static int fws_lstat(const char *path, struct stat *st) {
    int sr;
    if (settle_stat(path, st, 1, &sr)) return sr;
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
    int sr;
    if ((flag & ~AT_SYMLINK_NOFOLLOW) == 0 && settle_stat(path, st, flag & AT_SYMLINK_NOFOLLOW, &sr)) return sr;
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
    int sr;
    if (settle_stat(path, st, 0, &sr)) return sr;
    const char *p = MAP(path);
    int r = stat64(p, st);
    if (r == 0) own(st, p, -1, 0);
    return r;
}
INTERPOSE(fws_stat64, stat64);
static int fws_lstat64(const char *path, void *st) {
    int sr;
    if (settle_stat(path, st, 1, &sr)) return sr;
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
    int sr;
    if ((flag & ~AT_SYMLINK_NOFOLLOW) == 0 && settle_stat(path, st, flag & AT_SYMLINK_NOFOLLOW, &sr)) return sr;
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
// A chmod sets the mode on disk, which then stands for the one extraction
// recorded (MODE_XATTR).
#define MODE_XATTR "com.fws.mode"

// META_FLAG marks a file that holds an owner or a mode of the container's
// in an extended attribute, so that stat reads the attributes only for
// such a file: stat returns the flag at no cost, where reading an absent
// attribute costs a call of its own. The image store sets it beside the
// mode it records; the library hides it from the container and keeps it
// through the container's own chflags.
#define META_FLAG UF_NODUMP

// mark sets META_FLAG on a file, by descriptor when fd is set.
static void mark(const char *path, int fd, int nofollow) {
    struct stat st;
    int r = fd >= 0 ? fstat(fd, &st) : nofollow ? lstat(path, &st) : stat(path, &st);
    if (r != 0 || (st.st_flags & META_FLAG)) {
        return;
    }
    if (fd >= 0) {
        fchflags(fd, st.st_flags | META_FLAG);
    } else if (nofollow) {
        lchflags(path, st.st_flags | META_FLAG);
    } else {
        chflags(path, st.st_flags | META_FLAG);
    }
}
static int fws_chmod(const char *path, mode_t mode) {
    const char *p = MAP(path);
    int r = chmod(p, mode);
    if (r == 0) removexattr(p, MODE_XATTR, 0);
    return r;
}
INTERPOSE(fws_chmod, chmod);
static int fws_fchmodat(int fd, const char *path, mode_t mode, int flag) {
    const char *p = AT_MAPF(fd, path, !(flag & AT_SYMLINK_NOFOLLOW));
    int r = fchmodat(fd, p, mode, flag);
    if (r == 0) {
        int saved = errno;
        int f = openat(fd, p, O_RDONLY | O_CLOEXEC | ((flag & AT_SYMLINK_NOFOLLOW) ? O_SYMLINK : 0));
        if (f >= 0) {
            fremovexattr(f, MODE_XATTR, 0);
            close(f);
        }
        errno = saved;
    }
    return r;
}
INTERPOSE(fws_fchmodat, fchmodat);
static int fws_fchmod(int fd, mode_t mode) {
    int r = fchmod(fd, mode);
    if (r == 0) fremovexattr(fd, MODE_XATTR, 0);
    return r;
}
INTERPOSE(fws_fchmod, fchmod);
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
// keep_mark is flags with the file's own META_FLAG, so a container's
// chflags leaves the library's mark.
static unsigned int keep_mark(int r, const struct stat *st, unsigned int flags) {
    return r == 0 && st->st_uid == getuid_real() ? (flags & ~(unsigned int)META_FLAG) | (st->st_flags & META_FLAG) : flags;
}
static int fws_chflags(const char *path, unsigned int flags) {
    const char *p = MAP(path);
    struct stat st;
    return chflags(p, keep_mark(stat(p, &st), &st, flags));
}
INTERPOSE(fws_chflags, chflags);
static int fws_lchflags(const char *path, unsigned int flags) {
    const char *p = LMAP(path);
    struct stat st;
    return lchflags(p, keep_mark(lstat(p, &st), &st, flags));
}
INTERPOSE(fws_lchflags, lchflags);
static int fws_fchflags(int fd, unsigned int flags) {
    struct stat st;
    return fchflags(fd, keep_mark(fstat(fd, &st), &st, flags));
}
INTERPOSE(fws_fchflags, fchflags);
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
    char **out = calloc(n + 4 + sizeof kept_names / sizeof kept_names[0], sizeof *out);
    if (!out) return NULL;
    size_t j = 0;
    for (size_t i = 0; i < n; i++) {
        if (strncmp(envp[i], "FWS_UID=", 8) == 0 || strncmp(envp[i], "FWS_GID=", 8) == 0 ||
            strncmp(envp[i], "FWS_GROUPS=", 11) == 0) continue;
        // A program that empties the library's variable, as ClickHouse
        // does before it executes itself again, gets it back: a container
        // process cannot leave the container's view.
        if (strcmp(envp[i], "DYLD_INSERT_LIBRARIES=") == 0) continue;
        out[j++] = envp[i];
    }
    int a = snprintf(ids, 64, "FWS_UID=%u", fake_euid);
    snprintf(ids + a + 1, 64 - a - 1, "FWS_GID=%u", fake_egid);
    out[j++] = ids;
    out[j++] = ids + a + 1;
    out[j++] = groups_var;
    // out holds what the program passes, without what it emptied, and
    // ends with a NULL, as calloc left it.
    for (size_t i = 0; kept_names[i]; i++) {
        if (kept[i] && !has_var(out, kept[i])) out[j++] = kept[i];
    }
    return out;
}

// A container's processes do not see the variable that loads the library,
// which no container's environment holds.
static char *fws_getenv(const char *name) {
    if (strcmp(name, "DYLD_INSERT_LIBRARIES") == 0) return NULL;
    return getenv(name);
}
INTERPOSE(fws_getenv, getenv);

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
static int pids_full(void);

// spawns reports whether a posix_spawn with attributes a starts a process,
// rather than replacing the caller's program, as POSIX_SPAWN_SETEXEC does.
static int spawns(const posix_spawnattr_t *a) {
    short flags = 0;
    return !(a && posix_spawnattr_getflags(a, &flags) == 0 && (flags & POSIX_SPAWN_SETEXEC));
}

static int fws_posix_spawn(pid_t *pid, const char *path, const posix_spawn_file_actions_t *fa, const posix_spawnattr_t *a,
                           char *const argv[], char *const envp[]) {
    if (spawns(a) && pids_full()) {
        return EAGAIN;
    }
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
    if (spawns(a) && pids_full()) {
        return EAGAIN;
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
        return fake_ngroups;
    }
    if (n < fake_ngroups) {
        errno = EINVAL;
        return -1;
    }
    memcpy(list, fake_groups, fake_ngroups * sizeof *list);
    return fake_ngroups;
}
// Programs built with _DARWIN_C_SOURCE, such as Python, call getgroups as
// getgroups$DARWIN_EXTSN, which answers the user's group access list from
// the host's directory; others, such as Go's runtime, by its own name.
extern int getgroups_extsn(int, gid_t[]) __asm("_getgroups$DARWIN_EXTSN");
INTERPOSE(fws_getgroups, getgroups);
static int fws_getgroups_extsn(int n, gid_t *list) { return fws_getgroups(n, list); }
INTERPOSE(fws_getgroups_extsn, getgroups_extsn);

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
    if (fake_euid != 0) {
        errno = EPERM;
        return -1;
    }
    if (n < 0 || n > NGROUPS_MAX) {
        errno = EINVAL;
        return -1;
    }
    memcpy(fake_groups, list, n * sizeof *list);
    fake_ngroups = n;
    set_groups_var();
    setenv("FWS_GROUPS", groups_var + sizeof "FWS_GROUPS=" - 1, 1);
    return 0;
}
INTERPOSE(fws_setgroups, setgroups);
// initgroups gives the process gid and the groups of the container's
// /etc/group that list name as a member.
static int fws_initgroups(const char *name, int gid) {
    if (fake_euid != 0) {
        errno = EPERM;
        return -1;
    }
    gid_t list[NGROUPS_MAX];
    int n = 0;
    list[n++] = (gid_t)gid;
    char path[PATH_MAX];
    snprintf(path, sizeof path, "%s/etc/group", root);
    FILE *f = rootlen ? fopen(path, "r") : NULL;
    char line[4096];
    while (f && n < NGROUPS_MAX && fgets(line, sizeof line, f)) {
        char *fields[4];
        char *s = line;
        int k = 0;
        line[strcspn(line, "\n")] = 0;
        while (k < 4 && (fields[k] = strsep(&s, ":")) != NULL) {
            k++;
        }
        if (k < 4 || fields[0][0] == '#') {
            continue;
        }
        gid_t g = (gid_t)strtoul(fields[2], NULL, 10);
        char *member;
        while ((member = strsep(&fields[3], ",")) != NULL) {
            if (strcmp(member, name) == 0) {
                int seen = 0;
                for (int i = 0; i < n; i++) seen |= list[i] == g;
                if (!seen) list[n++] = g;
                break;
            }
        }
    }
    if (f) fclose(f);
    return fws_setgroups(n, list);
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
    int r = fd >= 0 ? fsetxattr(fd, OWNER_XATTR, v, (size_t)n, 0, 0)
                    : setxattr(path, OWNER_XATTR, v, (size_t)n, 0, nofollow ? XATTR_NOFOLLOW : 0);
    if (r == 0) {
        mark(path, fd, nofollow);
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
    if (!(st->st_flags & META_FLAG)) {
        st->st_uid = 0;
        st->st_gid = 0;
        return;
    }
    st->st_flags &= ~(uint32_t)META_FLAG;
    if (read_owner(path, fd, nofollow, &u, &g)) {
        st->st_uid = u;
        st->st_gid = g;
    } else {
        st->st_uid = 0;
        st->st_gid = 0;
    }
    // The mode the file's image gave it, where extraction kept the owner's
    // read and write, or search, on disk.
    char v[16];
    ssize_t n = fd >= 0 ? fgetxattr(fd, MODE_XATTR, v, sizeof v - 1, 0, 0)
                        : getxattr(path, MODE_XATTR, v, sizeof v - 1, 0, nofollow ? XATTR_NOFOLLOW : 0);
    if (n > 0) {
        v[n] = 0;
        mode_t m = 0;
        for (ssize_t i = 0; i < n && v[i] >= '0' && v[i] <= '7'; i++) m = (mode_t)(m * 8 + (mode_t)(v[i] - '0'));
        st->st_mode = (st->st_mode & ~(mode_t)07777) | (m & 07777);
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

// A container has none of the host user's directories: confstr names the
// container's /tmp for each, where a Linux program would put its files.
static size_t fws_confstr(int name, char *buf, size_t len) {
    if (name == _CS_DARWIN_USER_DIR || name == _CS_DARWIN_USER_TEMP_DIR || name == _CS_DARWIN_USER_CACHE_DIR) {
        static const char dir[] = "/tmp/";
        if (buf && len) {
            strlcpy(buf, dir, len);
        }
        return sizeof dir;
    }
    return confstr(name, buf, len);
}
INTERPOSE(fws_confstr, confstr);

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
static int fws_getdomainname(char *name, int len) {
    if (!hostname[0]) {
        return getdomainname(name, len);
    }
    if (len < 0 || strlen(domainname) + 1 > (size_t)len) {
        errno = EINVAL;
        return -1;
    }
    strcpy(name, domainname);
    return 0;
}
INTERPOSE(fws_getdomainname, getdomainname);
static int fws_uname(struct utsname *u) {
    int r = uname(u);
    if (r == 0 && hostname[0]) {
        strlcpy(u->nodename, hostname, sizeof u->nodename);
    }
    return r;
}
INTERPOSE(fws_uname, uname);

// The shim's operations (package pidns).
enum { NS_LIST = 1, NS_KILL = 2, NS_INFO = 3, NS_PATH = 4, NS_ARGS = 5 };

struct ns_request {
    int32_t op, pid, a, size;
    uint64_t b;
};

// ns_shared reports whether other containers share the PID namespace: its
// file lists more containers than this one.
static int ns_shared(void) {
    if (host_pids || !pidns_file[0] || !procs_path[0]) return 0;
    int fd = open(pidns_file, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return 0;
    char b[4096];
    int lines = 0;
    ssize_t n;
    while (lines < 2 && (n = read(fd, b, sizeof b)) > 0) {
        for (ssize_t i = 0; i < n; i++) lines += b[i] == '\n';
    }
    close(fd);
    return lines > 1;
}

static int io_all(int fd, void *b, size_t n, int out) {
    char *p = b;
    while (n > 0) {
        ssize_t r = out ? write(fd, p, n) : read(fd, p, n);
        if (r < 0 && errno == EINTR) continue;
        if (r <= 0) {
            if (r == 0) errno = EIO;
            return 0;
        }
        p += r;
        n -= (size_t)r;
    }
    return 1;
}

// ns_call asks the shim, and reads the data of its answer into buf, of cap
// bytes. It returns the answer's result, or -1 with errno set.
static int ns_call(int32_t op, int32_t pid, int32_t a, int32_t size, uint64_t b, void *buf, size_t cap) {
    int s = socket(AF_UNIX, SOCK_STREAM, 0);
    if (s < 0) return -1;
    int one = 1;
    setsockopt(s, SOL_SOCKET, SO_NOSIGPIPE, &one, sizeof one);
    struct sockaddr_un u;
    memset(&u, 0, sizeof u);
    u.sun_family = AF_UNIX;
    strlcpy(u.sun_path, procs_path, sizeof u.sun_path);
    u.sun_len = (unsigned char)SUN_LEN(&u);
    struct ns_request q = {op, pid, a, size, b};
    int32_t rep[2];
    if (connect(s, (struct sockaddr *)&u, u.sun_len) != 0 || !io_all(s, &q, sizeof q, 1) || !io_all(s, rep, sizeof rep, 0)) {
        int e = errno;
        close(s);
        errno = e;
        return -1;
    }
    if (rep[0] < 0) {
        close(s);
        errno = rep[1];
        return -1;
    }
    size_t data = op == NS_KILL ? 0 : op == NS_LIST ? (size_t)rep[0] * sizeof(int32_t) : (size_t)rep[0];
    if (data > cap) data = cap;
    int ok = !data || io_all(s, buf, data, 0);
    int e = errno;
    close(s);
    if (!ok) {
        errno = e;
        return -1;
    }
    return rep[0];
}

// ns_members returns the host pids of the shared namespace's processes, in
// a buffer the caller frees, and their count in *n; NULL where the
// namespace is the container's alone.
static pid_t *ns_members(int *n) {
    *n = 0;
    if (!ns_shared()) return NULL;
    size_t cap = 1024;
    for (int tries = 0; tries < 4; tries++) {
        pid_t *p = malloc(cap * sizeof *p);
        if (!p) return NULL;
        int r = ns_call(NS_LIST, 0, 0, 0, 0, p, cap * sizeof *p);
        if (r < 0) {
            free(p);
            return NULL;
        }
        if ((size_t)r <= cap) {
            *n = r;
            return p;
        }
        free(p);
        cap = (size_t)r + 64;
    }
    return NULL;
}

static int ns_has(const pid_t *set, int n, pid_t p) {
    for (int i = 0; i < n; i++) {
        if (set[i] == p) return 1;
    }
    return 0;
}

// ns_kill signals the shared namespace's processes, host pid hp, a group
// below -1, or -1 for all but the caller and pid 1, as kill does.
static int ns_kill(pid_t hp, int sig) {
    return ns_call(NS_KILL, hp, sig, init_pid, (uint64_t)getpid(), NULL, 0) < 0 ? -1 : 0;
}

// Pid 1. The container's main process (FWS_INIT, its host pid) is pid 1 to
// the container, as in a PID namespace, so a program that checks it runs
// as init does, and `kill -HUP 1` reaches it; every other process keeps
// its host pid, which the container sees as it is.

static pid_t to_host(pid_t p) { return init_pid && p == 1 ? init_pid : p; }
static pid_t to_container(pid_t p) { return init_pid && p == init_pid ? 1 : p; }

// naming is the thread pthread_setname_np runs in, whose getpid returns the
// host pid: libpthread names the thread through proc_info of getpid's pid,
// which in the main process would be launchd's. A thread-local would do,
// but an inserted library's thread-locals are not set up before the first
// calls of getpid.
static _Atomic(pthread_t) naming;
static os_unfair_lock naming_lock = OS_UNFAIR_LOCK_INIT;

static pid_t fws_getpid(void) {
    pid_t p = getpid();
    return atomic_load(&naming) == pthread_self() ? p : to_container(p);
}
INTERPOSE(fws_getpid, getpid);

static int fws_pthread_setname_np(const char *name) {
    os_unfair_lock_lock(&naming_lock);
    atomic_store(&naming, pthread_self());
    int r = pthread_setname_np(name);
    atomic_store(&naming, (pthread_t)NULL);
    os_unfair_lock_unlock(&naming_lock);
    return r;
}
INTERPOSE(fws_pthread_setname_np, pthread_setname_np);

// Init's parent is outside the container: 0, as in a PID namespace.
static pid_t fws_getppid(void) { return init_pid && getpid() == init_pid ? 0 : to_container(getppid()); }
INTERPOSE(fws_getppid, getppid);

// A signal to a process of a namespace's other container, which Seatbelt
// refuses, goes through the shim; on a shared namespace, so does one to
// every process.
static int fws_kill(pid_t pid, int sig) {
    if (pid == -1 && ns_shared()) {
        return ns_kill(-1, sig);
    }
    pid_t hp = pid < -1 ? -to_host(-pid) : pid == -1 ? pid : to_host(pid);
    int r = kill(hp, sig);
    if (r == -1 && errno == EPERM && hp != 0 && ns_shared()) {
        return ns_kill(hp, sig);
    }
    return r;
}
INTERPOSE(fws_kill, kill);

static int fws_killpg(pid_t pgrp, int sig) {
    int r = killpg(to_host(pgrp), sig);
    if (r == -1 && errno == EPERM && pgrp > 1 && ns_shared()) {
        return ns_kill(-to_host(pgrp), sig);
    }
    return r;
}
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
    if (r <= 0 && errno == EPERM && buf && size > 0 && ns_shared()) {
        int e = errno;
        r = ns_call(NS_INFO, to_host(pid), flavor, size, arg, buf, (size_t)size);
        if (r < 0) {
            errno = e;
            r = 0;
        }
    }
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

static int fws_proc_pidpath(int pid, void *buf, uint32_t size) {
    int r = proc_pidpath(to_host(pid), buf, size);
    if (r <= 0 && errno == EPERM && buf && size > 1 && ns_shared()) {
        int e = errno;
        r = ns_call(NS_PATH, to_host(pid), 0, 0, 0, buf, size - 1);
        if (r < 0 || (uint32_t)r >= size) {
            errno = r < 0 ? e : ENOMEM;
            return 0;
        }
        ((char *)buf)[r] = 0;
    }
    return r;
}
INTERPOSE(fws_proc_pidpath, proc_pidpath);

static int fws_proc_name(int pid, void *buf, uint32_t size) {
    int r = proc_name(to_host(pid), buf, size);
    if (r <= 0 && errno == EPERM && buf && size > 0 && ns_shared()) {
        struct proc_bsdinfo info;
        if (ns_call(NS_INFO, to_host(pid), PROC_PIDTBSDINFO, sizeof info, 0, &info, sizeof info) != (int)sizeof info) {
            errno = EPERM;
            return 0;
        }
        const char *name = info.pbi_name[0] ? info.pbi_name : info.pbi_comm;
        r = (int)strlcpy(buf, name, size);
        if ((uint32_t)r >= size) r = (int)size - 1;
    }
    return r;
}
INTERPOSE(fws_proc_name, proc_name);

static int fws_proc_pid_rusage(int pid, int flavor, rusage_info_t *buf) { return proc_pid_rusage(to_host(pid), flavor, buf); }
INTERPOSE(fws_proc_pid_rusage, proc_pid_rusage);

// Processes. The container's processes are those of its sandbox, whose
// information the sandbox lets them read, as a PID namespace holds a
// container's processes; a listing leaves out the others, and the anchor
// that starts the container's execs. On the host's PID namespace a
// listing holds every process.
static int visible(pid_t pid, const char *comm) {
    if (pid == getpid() || host_pids) {
        return 1;
    }
    if (helper_comm[0] && strncmp(comm, helper_comm, MAXCOMLEN) == 0) {
        return 0;
    }
    struct proc_bsdshortinfo info;
    return proc_pidinfo(pid, PROC_PIDT_SHORTBSDINFO, 0, &info, sizeof info) == (int)sizeof info;
}

// listed reports whether a listing holds pid: one of the container's
// processes, or of the shared namespace's, ns, n long.
static int listed(pid_t pid, const char *comm, const pid_t *ns, int n) {
    if (visible(pid, comm)) return 1;
    if (helper_comm[0] && strncmp(comm, helper_comm, MAXCOMLEN) == 0) return 0;
    return ns && ns_has(ns, n, pid);
}

// tagged reports whether pid started with the container's root in its
// environment, as the engine tells a container's processes apart.
static int tagged(pid_t pid) {
    int mib[3] = {CTL_KERN, KERN_PROCARGS2, pid};
    size_t len = 0;
    if (!root_tag[0] || sysctl(mib, 3, NULL, &len, NULL, 0) != 0 || len <= sizeof(int)) {
        return 0;
    }
    char *b = malloc(len);
    if (!b) {
        return 0;
    }
    int found = 0;
    if (sysctl(mib, 3, b, &len, NULL, 0) == 0 && len > sizeof(int)) {
        int argc;
        memcpy(&argc, b, sizeof argc);
        char *p = b + sizeof argc, *end = b + len;
        // The executable's path, the padding after it, then the arguments.
        p += strnlen(p, (size_t)(end - p));
        while (p < end && !*p) p++;
        for (int i = 0; i < argc && p < end; i++) p += strnlen(p, (size_t)(end - p)) + 1;
        size_t tl = strlen(root_tag);
        while (p < end && *p) {
            size_t n = strnlen(p, (size_t)(end - p));
            if (n == tl && memcmp(p, root_tag, tl) == 0) {
                found = 1;
                break;
            }
            p += n + 1;
        }
    }
    free(b);
    return found;
}

// counted reports whether pid is one of the container's processes: one of its
// sandbox's, or on the host's PID namespace, one the engine tagged.
static int counted(pid_t pid, const char *comm) {
    if (!host_pids) {
        return visible(pid, comm);
    }
    if (helper_comm[0] && strncmp(comm, helper_comm, MAXCOMLEN) == 0) {
        return 0;
    }
    return pid == getpid() || tagged(pid);
}

// pids_full reports whether the container runs as many processes as its
// pids limit allows, so that a fork fails with EAGAIN, as one past a pids
// cgroup's limit does. The container's supervisor kills any process that
// a race between two forks lets past it.
static int pids_full(void) {
    if (!pids_path[0]) return 0;
    char b[32];
    int fd = open(pids_path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return 0;
    ssize_t n = read(fd, b, sizeof b - 1);
    close(fd);
    if (n <= 0) return 0;
    b[n] = 0;
    unsigned long max;
    if (!num(b, &max) || max == 0) return 0;
    int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_RUID, (int)getuid()};
    size_t len = 0;
    if (sysctl(mib, 4, NULL, &len, NULL, 0) != 0) return 0;
    len += 32 * sizeof(struct kinfo_proc);
    struct kinfo_proc *kp = malloc(len);
    if (!kp) return 0;
    unsigned long count = 0;
    if (sysctl(mib, 4, kp, &len, NULL, 0) == 0) {
        for (size_t i = 0; i < len / sizeof *kp; i++) {
            if (kp[i].kp_proc.p_stat != SZOMB && counted(kp[i].kp_proc.p_pid, kp[i].kp_proc.p_comm)) count++;
        }
    }
    free(kp);
    return count >= max;
}

static pid_t fws_fork(void) {
    if (pids_full()) {
        errno = EAGAIN;
        return -1;
    }
    return fork();
}
INTERPOSE(fws_fork, fork);

// own_procs keeps the container's processes of a kinfo_proc array of n
// bytes, in place, and returns its new size.
static size_t own_procs(struct kinfo_proc *kp, size_t n) {
    size_t count = n / sizeof *kp, kept = 0;
    int nn;
    pid_t *ns = ns_members(&nn);
    for (size_t i = 0; i < count; i++) {
        if (listed(kp[i].kp_proc.p_pid, kp[i].kp_proc.p_comm, ns, nn)) {
            if (kept != i) memmove(&kp[kept], &kp[i], sizeof *kp);
            struct kinfo_proc *k = &kp[kept];
            k->kp_eproc.e_ppid = init_pid && k->kp_proc.p_pid == init_pid ? 0 : to_container(k->kp_eproc.e_ppid);
            k->kp_eproc.e_pgid = to_container(k->kp_eproc.e_pgid);
            k->kp_proc.p_pid = to_container(k->kp_proc.p_pid);
            kept++;
        }
    }
    free(ns);
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
    int n = 0, cap = size / (int)sizeof(pid_t), nn;
    pid_t *ns = ns_members(&nn);
    for (size_t i = 0; i < len / sizeof *kp && n < cap; i++) {
        if (ppid > 0 && kp[i].kp_eproc.e_ppid != to_host(ppid)) continue;
        if (listed(kp[i].kp_proc.p_pid, kp[i].kp_proc.p_comm, ns, nn)) buf[n++] = to_container(kp[i].kp_proc.p_pid);
    }
    free(ns);
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
// The container's network interfaces, as a container's network namespace
// holds them: the loopback interface, and an Ethernet interface, eth0
// onwards, for each of its addresses: on each network with a subnet, its
// address there, and on the others its member address in the member
// block, where the host's interfaces are. libSystem's getifaddrs and
// if_nameindex, and Go's net.Interfaces, read them from the routing
// sysctl NET_RT_IFLIST2 or NET_RT_IFLIST.

#define MEMBER_MASK 0xffff0000u
#define ETH_MTU 1500
#define MAX_IFACES 16
#define RT_ROUNDUP(a) ((a) > 0 ? (1 + (((a) - 1) | (sizeof(uint32_t) - 1))) : sizeof(uint32_t))

// An interface has an IPv4 address, and an IPv6 one where bits6 is not 0.
struct iface {
    in_addr_t addr, mask;
    struct in6_addr addr6;
    int bits6;
};
static int own_ifaces(struct iface *out);

// eth_mac is a MAC address of Docker's form, 02:42 and the address.
static void eth_mac(unsigned char mac[6], in_addr_t addr) {
    mac[0] = 0x02;
    mac[1] = 0x42;
    memcpy(mac + 2, &addr, 4);
}

static struct sockaddr_in v4_sockaddr(in_addr_t a) {
    struct sockaddr_in s;
    memset(&s, 0, sizeof s);
    s.sin_len = sizeof s;
    s.sin_family = AF_INET;
    s.sin_addr.s_addr = a;
    return s;
}

// eth_info writes at out the interface message of type, RTM_IFINFO or
// RTM_IFINFO2, of the container's interface eth<i>, of address addr, and
// returns its length.
static size_t eth_info(char *out, unsigned char type, unsigned short index, int i, in_addr_t addr) {
    size_t head = type == RTM_IFINFO2 ? sizeof(struct if_msghdr2) : sizeof(struct if_msghdr);
    char name[IFNAMSIZ];
    snprintf(name, sizeof name, "eth%d", i);
    struct sockaddr_dl dl;
    memset(&dl, 0, sizeof dl);
    dl.sdl_len = sizeof dl;
    dl.sdl_family = AF_LINK;
    dl.sdl_index = index;
    dl.sdl_type = IFT_ETHER;
    dl.sdl_nlen = (unsigned char)strlen(name);
    dl.sdl_alen = 6;
    memcpy(dl.sdl_data, name, dl.sdl_nlen);
    eth_mac((unsigned char *)dl.sdl_data + dl.sdl_nlen, addr);
    memset(out, 0, head);
    struct if_msghdr *m = (struct if_msghdr *)out;
    m->ifm_version = RTM_VERSION;
    m->ifm_type = type;
    m->ifm_addrs = RTA_IFP;
    m->ifm_flags = IFF_UP | IFF_BROADCAST | IFF_RUNNING | IFF_SIMPLEX | IFF_MULTICAST;
    m->ifm_index = index;
    if (type == RTM_IFINFO2) {
        struct if_msghdr2 *m2 = (struct if_msghdr2 *)out;
        m2->ifm_data.ifi_type = IFT_ETHER;
        m2->ifm_data.ifi_addrlen = 6;
        m2->ifm_data.ifi_hdrlen = 14;
        m2->ifm_data.ifi_mtu = ETH_MTU;
    } else {
        m->ifm_data.ifi_type = IFT_ETHER;
        m->ifm_data.ifi_addrlen = 6;
        m->ifm_data.ifi_hdrlen = 14;
        m->ifm_data.ifi_mtu = ETH_MTU;
    }
    memcpy(out + head, &dl, sizeof dl);
    size_t n = head + RT_ROUNDUP(sizeof dl);
    m->ifm_msglen = (unsigned short)n;
    return n;
}

// eth_addr writes at out the message of an interface's address and
// returns its length.
static size_t eth_addr(char *out, unsigned short index, const struct iface *f) {
    struct ifa_msghdr h;
    memset(&h, 0, sizeof h);
    h.ifam_version = RTM_VERSION;
    h.ifam_type = RTM_NEWADDR;
    h.ifam_addrs = RTA_NETMASK | RTA_IFA | RTA_BRD;
    h.ifam_index = index;
    struct sockaddr_in sa[3] = {v4_sockaddr(f->mask), v4_sockaddr(f->addr), v4_sockaddr(f->addr | ~f->mask)};
    size_t n = sizeof h;
    for (int i = 0; i < 3; i++) {
        memcpy(out + n, &sa[i], sizeof sa[i]);
        n += RT_ROUNDUP(sizeof sa[i]);
    }
    h.ifam_msglen = (unsigned short)n;
    memcpy(out, &h, sizeof h);
    return n;
}

// eth_addr6 writes at out the message of an interface's IPv6 address and
// returns its length.
static size_t eth_addr6(char *out, unsigned short index, const struct iface *f) {
    struct ifa_msghdr h;
    memset(&h, 0, sizeof h);
    h.ifam_version = RTM_VERSION;
    h.ifam_type = RTM_NEWADDR;
    h.ifam_addrs = RTA_NETMASK | RTA_IFA;
    h.ifam_index = index;
    struct sockaddr_in6 sa[2];
    memset(sa, 0, sizeof sa);
    for (int i = 0; i < 2; i++) {
        sa[i].sin6_len = sizeof sa[i];
        sa[i].sin6_family = AF_INET6;
    }
    for (int b = 0; b < f->bits6; b++) sa[0].sin6_addr.s6_addr[b / 8] |= (unsigned char)(0x80 >> (b % 8));
    sa[1].sin6_addr = f->addr6;
    size_t n = sizeof h;
    for (int i = 0; i < 2; i++) {
        memcpy(out + n, &sa[i], sizeof sa[i]);
        n += RT_ROUNDUP(sizeof sa[i]);
    }
    h.ifam_msglen = (unsigned short)n;
    memcpy(out, &h, sizeof h);
    return n;
}

// iflist answers a routing sysctl of interfaces, NET_RT_IFLIST or
// NET_RT_IFLIST2, as the container's network namespace would: the host's
// messages of its loopback interfaces, then the container's, for the
// interface the request names, or all of them.
static int iflist(const int *name, void *old, size_t *oldlen) {
    int mib[6];
    memcpy(mib, name, sizeof mib);
    int want = mib[5], af = mib[3];
    mib[5] = 0;
    char *buf = NULL;
    size_t n = 0;
    for (;;) {
        if (sysctl(mib, 6, NULL, &n, NULL, 0) == -1) return -1;
        n += n / 4 + 1024;
        free(buf);
        if (!(buf = malloc(n + MAX_IFACES * 512))) {
            errno = ENOMEM;
            return -1;
        }
        if (sysctl(mib, 6, buf, &n, NULL, 0) == 0) break;
        if (errno != ENOMEM) {
            free(buf);
            return -1;
        }
    }
    // The loopback interfaces, and the highest index, whose successors
    // the container's interfaces take so that no host interface's index
    // names one.
    unsigned char loop[65536 / 8] = {0};
    unsigned short top = 0;
    for (size_t off = 0; off + sizeof(struct if_msghdr) <= n;) {
        const struct if_msghdr *m = (const void *)(buf + off);
        if (m->ifm_msglen == 0) break;
        if (m->ifm_index > top) top = m->ifm_index;
        if ((m->ifm_type == RTM_IFINFO || m->ifm_type == RTM_IFINFO2) && (m->ifm_flags & IFF_LOOPBACK)) {
            loop[m->ifm_index / 8] |= (unsigned char)(1 << (m->ifm_index % 8));
        }
        off += m->ifm_msglen;
    }
    size_t out = 0;
    for (size_t off = 0; off + sizeof(struct if_msghdr) <= n;) {
        struct if_msghdr *m = (void *)(buf + off);
        size_t len = m->ifm_msglen;
        if (len == 0) break;
        if ((loop[m->ifm_index / 8] & (1 << (m->ifm_index % 8))) && (!want || want == m->ifm_index)) {
            memmove(buf + out, m, len);
            out += len;
        }
        off += len;
    }
    struct iface ifs[MAX_IFACES];
    int nif = own_ifaces(ifs);
    char extra[MAX_IFACES * 512];
    size_t more = 0;
    for (int i = 0; i < nif && top + 1 + i <= 65535; i++) {
        unsigned short index = (unsigned short)(top + 1 + i);
        if (want && want != index) continue;
        more += eth_info(extra + more, mib[4] == NET_RT_IFLIST2 ? RTM_IFINFO2 : RTM_IFINFO, index, i, ifs[i].addr);
        if (af == 0 || af == AF_INET) more += eth_addr(extra + more, index, &ifs[i]);
        if ((af == 0 || af == AF_INET6) && ifs[i].bits6) more += eth_addr6(extra + more, index, &ifs[i]);
    }
    memcpy(buf + out, extra, more);
    out += more;
    if (!old) {
        *oldlen = out;
        free(buf);
        return 0;
    }
    int r = 0;
    if (*oldlen < out) {
        errno = ENOMEM;
        r = -1;
        out = *oldlen;
    }
    memcpy(old, buf, out);
    *oldlen = out;
    free(buf);
    return r;
}

// ifconf answers SIOCGIFCONF from the interfaces getifaddrs reads, which
// the routing sysctl presents: an entry per address, of its interface's
// name and the address, as long as the address or a struct sockaddr.
static int ifconf(struct ifconf *c) {
    struct ifaddrs *list;
    if (getifaddrs(&list) == -1) return -1;
    int used = 0;
    for (struct ifaddrs *a = list; a; a = a->ifa_next) {
        if (!a->ifa_addr) continue;
        int size = IFNAMSIZ + (a->ifa_addr->sa_len > sizeof(struct sockaddr) ? a->ifa_addr->sa_len : (int)sizeof(struct sockaddr));
        if (c->ifc_buf && used + size > c->ifc_len) break;
        if (c->ifc_buf) {
            char *e = c->ifc_buf + used;
            memset(e, 0, (size_t)size);
            strlcpy(e, a->ifa_name, IFNAMSIZ);
            memcpy(e + IFNAMSIZ, a->ifa_addr, a->ifa_addr->sa_len);
        }
        used += size;
    }
    freeifaddrs(list);
    c->ifc_len = used;
    return 0;
}

// eth_ioctl answers a query of a container interface's flags, MTU or
// addresses.
static int eth_ioctl(unsigned long req, struct ifreq *r, const struct iface *f) {
    struct sockaddr_in a;
    switch (req) {
    case SIOCGIFFLAGS:
        r->ifr_flags = (short)(IFF_UP | IFF_BROADCAST | IFF_RUNNING | IFF_SIMPLEX | IFF_MULTICAST);
        return 0;
    case SIOCGIFMTU:
        r->ifr_mtu = ETH_MTU;
        return 0;
    case SIOCGIFADDR:
        a = v4_sockaddr(f->addr);
        break;
    case SIOCGIFNETMASK:
        a = v4_sockaddr(f->mask);
        break;
    default:
        a = v4_sockaddr(f->addr | ~f->mask);
        break;
    }
    memcpy(&r->ifr_addr, &a, sizeof a);
    return 0;
}

// eth_named finds the container's interface ethN that name names.
static int eth_named(const char *name, struct iface *out) {
    unsigned long i;
    const char *e;
    if (strncmp(name, "eth", 3) != 0 || !(e = num(name + 3, &i)) || *e || (name[3] == '0' && name[4])) return 0;
    struct iface ifs[MAX_IFACES];
    int n = own_ifaces(ifs);
    if (i >= (unsigned long)n) return 0;
    *out = ifs[i];
    return 1;
}

static int fws_ioctl(int fd, unsigned long req, ...) {
    va_list ap;
    va_start(ap, req);
    void *arg = va_arg(ap, void *);
    va_end(ap);
    switch (req) {
    case SIOCGIFCONF:
        if (arg) return ifconf(arg);
        break;
    case SIOCGIFFLAGS:
    case SIOCGIFMTU:
    case SIOCGIFADDR:
    case SIOCGIFNETMASK:
    case SIOCGIFBRDADDR: {
        struct ifreq *r = arg;
        if (!r) break;
        char name[IFNAMSIZ + 1];
        struct iface f;
        memcpy(name, r->ifr_name, IFNAMSIZ);
        name[IFNAMSIZ] = 0;
        if (eth_named(name, &f)) return eth_ioctl(req, r, &f);
        // The host's other interfaces are not the container's.
        if (strncmp(r->ifr_name, "lo", 2) != 0) {
            errno = ENXIO;
            return -1;
        }
        break;
    }
    }
    return ioctl(fd, req, arg);
}
INTERPOSE(fws_ioctl, ioctl);

// ns_procargs reads kern.procargs2 of a namespace's other container's
// process, hp, through the shim, or kern.procargs, which leaves out its
// argument count, as sysctl does.
static int ns_procargs(pid_t hp, int two, void *old, size_t *oldlen) {
    size_t cap = 1 << 20;
    char *b = malloc(cap);
    if (!b) {
        errno = ENOMEM;
        return -1;
    }
    int r = ns_call(NS_ARGS, hp, 0, 0, 0, b, cap);
    if (r < 0) {
        int e = errno;
        free(b);
        errno = e == ESRCH ? EINVAL : e;
        return -1;
    }
    size_t skip = two || r < (int)sizeof(int) ? 0 : sizeof(int), n = (size_t)r - skip;
    if (!old) {
        *oldlen = n;
        free(b);
        return 0;
    }
    if (*oldlen < n) {
        memcpy(old, b + skip, *oldlen);
        free(b);
        errno = ENOMEM;
        return -1;
    }
    memcpy(old, b + skip, n);
    *oldlen = n;
    free(b);
    return 0;
}

static int fws_sysctl(int *name, u_int namelen, void *old, size_t *oldlen, void *new, size_t newlen) {
    if (namelen == 6 && name[0] == CTL_NET && name[1] == PF_ROUTE && name[2] == 0 &&
        (name[4] == NET_RT_IFLIST || name[4] == NET_RT_IFLIST2) && oldlen && !new) {
        return iflist(name, old, oldlen);
    }
    if (namelen == 3 && name[0] == CTL_KERN && (name[1] == KERN_PROCARGS || name[1] == KERN_PROCARGS2) && !new && oldlen) {
        int mib[3] = {name[0], name[1], to_host(name[2])};
        int r = sysctl(mib, 3, old, oldlen, new, newlen);
        if (r == 0 || errno != EPERM || !ns_shared()) {
            return r;
        }
        return ns_procargs(mib[2], name[1] == KERN_PROCARGS2, old, oldlen);
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
    // On its own UTS namespace the container has its hostname and NIS
    // domain name, which getdomainname reads here.
    if (hostname[0] && namelen == 2 && name[0] == CTL_KERN && (name[1] == KERN_HOSTNAME || name[1] == KERN_NISDOMAINNAME) &&
        new == NULL) {
        const char *v = name[1] == KERN_HOSTNAME ? hostname : domainname;
        size_t n = strlen(v) + 1;
        if (old == NULL) {
            *oldlen = n;
            return 0;
        }
        if (*oldlen < n) {
            errno = ENOMEM;
            return -1;
        }
        memcpy(old, v, n);
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
    if (hostname[0] && strcmp(name, "kern.nisdomainname") == 0 && new == NULL) {
        int mib[2] = {CTL_KERN, KERN_NISDOMAINNAME};
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

// The sizes of the IPC namespace's shared memory objects, which a ledger
// file every process of the namespace locks and rewrites keeps, as a
// tmpfs at /dev/shm keeps its files' sizes: an object grows only while
// the objects' sizes stay within the limit.
static char shm_ledger[PATH_MAX];
static unsigned long long shm_limit;
static struct {
    int fd;
    dev_t dev;
    ino_t ino;
    char name[POSIX_IPC_NAME_MAX + 1];
} shm_fds[64];
static pthread_mutex_t shm_lock = PTHREAD_MUTEX_INITIALIZER;

static void parse_shm(const char *v) {
    const char *tab = v ? strchr(v, '\t') : NULL;
    unsigned long limit;
    if (!tab || (size_t)(tab - v) >= sizeof shm_ledger || !num(tab + 1, &limit)) return;
    memcpy(shm_ledger, v, (size_t)(tab - v));
    shm_ledger[tab - v] = 0;
    shm_limit = limit;
}

// shm_record sets name's size in the ledger, or removes it for a size of
// -1. With check, it refuses a size that would take the objects past the
// limit. It reports the size the ledger held before in *prev, -1 for none.
static int shm_record(const char *name, long long size, int check, long long *prev) {
    *prev = -1;
    int lf = open(shm_ledger, O_RDWR | O_CREAT | O_CLOEXEC, 0600);
    if (lf < 0) return -1;
    if (flock(lf, LOCK_EX) != 0) {
        close(lf);
        return -1;
    }
    static char in[65536], out[65536];
    ssize_t n = pread(lf, in, sizeof in - 1, 0);
    if (n < 0) n = 0;
    in[n] = 0;
    size_t o = 0;
    unsigned long long total = 0;
    for (char *line = in, *next; line && *line; line = next) {
        next = strchr(line, '\n');
        if (next) *next++ = 0;
        char *tab = strchr(line, '\t');
        unsigned long sz;
        if (!tab || !num(tab + 1, &sz)) continue;
        *tab = 0;
        if (strcmp(line, name) == 0) {
            *prev = (long long)sz;
            continue;
        }
        total += sz;
        o += (size_t)snprintf(out + o, sizeof out - o, "%s\t%lu\n", line, sz);
        if (o >= sizeof out) break;
    }
    int r = 0;
    if (check && size >= 0 && total + (unsigned long long)size > shm_limit) {
        errno = ENOSPC;
        r = -1;
    } else {
        if (size >= 0 && o < sizeof out) o += (size_t)snprintf(out + o, sizeof out - o, "%s\t%lld\n", name, size);
        if (o > sizeof out) o = sizeof out;
        if (ftruncate(lf, 0) != 0 || pwrite(lf, out, o, 0) != (ssize_t)o) r = -1;
    }
    flock(lf, LOCK_UN);
    close(lf);
    return r;
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
    const char *n = posix_ipc_name(name, own);
    int fd = shm_open(n, flags, mode);
    struct stat st;
    if (fd >= 0 && shm_ledger[0] && fstat(fd, &st) == 0) {
        pthread_mutex_lock(&shm_lock);
        size_t slot = sizeof shm_fds / sizeof shm_fds[0];
        for (size_t i = 0; i < sizeof shm_fds / sizeof shm_fds[0]; i++) {
            if (shm_fds[i].name[0] == 0 || shm_fds[i].fd == fd) {
                slot = i;
                if (shm_fds[i].fd == fd) break;
            }
        }
        if (slot < sizeof shm_fds / sizeof shm_fds[0]) {
            shm_fds[slot].fd = fd;
            shm_fds[slot].dev = st.st_dev;
            shm_fds[slot].ino = st.st_ino;
            strlcpy(shm_fds[slot].name, n, sizeof shm_fds[slot].name);
        }
        pthread_mutex_unlock(&shm_lock);
    }
    return fd;
}
INTERPOSE(fws_shm_open, shm_open);

static int fws_shm_unlink(const char *name) {
    char own[POSIX_IPC_NAME_MAX + 1];
    const char *n = posix_ipc_name(name, own);
    int r = shm_unlink(n);
    if (r == 0 && shm_ledger[0]) {
        int saved = errno;
        long long prev;
        shm_record(n, -1, 0, &prev);
        errno = saved;
    }
    return r;
}
INTERPOSE(fws_shm_unlink, shm_unlink);

// ftruncate sizes a shared memory object, which macOS lets happen once:
// the ledger takes the size first, and gets the old one back if the
// kernel refuses it.
static int fws_ftruncate(int fd, off_t length) {
    char name[POSIX_IPC_NAME_MAX + 1] = "";
    struct stat st;
    if (shm_ledger[0] && length >= 0 && fstat(fd, &st) == 0) {
        pthread_mutex_lock(&shm_lock);
        for (size_t i = 0; i < sizeof shm_fds / sizeof shm_fds[0]; i++) {
            if (shm_fds[i].name[0] && shm_fds[i].fd == fd && shm_fds[i].dev == st.st_dev && shm_fds[i].ino == st.st_ino) {
                strlcpy(name, shm_fds[i].name, sizeof name);
                break;
            }
        }
        pthread_mutex_unlock(&shm_lock);
    }
    if (!name[0]) return ftruncate(fd, length);
    long long prev;
    if (shm_record(name, (long long)length, 1, &prev) != 0) return -1;
    int r = ftruncate(fd, length);
    if (r != 0) {
        int saved = errno;
        long long ignored;
        shm_record(name, prev, 0, &ignored);
        errno = saved;
    }
    return r;
}
INTERPOSE(fws_ftruncate, ftruncate);

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
static int own_addr(in_addr_t a);
static in_addr_t own_seen_by(in_addr_t from);
static int member_of6(const struct in6_addr *a, in_addr_t *out);
static int member_v6(in_addr_t a, in_addr_t peer, struct in6_addr *out);
static int own_addr6(const struct in6_addr *a);
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
// NET_DCONN: a UDP socket on a port without a host port connected to
// net_peer and net_dport, the peer as reach finds it.
#define NET_DCONN 32
// NET_P6: an IPv6 socket that reached a member at an IPv6 address, whose
// members' addresses read as theirs on IPv6.
#define NET_P6 64
// NET_HELD: a socket listen_unix bound to its port's Unix socket file.
#define NET_HELD 128
// What the router puts before a datagram on a Unix one: the IPv4 address
// and port it came from, or goes to.
#define DGRAM_HEADER 6
// What the router writes on a connection to a listener on a port without a
// host port before its stream: the client's IPv4 address and port.
#define CLIENT_HEADER 6
// The most iovecs a datagram takes, one fewer than the kernel's UIO_MAXIOV
// for the header's.
#define DGRAM_IOV 1023
static unsigned char net_state[NET_FDS];
// net_v6only: an IPv6 socket listen_unix bound that its program made
// IPv6-only, which shares its port with the program's IPv4 listener on it,
// as an IPv6-only and an IPv4 socket share a port on Linux.
static unsigned char net_v6only[NET_FDS];
// For NET_UNIX and NET_ROUTED sockets: the container port, and for
// NET_ROUTED ones the peer's address.
static unsigned short net_port[NET_FDS];
static in_addr_t net_peer[NET_FDS];
static unsigned short net_dport[NET_FDS];
// The control messages a UDP socket on a port without a host port asks
// for: the kernel sees a Unix datagram socket, which has no IP options, so
// the library keeps them and writes the messages itself.
#define CM_DSTADDR 1
#define CM_PKTINFO 2
#define CM_PKTINFO6 4
#define CM_2292PKTINFO6 8
static unsigned char net_cmsg[NET_FDS];

static int cmsg_flag(int level, int name) {
    if (level == IPPROTO_IP && name == IP_RECVDSTADDR) return CM_DSTADDR;
    if (level == IPPROTO_IP && name == IP_PKTINFO) return CM_PKTINFO;
    if (level == IPPROTO_IPV6 && name == IPV6_RECVPKTINFO) return CM_PKTINFO6;
    if (level == IPPROTO_IPV6 && name == IPV6_2292PKTINFO) return CM_2292PKTINFO6;
    return 0;
}

// unix_names and unix_peers hold the container paths of Unix sockets
// bound, or connected, by relative names, whose addresses hold only the
// path below their mount or the root.
static char *unix_names[NET_FDS], *unix_peers[NET_FDS];

// acc_peers holds the peer the kernel named as accept took each connection,
// which getpeername names once Darwin refuses to: after the connection shut
// down both ways, where Linux still names a TCP connection's client.
static struct sockaddr_storage *acc_peers[NET_FDS];

static void set_peer(int fd, const struct sockaddr_storage *k) {
    if (fd < 0 || fd >= NET_FDS || acc_peers[fd] == k) return;
    free(acc_peers[fd]);
    acc_peers[fd] = NULL;
    if (k && (k->ss_family == AF_INET || k->ss_family == AF_INET6) && (acc_peers[fd] = malloc(sizeof *k))) {
        memcpy(acc_peers[fd], k, sizeof *k);
    }
}

static void set_name(char **table, int fd, const char *name) {
    if (fd < 0 || fd >= NET_FDS || table[fd] == name) return;
    free(table[fd]);
    table[fd] = name ? strdup(name) : NULL;
}

static void name_unix(char **table, int fd, const struct sockaddr *sa, socklen_t len) {
    char p[sizeof((struct sockaddr_un *)0)->sun_path + 1] = {0};
    size_t at = offsetof(struct sockaddr_un, sun_path), n = (size_t)len - at;
    memcpy(p, ((const struct sockaddr_un *)(const void *)sa)->sun_path, n < sizeof p - 1 ? n : sizeof p - 1);
    set_name(table, fd, p);
}

static unsigned char state_of(int fd) { return fd >= 0 && fd < NET_FDS ? net_state[fd] : 0; }

static void set_state(int fd, unsigned char st) {
    if (fd >= 0 && fd < NET_FDS) {
        net_state[fd] = st;
        // Only a socket listen_unix holds keeps whether it is IPv6-only.
        if (!(st & NET_HELD)) net_v6only[fd] = 0;
    }
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

// member6 reports whether a is an IPv6 address the library maps to IPv4,
// none of loopback, the wildcard and IPv4-mapped's, which have their own.
static int member6(const struct in6_addr *a) {
    return !IN6_IS_ADDR_V4MAPPED(a) && !IN6_IS_ADDR_LOOPBACK(a) && !IN6_IS_ADDR_UNSPECIFIED(a);
}

// dest_p6 is NET_P6 for an address that is a member's IPv6 one, the
// container's own included.
static unsigned char dest_p6(const struct sockaddr *sa) {
    return sa && sa->sa_family == AF_INET6 && member6(&((const struct sockaddr_in6 *)sa)->sin6_addr) ? NET_P6 : 0;
}

// v6_target maps member6 address a to the IPv4 address the library
// routes it by, the member address of the member the container sees that
// holds it, and reports whether one does. Where a lies in a subnet of the
// members' networks that no member it sees holds, it fails with errno
// EHOSTUNREACH, as an address on a network no host answers at would.
static int v6_target(const struct in6_addr *a, in_addr_t *v4) {
    int r = member_of6(a, v4);
    if (r == -1) errno = EHOSTUNREACH;
    return r;
}

// p6_addr finds the IPv6 address a member's IPv4 address a reads as on
// fd, a NET_P6 socket: its member's, or with own set, the container's
// own, on a network it shares with the socket's peer.
static int p6_addr(int fd, in_addr_t a, int own, struct in6_addr *out) {
    if (!(state_of(fd) & NET_P6)) return 0;
    if (own ? a != htonl(INADDR_LOOPBACK) && !own_addr(a) : !member_block(a)) return 0;
    return member_v6(own ? htonl(INADDR_LOOPBACK) : a, fd < NET_FDS ? net_peer[fd] : 0, out);
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

// enter_dir makes dir the calling thread's working directory alone, for
// a Unix socket named relative to it; leave_dir restores the process's
// and closes dir, which enter_dir also closes when it fails.
extern int pthread_fchdir_np(int) __attribute__((weak_import));
static int enter_dir(int dir) {
    int r = pthread_fchdir_np(dir);
    if (r != 0) {
        int e = errno;
        close(dir);
        errno = e;
    }
    return r;
}

static void leave_dir(int dir) {
    int e = errno;
    pthread_fchdir_np(-1);
    close(dir);
    errno = e;
}

// in_dir runs a bind or connect of fd to a Unix socket named relative to
// dir, which it closes.
static int in_dir(int dir, int (*op)(int, const struct sockaddr *, socklen_t), int fd, const struct sockaddr *to, socklen_t len) {
    if (enter_dir(dir) != 0) {
        return -1;
    }
    int r = op(fd, to, len);
    leave_dir(dir);
    return r;
}

// unix_base returns the length of the directory a host path longer than a
// Unix socket address holds is named relative to: the source of the mount
// that holds it, in unmap's order, or the root. The rest of the path is
// then the container's path below the mount or the root, which fits when
// the container's does, and which unix_relative finds again from a
// datagram's source. It returns 0 for a path in neither.
static size_t unix_base(const char *host) {
    for (size_t i = 0; i < nmounts; i++) {
        if (under(host, mounts[i].src, mounts[i].srclen)) return mounts[i].srclen;
    }
    return in_root(host) ? rootlen : 0;
}

// unix_relative turns the relative path a socket bound by unix_host's
// relative name reports, in place, into its container path: below the
// mount, or the root, where a socket of that path is.
static void unix_relative(char *p, size_t size) {
    char host[PATH_MAX], cpath[PATH_MAX];
    struct stat st;
    for (size_t i = 0; i < nmounts; i++) {
        if (join(host, sizeof host, mounts[i].src, "/", p) && stat(host, &st) == 0 && S_ISSOCK(st.st_mode) &&
            join(cpath, sizeof cpath, mounts[i].dest, "/", p)) {
            strlcpy(p, cpath, size);
            return;
        }
    }
    if (join(host, sizeof host, root, "/", p) && stat(host, &st) == 0 && S_ISSOCK(st.st_mode) && join(cpath, sizeof cpath, "", "/", p)) {
        strlcpy(p, cpath, size);
    }
}

// unix_host maps a Unix socket address's container path to the host path
// it names, as the file calls map paths; follow says whether a link its
// last component names is followed. A host path longer than a Unix socket
// address holds, as a root's deep path makes one the container's fits,
// is named relative to its mount's source or the root, opened in *dir,
// for in_dir; without dir, it fails with ENAMETOOLONG.
static const struct sockaddr *unix_host(const struct sockaddr *sa, socklen_t *len, struct sockaddr_storage *out, int follow, int *dir) {
    size_t at = offsetof(struct sockaddr_un, sun_path);
    if (!rootlen || *len <= (socklen_t)at) {
        return sa;
    }
    char path[sizeof((struct sockaddr_un *)0)->sun_path + 1];
    size_t n = (size_t)*len - at;
    if (n > sizeof path - 1) n = sizeof path - 1;
    memcpy(path, ((const struct sockaddr_un *)(const void *)sa)->sun_path, n);
    path[n] = 0;
    if (!path[0]) {
        return sa;
    }
    const char *host = MAPF(path, follow);
    if (host == path || strcmp(host, path) == 0) {
        return sa;
    }
    struct sockaddr_un *u = (struct sockaddr_un *)out;
    memset(u, 0, sizeof *u);
    if (strlcpy(u->sun_path, host, sizeof u->sun_path) >= sizeof u->sun_path) {
        size_t bn = unix_base(host);
        if (!dir || !bn || host[bn] != '/' || strlen(host + bn + 1) >= sizeof u->sun_path || !pthread_fchdir_np) {
            errno = ENAMETOOLONG;
            return NULL;
        }
        char base[PATH_MAX];
        memcpy(base, host, bn);
        base[bn] = 0;
        if ((*dir = open(base, O_RDONLY | O_DIRECTORY | O_CLOEXEC)) == -1) {
            return NULL;
        }
        memset(u->sun_path, 0, sizeof u->sun_path);
        strlcpy(u->sun_path, host + bn + 1, sizeof u->sun_path);
    }
    u->sun_family = AF_UNIX;
    u->sun_len = (unsigned char)SUN_LEN(u);
    *len = u->sun_len;
    return (const struct sockaddr *)out;
}

// local returns the address a bind to sa binds, in out, converting fd first
// where an IPv4 address goes through an IPv6 socket.
static const struct sockaddr *local(int fd, const struct sockaddr *sa, socklen_t *len, struct sockaddr_storage *out, unsigned char *any, int *dir) {
    *any = 0;
    if (!sa) {
        return sa;
    }
    if (sa->sa_family == AF_UNIX) {
        return unix_host(sa, len, out, 0, dir);
    }
    int type = sock_type(fd);
    if (sa->sa_family == AF_INET && *len >= (socklen_t)sizeof(struct sockaddr_in)) {
        const struct sockaddr_in *a = (const void *)sa;
        // The container's own member address is one of loopback's.
        in_addr_t at = own_addr(a->sin_addr.s_addr) ? htonl(INADDR_LOOPBACK) : a->sin_addr.s_addr;
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
        // The container's own IPv6 address is one of loopback's, as its
        // IPv4 one is.
        int mine = !wild && member6(&a->sin6_addr) && own_addr6(&a->sin6_addr);
        if (IN6_IS_ADDR_V4MAPPED(&a->sin6_addr) || (wild && !v6only) || mine) {
            in_addr_t v4 = wild ? htonl(INADDR_ANY) : mine ? htonl(INADDR_LOOPBACK) : mapped_v4(&a->sin6_addr);
            if (own_addr(v4)) v4 = htonl(INADDR_LOOPBACK);
            if (!v4_local(v4) || to_ipv4(fd, type) == -1) {
                return sa;
            }
            if (mine) set_state(fd, state_of(fd) | NET_P6);
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

// relay_key reads the relay key a request for a relay carries, which the
// container's router keeps in the port directory for the container's life.
#define RELAY_KEY 16
static unsigned char relay_key_bytes[RELAY_KEY];
static int relay_key_read;

static const unsigned char *relay_key(void) {
    if (__atomic_load_n(&relay_key_read, __ATOMIC_ACQUIRE)) {
        return relay_key_bytes;
    }
    char p[PATH_MAX];
    if (!join(p, sizeof p, port_dir, "/relay-key", "")) {
        return NULL;
    }
    int f = open(p, O_RDONLY | O_CLOEXEC);
    if (f < 0) {
        return NULL;
    }
    unsigned char k[RELAY_KEY];
    ssize_t n = read(f, k, sizeof k);
    close(f);
    if (n != RELAY_KEY) {
        return NULL;
    }
    memcpy(relay_key_bytes, k, sizeof k);
    __atomic_store_n(&relay_key_read, 1, __ATOMIC_RELEASE);
    return relay_key_bytes;
}

// ask_router asks the router at loopback port router for UDP port port's
// flow port, or with foreign set, the container's own router for its relay
// to UDP port port of another stack's member at addr.
static unsigned short ask_router(unsigned short router, unsigned short port, in_addr_t addr, int foreign) {
    int s = socket(AF_INET, SOCK_STREAM, 0);
    if (s < 0) {
        return 0;
    }
    struct sockaddr_in r = v4_addr(htonl(INADDR_LOOPBACK), router);
    unsigned char req[4 + RELAY_KEY + 6] = {0, 0, (unsigned char)(port >> 8), (unsigned char)port}, ans[3] = {1, 0, 0};
    ssize_t n = 4;
    if (foreign) {
        const unsigned char *key = relay_key();
        if (!key) {
            close(s);
            return 0;
        }
        req[2] = req[3] = 0xff;
        memcpy(req + 4, key, RELAY_KEY);
        memcpy(req + 4 + RELAY_KEY, &addr, 4);
        req[8 + RELAY_KEY] = (unsigned char)(port >> 8);
        req[9 + RELAY_KEY] = (unsigned char)port;
        n = (ssize_t)sizeof req;
    }
    int ok = connect(s, (struct sockaddr *)&r, sizeof r) == 0 && write(s, req, (size_t)n) == n && read(s, ans, 1) == 1 && ans[0] == 0 &&
             read(s, ans + 1, 2) == 2;
    close(s);
    return ok ? (unsigned short)(ans[1] << 8 | ans[2]) : 0;
}

static int port_path(int type, unsigned short port, struct sockaddr_un *u);

static unsigned short flow_port(unsigned short router, unsigned short port, in_addr_t addr, int foreign) {
    int n = __atomic_load_n(&nflows, __ATOMIC_ACQUIRE);
    for (int i = 0; i < n && i < MAX_FLOWS; i++) {
        if (__atomic_load_n(&flows[i].ready, __ATOMIC_ACQUIRE) && flows[i].router == router && flows[i].port == port && flows[i].addr == addr) {
            return flows[i].flow;
        }
    }
    unsigned short f = ask_router(router, port, addr, foreign);
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
        if (!mh && member == 1 && type == SOCK_DGRAM && router) mh = flow_port(router, port, v4, 0);
        // Another stack's member's datagrams go through the container's
        // own router's relay to it.
        if (member == 2 && type == SOCK_DGRAM && router_port) mh = flow_port(router_port, port, v4, 1);
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
        h = flow_port(router_port, port, htonl(INADDR_LOOPBACK), 0);
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
static const struct sockaddr *remote(int fd, const struct sockaddr *sa, socklen_t *len, struct sockaddr_storage *out, int *dir) {
    if (!sa) {
        return sa;
    }
    if (sa->sa_family == AF_UNIX) {
        return unix_host(sa, len, out, 1, dir);
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
        int t = member6(&a->sin6_addr) ? v6_target(&a->sin6_addr, &v4) : 0;
        if (t == -1) {
            return NULL;
        }
        if (t == 1) {
            // A member's IPv6 address is its IPv4 one, through the IPv4
            // socket in the IPv6 one's place.
            if (!(state_of(fd) & NET_V6) && to_ipv4(fd, type) == -1) {
                return NULL;
            }
            set_state(fd, state_of(fd) | NET_P6);
            if (fd < NET_FDS && !(state_of(fd) & (NET_ROUTED | NET_DCONN))) net_peer[fd] = v4;
        } else if (IN6_IS_ADDR_V4MAPPED(&a->sin6_addr) || (state_of(fd) & NET_V6)) {
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
// wildcard as IPv6's, and a member's address as its IPv6 one on a
// NET_P6 socket. own marks the socket's own address.
static void present_inet(int fd, in_addr_t a, unsigned short port, int own, struct sockaddr *out, socklen_t *outlen) {
    unsigned char st = state_of(fd);
    struct sockaddr_storage v;
    socklen_t vn;
    memset(&v, 0, sizeof v);
    if (st & NET_V6) {
        struct sockaddr_in6 *b = (struct sockaddr_in6 *)&v;
        b->sin6_len = sizeof *b;
        b->sin6_family = AF_INET6;
        b->sin6_port = htons(port);
        int p6 = p6_addr(fd, a, own, &b->sin6_addr);
        if (!p6 && (st & NET_LO6) && a == htonl(INADDR_LOOPBACK)) {
            b->sin6_addr = in6addr_loopback;
        } else if (!p6 && a != htonl(INADDR_ANY)) {
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
        // accepted, whose client is where the router saw it.
        int in = fd >= 0 && fd < NET_FDS;
        in_addr_t a = own ? ((st & NET_ANY) ? htonl(INADDR_ANY) : htonl(INADDR_LOOPBACK)) : in && net_peer[fd] ? net_peer[fd] : htonl(INADDR_LOOPBACK);
        present_inet(fd, a, !in ? 0 : own ? net_port[fd] : net_dport[fd], own, out, outlen);
        return;
    }
    if ((st & NET_ROUTED) && !own) {
        present_inet(fd, net_peer[fd], net_port[fd], 0, out, outlen);
        return;
    }
    struct sockaddr_storage v;
    memcpy(&v, k, sizeof v);
    socklen_t vn = kn;
    int type = sock_type(fd);
    if (k->ss_family == AF_UNIX && kn > (socklen_t)offsetof(struct sockaddr_un, sun_path)) {
        // A Unix socket's path is the container's.
        struct sockaddr_un *u = (struct sockaddr_un *)&v;
        u->sun_path[sizeof u->sun_path - 1] = 0;
        char *name = fd >= 0 && fd < NET_FDS ? (own ? unix_names : unix_peers)[fd] : NULL;
        if (name && u->sun_path[0] && u->sun_path[0] != '/') {
            strlcpy(u->sun_path, name, sizeof u->sun_path);
        } else if (u->sun_path[0] && u->sun_path[0] != '/') {
            unix_relative(u->sun_path, sizeof u->sun_path);
        } else {
            unmap(u->sun_path, sizeof u->sun_path);
        }
        u->sun_len = (unsigned char)SUN_LEN(u);
        vn = u->sun_len;
    }
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
            int wild = own && (st & NET_ANY), p6 = !wild && p6_addr(fd, a->sin_addr.s_addr, own, &b.sin6_addr);
            if (!p6 && (st & NET_LO6) && a->sin_addr.s_addr == htonl(INADDR_LOOPBACK)) {
                b.sin6_addr = in6addr_loopback;
            } else if (!p6 && !wild && a->sin_addr.s_addr != htonl(INADDR_ANY)) {
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
// container can listen on it with a Unix socket, and the TCP port sa names
// on loopback alone, which the host port would publish; any is whether sa
// is the wildcard, zero whether sa asks for any port, which unbacked
// reports as 0, and lo whether sa is loopback on a port with a host port.
static unsigned short unbacked(int fd, const struct sockaddr *sa, socklen_t len, int *any, int *v6, int *zero, int *lo) {
    *zero = 0;
    *lo = 0;
    int type = sa && port_dir[0] ? sock_type(fd) : 0;
    if (type != SOCK_STREAM && type != SOCK_DGRAM) {
        return 0;
    }
    in_addr_t a;
    unsigned short port;
    int own = 0;
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
        } else if (IN6_IS_ADDR_LOOPBACK(&s6->sin6_addr) || own_addr6(&s6->sin6_addr)) {
            own = !IN6_IS_ADDR_LOOPBACK(&s6->sin6_addr);
            a = htonl(INADDR_LOOPBACK);
        } else if (IN6_IS_ADDR_V4MAPPED(&s6->sin6_addr)) {
            a = mapped_v4(&s6->sin6_addr);
        } else {
            return 0;
        }
    } else {
        return 0;
    }
    if (own_addr(a)) {
        own = 1;
        a = htonl(INADDR_LOOPBACK);
    }
    if (!v4_local(a)) {
        return 0;
    }
    *any = a == htonl(INADDR_ANY);
    if (!port) {
        *zero = 1;
        return 0;
    }
    // A container's published port forwards to its own address, never to
    // its loopback, so a listener on loopback alone stays the container's.
    if (type == SOCK_STREAM && host_port(type, port) && !*any && !own) {
        *lo = 1;
        return port;
    }
    // A container port with a host port binds it, and so does a program
    // that binds the host port itself, as the container contract has it.
    if (host_port(type, port) || container_port(type, port)) {
        return 0;
    }
    return port;
}

// loopback_held reports whether a listener on loopback alone holds TCP
// container port port, where its host port could not be bound beside it,
// and removes the socket of one that ended.
static int loopback_held(unsigned short port) {
    struct sockaddr_un u;
    struct stat st;
    if (!port_dir[0] || port_path(SOCK_STREAM, port, &u) == -1 || stat(u.sun_path, &st) != 0 || !S_ISSOCK(st.st_mode)) {
        return 0;
    }
    int t = socket(AF_UNIX, SOCK_STREAM, 0);
    int live = t >= 0 && connect(t, (struct sockaddr *)&u, u.sun_len) == 0;
    if (t >= 0) close(t);
    if (!live) unlink(u.sun_path);
    return live;
}

// host_held reports whether a socket holds host port port on the loopback
// address sa names, by trying to bind it, which no listener sees.
static int host_held(const struct sockaddr *sa, unsigned short port) {
    if (sa->sa_family == AF_INET6 && IN6_IS_ADDR_LOOPBACK(&((const struct sockaddr_in6 *)sa)->sin6_addr)) {
        return bound_v6(SOCK_STREAM, port);
    }
    int s = socket(AF_INET, SOCK_STREAM, 0);
    if (s < 0) {
        return 1;
    }
    int on = 1;
    setsockopt(s, SOL_SOCKET, SO_REUSEADDR, &on, sizeof on);
    in_addr_t a = sa->sa_family == AF_INET ? ((const struct sockaddr_in *)sa)->sin_addr.s_addr : mapped_v4(&((const struct sockaddr_in6 *)sa)->sin6_addr);
    struct sockaddr_in b = v4_addr(a, port);
    int r = bind(s, (struct sockaddr *)&b, sizeof b), e = errno;
    close(s);
    return r == -1 && e == EADDRINUSE;
}

// holder returns the socket of this process other than fd that holds the
// Unix socket file u names, as one bound to IPv6's wildcard address holds a
// port a bind to IPv4's then asks for, or -1 for none.
static int holder(int fd, unsigned short port, const struct sockaddr_un *u) {
    for (int i = 0; i < NET_FDS; i++) {
        if (i == fd || !(net_state[i] & NET_HELD) || net_port[i] != port) continue;
        struct sockaddr_un b;
        socklen_t n = sizeof b;
        if (getsockname(i, (struct sockaddr *)&b, &n) == 0 && b.sun_family == AF_UNIX &&
            strncmp(b.sun_path, u->sun_path, sizeof b.sun_path) == 0) {
            return i;
        }
    }
    return -1;
}

// listen_unix puts a Unix socket of fd's type in its place and binds it
// where the library listens on port. With reclaim set, it takes over a
// socket file no listener holds.
static int listen_unix(int fd, unsigned short port, int any, int v6, int reclaim) {
    int type = sock_type(fd);
    struct sockaddr_un u;
    if (port_path(type, port, &u) == -1) {
        return -1;
    }
    // Whether the program made an IPv6 socket IPv6-only, which the Unix
    // socket in its place cannot say.
    int only = 0;
    if (v6 && !(state_of(fd) & NET_V6)) {
        socklen_t on = sizeof only;
        if (getsockopt(fd, IPPROTO_IPV6, IPV6_V6ONLY, &only, &on) != 0) only = 0;
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
    if (fd < NET_FDS) net_v6only[fd] = (unsigned char)(only != 0);
    int r = bind(fd, (struct sockaddr *)&u, u.sun_len);
    if (r == -1 && errno == EADDRINUSE && reclaim) {
        // A socket of this process holds the file: the probe below would
        // reach its listener as a connection no client made.
        int h = holder(fd, port, &u);
        if (h != -1) {
            // An IPv6 socket on the wildcard shares the port of the
            // program's IPv4 listener, as an IPv6-only one does on Linux,
            // nginx's listen [::]:80 beside its listen 80: fd takes the
            // holder's socket, whose connections either one accepts, as the
            // one Unix socket carries both families'. An IPv4 socket shares
            // an IPv6 listener's port where that one is IPv6-only; beside a
            // dual-stack one it fails, as on Linux.
            int hv6 = (net_state[h] & NET_V6) != 0;
            if (hv6 != v6 && (v6 ? any : net_v6only[h])) {
                if (dup2(h, fd) == -1) {
                    return -1;
                }
                if (fdfl != -1) fcntl(fd, F_SETFD, fdfl);
                set_state(fd, state_of(fd) | NET_HELD);
                return 0;
            }
            errno = EADDRINUSE;
            return -1;
        }
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
    if (r == 0) set_state(fd, state_of(fd) | NET_HELD);
    return r;
}

// listen_ephemeral binds fd to a port of its own, for a bind to port 0,
// as the kernel picks one: from the range macOS draws ephemeral ports
// from, one that no host port backs and no socket of the container holds.
// The kernel would bind the port itself, which Seatbelt allows only for
// the stack's ports, so the port is one with a Unix socket, as a container
// port without a host port is.
static int listen_ephemeral(int fd, int any, int v6) {
    int type = sock_type(fd);
    const unsigned first = 49152, span = 65536 - first;
    unsigned start = arc4random_uniform(span);
    for (unsigned i = 0; i < span; i++) {
        unsigned short p = (unsigned short)(first + (start + i) % span);
        struct sockaddr_un u;
        if (host_port(type, p) || container_port(type, p) || port_path(type, p, &u) == -1) continue;
        // A file there is another socket's, live or not; taking a stale
        // one could race the listener about to hold it.
        if (access(u.sun_path, F_OK) == 0) continue;
        if (listen_unix(fd, p, any, v6, 0) == 0) return 0;
        if (errno != EADDRINUSE) return -1;
    }
    errno = EADDRINUSE;
    return -1;
}

static int fws_bind(int fd, const struct sockaddr *sa, socklen_t len) {
    int uany, uv6, zero, lo;
    unsigned short uport = unbacked(fd, sa, len, &uany, &uv6, &zero, &lo);
    if (lo && host_held(sa, host_port(SOCK_STREAM, uport))) {
        errno = EADDRINUSE;
        return -1;
    }
    if (uport || zero) {
        int r = uport ? listen_unix(fd, uport, uany, uv6, 1) : listen_ephemeral(fd, uany, uv6);
        if (r == 0) set_state(fd, state_of(fd) | dest_p6(sa));
        return r;
    }
    struct sockaddr_storage b;
    unsigned char any;
    int dir = -1;
    socklen_t given = len;
    const struct sockaddr *to = local(fd, sa, &len, &b, &any, &dir);
    if (sa && !to) {
        return -1;
    }
    if (any && sock_type(fd) == SOCK_STREAM && loopback_held(sa->sa_family == AF_INET ? ntohs(((const struct sockaddr_in *)sa)->sin_port) : ntohs(((const struct sockaddr_in6 *)sa)->sin6_port))) {
        errno = EADDRINUSE;
        return -1;
    }
    if (dir != -1) {
        int r = in_dir(dir, bind, fd, to, len), e = errno;
        if (r == 0) name_unix(unix_names, fd, sa, given);
        errno = e;
        return r;
    }
    int r = bind(fd, to, len);
    if (r == 0 && any) set_state(fd, state_of(fd) | NET_ANY);
    return r;
}
INTERPOSE(fws_bind, bind);

// connect_dir connects fd to a Unix socket named relative to dir, which
// it closes; sa and given are the address the caller gave.
static int connect_dir(int dir, int fd, const struct sockaddr *sa, socklen_t given, const struct sockaddr *to, socklen_t len) {
    int r = in_dir(dir, connect, fd, to, len), e = errno;
    if (r == 0) name_unix(unix_peers, fd, sa, given);
    errno = e;
    return r;
}

// route connects fd through the router at loopback port router to the
// container port port of the member at addr, and reports whether it did:
// at once, as a connection to loopback is made, whether or not fd blocks.
// With foreign set, the router is the container's own, which relays to
// another stack's member.
static int route(int fd, in_addr_t addr, unsigned short router, unsigned short port, int v6, int foreign) {
    if (v6 && !(state_of(fd) & NET_V6) && to_ipv4(fd, SOCK_STREAM) == -1) {
        return -1;
    }
    int fl = fcntl(fd, F_GETFL);
    if (fl != -1 && (fl & O_NONBLOCK)) fcntl(fd, F_SETFL, fl & ~O_NONBLOCK);
    struct sockaddr_in r = v4_addr(htonl(INADDR_LOOPBACK), router);
    unsigned char req[2 + RELAY_KEY + 6] = {(unsigned char)(port >> 8), (unsigned char)port}, ans = 1;
    size_t n = 2;
    if (foreign) {
        const unsigned char *key = relay_key();
        if (!key) {
            if (fl != -1 && (fl & O_NONBLOCK)) fcntl(fd, F_SETFL, fl);
            errno = ECONNREFUSED;
            return -1;
        }
        req[0] = req[1] = 0xff;
        memcpy(req + 2, key, RELAY_KEY);
        memcpy(req + 2 + RELAY_KEY, &addr, 4);
        req[6 + RELAY_KEY] = (unsigned char)(port >> 8);
        req[7 + RELAY_KEY] = (unsigned char)port;
        n = sizeof req;
    }
    int ok = connect(fd, (struct sockaddr *)&r, sizeof r) == 0 && write(fd, req, n) == (ssize_t)n && read(fd, &ans, 1) == 1;
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
    int v6 = sa->sa_family == AF_INET6, p6 = 0;
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
        } else if (member6(&s6->sin6_addr) && port && v6_target(&s6->sin6_addr, &a) == 1) {
            p6 = 1;
        } else {
            return 0;
        }
    } else {
        return 0;
    }
    if (!port) {
        return 0;
    }
    if (member_block(a) && !own_addr(a)) {
        int member;
        unsigned short router = 0;
        unsigned short h = member_route(a, SOCK_STREAM, port, &member, &router);
        if (member == 2) {
            if (!router_port) return 0;
            *r = route(fd, a, router_port, port, v6, 1);
        } else if (h || !member || !router) {
            return 0;
        } else {
            *r = route(fd, a, router, port, v6, 0);
        }
        if (*r == 0 && p6) set_state(fd, state_of(fd) | NET_P6);
        return 1;
    }
    if ((v4_local(a) || own_addr(a)) && router_port && !(host_port(SOCK_STREAM, port) && own_addr(a))) {
        // Loopback reaches the container's own listener when it has one,
        // and on a port with a host port, its listener on loopback alone;
        // any other loopback port is the host's.
        struct sockaddr_un u;
        struct stat st;
        if (port_path(SOCK_STREAM, port, &u) == -1 || stat(u.sun_path, &st) != 0 || !S_ISSOCK(st.st_mode)) {
            return 0;
        }
        *r = route(fd, own_addr(a) ? a : htonl(INADDR_LOOPBACK), router_port, port, v6, 0);
        if (*r == 0 && v6 && IN6_IS_ADDR_LOOPBACK(&((const struct sockaddr_in6 *)sa)->sin6_addr)) {
            set_state(fd, state_of(fd) | NET_LO6);
        }
        if (*r == 0 && p6) set_state(fd, state_of(fd) | NET_P6);
        return 1;
    }
    return 0;
}

static int dgram_unix(int fd);
static int dgram_connect(int fd, const struct sockaddr *sa, socklen_t len);

static int dgram_dest(const struct sockaddr *sa, socklen_t len, in_addr_t *v4, unsigned short *port);

// relay_bind binds an unbound UDP socket about to reach another stack's
// member to an ephemeral port of the container's own, where the kernel
// would bind one of its own on the first datagram: the router's relay to
// the member takes datagrams from the container's ports alone.
static int relay_bind(int fd, const struct sockaddr *sa, socklen_t len) {
    if (!sa || !router_port || (state_of(fd) & NET_UNIX) || sock_type(fd) != SOCK_DGRAM) {
        return 0;
    }
    int e = errno;
    in_addr_t v4;
    unsigned short port, router = 0;
    int member = 0;
    if (dgram_dest(sa, len, &v4, &port) == 0 && member_block(v4)) member_route(v4, SOCK_DGRAM, port, &member, &router);
    if (member != 2) {
        errno = e;
        return 0;
    }
    struct sockaddr_storage b;
    socklen_t n = sizeof b;
    if (getsockname(fd, (struct sockaddr *)&b, &n) != 0) {
        return -1;
    }
    unsigned short bound = b.ss_family == AF_INET6 ? ((struct sockaddr_in6 *)&b)->sin6_port : ((struct sockaddr_in *)&b)->sin_port;
    if (bound) {
        errno = e;
        return 0;
    }
    if (listen_ephemeral(fd, 1, b.ss_family == AF_INET6) != 0) {
        return -1;
    }
    errno = e;
    return 0;
}

static int fws_connect(int fd, const struct sockaddr *sa, socklen_t len) {
    if (relay_bind(fd, sa, len) != 0) {
        return -1;
    }
    if (dgram_unix(fd)) {
        return dgram_connect(fd, sa, len);
    }
    int rr;
    if (routed(fd, sa, len, &rr)) {
        return rr;
    }
    struct sockaddr_storage b;
    int dir = -1;
    socklen_t given = len;
    const struct sockaddr *to = remote(fd, sa, &len, &b, &dir);
    if (sa && !to) {
        return -1;
    }
    return dir == -1 ? connect(fd, to, len) : connect_dir(dir, fd, sa, given, to, len);
}
INTERPOSE(fws_connect, connect);

extern int connect_nocancel(int, const struct sockaddr *, socklen_t) __asm("_connect$NOCANCEL") __attribute__((weak_import));
static int fws_connect_nocancel(int fd, const struct sockaddr *sa, socklen_t len) {
    if (relay_bind(fd, sa, len) != 0) {
        return -1;
    }
    if (dgram_unix(fd)) {
        return dgram_connect(fd, sa, len);
    }
    int rr;
    if (routed(fd, sa, len, &rr)) {
        return rr;
    }
    struct sockaddr_storage b;
    int dir = -1;
    socklen_t given = len;
    const struct sockaddr *to = remote(fd, sa, &len, &b, &dir);
    if (sa && !to) {
        return -1;
    }
    return dir == -1 ? connect_nocancel(fd, to, len) : connect_dir(dir, fd, sa, given, to, len);
}
INTERPOSE(fws_connect_nocancel, connect_nocancel);

// A UDP socket on a port without a host port is a Unix datagram one, whose
// datagrams the router relays with a header: the destination, as reach
// finds it, on the way out, and the source on the way in.
static int dgram_unix(int fd) { return (state_of(fd) & NET_UNIX) && sock_type(fd) == SOCK_DGRAM; }

typedef ssize_t (*sendmsg_fn)(int, const struct msghdr *, int);

// send_to sends m to its address as remote maps it, from the address's
// directory where the host path is longer than the address holds.
static ssize_t send_to(int fd, const struct msghdr *m, int flags, sendmsg_fn send) {
    struct msghdr c = *m;
    struct sockaddr_storage b;
    socklen_t len = c.msg_namelen;
    int dir = -1;
    c.msg_name = (void *)remote(fd, m->msg_name, &len, &b, &dir);
    if (!c.msg_name) {
        return -1;
    }
    c.msg_namelen = len;
    if (dir == -1) {
        return send(fd, &c, flags);
    }
    if (enter_dir(dir) != 0) {
        return -1;
    }
    ssize_t r = send(fd, &c, flags);
    leave_dir(dir);
    return r;
}
typedef ssize_t (*recvmsg_fn)(int, struct msghdr *, int);

// dgram_dest reads a datagram's destination as an IPv4 address and port.
static int dgram_dest(const struct sockaddr *sa, socklen_t len, in_addr_t *v4, unsigned short *port) {
    if (sa->sa_family == AF_INET && len >= (socklen_t)sizeof(struct sockaddr_in)) {
        const struct sockaddr_in *a = (const void *)sa;
        *v4 = a->sin_addr.s_addr;
        *port = ntohs(a->sin_port);
        return 0;
    }
    if (sa->sa_family == AF_INET6 && len >= (socklen_t)sizeof(struct sockaddr_in6)) {
        const struct sockaddr_in6 *a = (const void *)sa;
        *port = ntohs(a->sin6_port);
        if (IN6_IS_ADDR_V4MAPPED(&a->sin6_addr)) {
            *v4 = mapped_v4(&a->sin6_addr);
        } else if (IN6_IS_ADDR_LOOPBACK(&a->sin6_addr) || IN6_IS_ADDR_UNSPECIFIED(&a->sin6_addr)) {
            *v4 = htonl(INADDR_LOOPBACK);
        } else {
            int t = v6_target(&a->sin6_addr, v4);
            if (t != 1) {
                if (t == 0) errno = ENETUNREACH;
                return -1;
            }
        }
        return 0;
    }
    errno = EAFNOSUPPORT;
    return -1;
}

// A connected one keeps its peer, as reach finds it, for the datagrams it
// sends without a destination, and takes datagrams from that peer alone;
// one connected to AF_UNSPEC is no longer connected.
static int dgram_connect(int fd, const struct sockaddr *sa, socklen_t len) {
    if (!sa) {
        errno = EINVAL;
        return -1;
    }
    if (sa->sa_family == AF_UNSPEC) {
        set_state(fd, state_of(fd) & ~NET_DCONN);
        return 0;
    }
    in_addr_t v4, ra;
    unsigned short port, rp;
    if (dgram_dest(sa, len, &v4, &port) == -1 || reach(SOCK_DGRAM, v4, port, &ra, &rp) == -1) {
        return -1;
    }
    if (fd >= NET_FDS) {
        errno = EMFILE;
        return -1;
    }
    net_peer[fd] = ra;
    net_dport[fd] = rp;
    set_state(fd, state_of(fd) | NET_DCONN | dest_p6(sa));
    return 0;
}

static ssize_t dgram_send(int fd, const struct iovec *iov, int iovlen, int flags, const struct sockaddr *sa, socklen_t len, sendmsg_fn send) {
    int connected = state_of(fd) & NET_DCONN;
    if (sa && connected) {
        errno = EISCONN;
        return -1;
    }
    if (!sa && !connected) {
        errno = EDESTADDRREQ;
        return -1;
    }
    if (iovlen < 0 || iovlen > DGRAM_IOV) {
        errno = EMSGSIZE;
        return -1;
    }
    in_addr_t ra;
    unsigned short rp;
    if (connected) {
        ra = net_peer[fd];
        rp = net_dport[fd];
    } else {
        in_addr_t v4;
        unsigned short port;
        if (dgram_dest(sa, len, &v4, &port) == -1 || reach(SOCK_DGRAM, v4, port, &ra, &rp) == -1) {
            return -1;
        }
        set_state(fd, state_of(fd) | dest_p6(sa));
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
        int foreign = 0;
        if (r >= (ssize_t)sizeof hdr && (state_of(fd) & NET_DCONN)) {
            in_addr_t a;
            memcpy(&a, hdr, 4);
            foreign = a != net_peer[fd] || (unsigned short)(hdr[4] << 8 | hdr[5]) != net_dport[fd];
        }
        if (r < (ssize_t)sizeof hdr || foreign) {
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

static int fws_getsockname(int fd, struct sockaddr *sa, socklen_t *len);
static int member_block(in_addr_t a);
static int own_addr(in_addr_t a);
static in_addr_t own_seen_by(in_addr_t from);

// v4_of is the IPv4 address of an IPv4 or IPv4-mapped IPv6 address, with
// IPv6 loopback as IPv4's and any other IPv6 address as the wildcard.
static in_addr_t v4_of(const struct sockaddr_storage *s) {
    if (s->ss_family == AF_INET) return ((const struct sockaddr_in *)s)->sin_addr.s_addr;
    if (s->ss_family == AF_INET6) {
        const struct in6_addr *a = &((const struct sockaddr_in6 *)s)->sin6_addr;
        if (IN6_IS_ADDR_V4MAPPED(a)) return mapped_v4(a);
        if (IN6_IS_ADDR_LOOPBACK(a)) return htonl(INADDR_LOOPBACK);
    }
    return htonl(INADDR_ANY);
}

static void add_cmsg(struct msghdr *m, socklen_t cap, socklen_t *used, int level, int type, const void *data, size_t len) {
    if (!m->msg_control || *used + CMSG_SPACE(len) > cap) {
        m->msg_flags |= MSG_CTRUNC;
        return;
    }
    struct cmsghdr *c = (struct cmsghdr *)((char *)m->msg_control + *used);
    memset(c, 0, CMSG_SPACE(len));
    c->cmsg_len = (socklen_t)CMSG_LEN(len);
    c->cmsg_level = level;
    c->cmsg_type = type;
    memcpy(CMSG_DATA(c), data, len);
    *used += (socklen_t)CMSG_SPACE(len);
}

// dgram_control writes to m the control messages fd asks for, of a
// datagram from src. Its destination is the address the socket is bound
// to, or, bound to the wildcard, the container's member address for a
// member's datagram and loopback for any other: the kernel reports the
// address the sender named.
static void dgram_control(int fd, struct msghdr *m, const struct sockaddr_storage *src) {
    socklen_t cap = m->msg_controllen, used = 0;
    unsigned char want = fd < NET_FDS ? net_cmsg[fd] : 0;
    in_addr_t dst = htonl(INADDR_LOOPBACK);
    struct sockaddr_storage self;
    socklen_t sn = sizeof self;
    if (want && fws_getsockname(fd, (struct sockaddr *)&self, &sn) == 0 && v4_of(&self) != htonl(INADDR_ANY)) {
        dst = v4_of(&self);
    } else if (want) {
        in_addr_t from = v4_of(src);
        if (own_address && member_block(from) && !own_addr(from)) dst = own_seen_by(from);
    }
    static unsigned int lo0;
    if (want && !lo0) lo0 = if_nametoindex("lo0");
    if (want & CM_DSTADDR) {
        struct in_addr a = {dst};
        add_cmsg(m, cap, &used, IPPROTO_IP, IP_RECVDSTADDR, &a, sizeof a);
    }
    if (want & CM_PKTINFO) {
        struct in_pktinfo pi;
        memset(&pi, 0, sizeof pi);
        pi.ipi_ifindex = lo0;
        pi.ipi_spec_dst.s_addr = dst;
        pi.ipi_addr.s_addr = dst;
        add_cmsg(m, cap, &used, IPPROTO_IP, IP_PKTINFO, &pi, sizeof pi);
    }
    if (want & (CM_PKTINFO6 | CM_2292PKTINFO6)) {
        struct in6_pktinfo pi;
        memset(&pi, 0, sizeof pi);
        pi.ipi6_ifindex = lo0;
        pi.ipi6_addr.s6_addr[10] = pi.ipi6_addr.s6_addr[11] = 0xff;
        memcpy(&pi.ipi6_addr.s6_addr[12], &dst, 4);
        if (want & CM_PKTINFO6) add_cmsg(m, cap, &used, IPPROTO_IPV6, IPV6_PKTINFO, &pi, sizeof pi);
        if (want & CM_2292PKTINFO6) add_cmsg(m, cap, &used, IPPROTO_IPV6, IPV6_2292PKTINFO, &pi, sizeof pi);
    }
    m->msg_controllen = used;
}

// dgram_recvmsg receives a datagram on a Unix one for recvmsg, with its
// source and the control messages its socket asks for.
static ssize_t dgram_recvmsg(int fd, struct msghdr *m, int flags, recvmsg_fn recv) {
    struct sockaddr_storage src;
    socklen_t sn = sizeof src;
    ssize_t r = dgram_recv(fd, m->msg_iov, m->msg_iovlen, flags, (struct sockaddr *)&src, &sn, &m->msg_flags, recv);
    if (r < 0) {
        return r;
    }
    if (m->msg_name) {
        socklen_t n = sn < m->msg_namelen ? sn : m->msg_namelen;
        memcpy(m->msg_name, &src, n);
        m->msg_namelen = n;
    }
    dgram_control(fd, m, &src);
    return r;
}

static ssize_t plain_sendmsg(int fd, const struct msghdr *m, int flags) { return sendmsg(fd, m, flags); }
static ssize_t plain_recvmsg(int fd, struct msghdr *m, int flags) { return recvmsg(fd, m, flags); }

static ssize_t fws_sendto(int fd, const void *buf, size_t n, int flags, const struct sockaddr *sa, socklen_t len) {
    if (relay_bind(fd, sa, len) != 0) {
        return -1;
    }
    if (dgram_unix(fd)) {
        struct iovec v = {(void *)buf, n};
        return dgram_send(fd, &v, 1, flags, sa, len, plain_sendmsg);
    }
    if (!sa) {
        return sendto(fd, buf, n, flags, sa, len);
    }
    struct iovec v = {(void *)buf, n};
    struct msghdr m;
    memset(&m, 0, sizeof m);
    m.msg_name = (void *)sa;
    m.msg_namelen = len;
    m.msg_iov = &v;
    m.msg_iovlen = 1;
    return send_to(fd, &m, flags, plain_sendmsg);
}
INTERPOSE(fws_sendto, sendto);

static ssize_t fws_sendmsg(int fd, const struct msghdr *m, int flags) {
    if (m && relay_bind(fd, m->msg_name, m->msg_namelen) != 0) {
        return -1;
    }
    if (m && dgram_unix(fd)) {
        return dgram_send(fd, m->msg_iov, m->msg_iovlen, flags, m->msg_name, m->msg_namelen, plain_sendmsg);
    }
    if (!m || !m->msg_name) {
        return sendmsg(fd, m, flags);
    }
    return send_to(fd, m, flags, plain_sendmsg);
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
        return dgram_recvmsg(fd, m, flags, plain_recvmsg);
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

static ssize_t fws_send(int fd, const void *buf, size_t n, int flags) {
    if (dgram_unix(fd)) {
        struct iovec v = {(void *)buf, n};
        return dgram_send(fd, &v, 1, flags, NULL, 0, plain_sendmsg);
    }
    return send(fd, buf, n, flags);
}
INTERPOSE(fws_send, send);

static ssize_t fws_write(int fd, const void *buf, size_t n) {
    if (dgram_unix(fd)) {
        struct iovec v = {(void *)buf, n};
        return dgram_send(fd, &v, 1, 0, NULL, 0, plain_sendmsg);
    }
    return write(fd, buf, n);
}
INTERPOSE(fws_write, write);

static ssize_t fws_writev(int fd, const struct iovec *iov, int iovcnt) {
    if (dgram_unix(fd)) {
        return dgram_send(fd, iov, iovcnt, 0, NULL, 0, plain_sendmsg);
    }
    return writev(fd, iov, iovcnt);
}
INTERPOSE(fws_writev, writev);

static ssize_t fws_readv(int fd, const struct iovec *iov, int iovcnt) {
    if (dgram_unix(fd)) {
        return dgram_recv(fd, iov, iovcnt, 0, NULL, NULL, NULL, plain_recvmsg);
    }
    return readv(fd, iov, iovcnt);
}
INTERPOSE(fws_readv, readv);

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

// put writes the n bytes at p to the stream s, adding what it writes to
// *sent.
static int put(int s, const char *p, size_t n, off_t *sent) {
    while (n) {
        ssize_t w = write(s, p, n);
        if (w < 0) {
            if (errno == EINTR) continue;
            return -1;
        }
        p += w;
        n -= (size_t)w;
        *sent += w;
    }
    return 0;
}

// The kernel's sendfile fails with EPERM under a container's profile, and
// Java's FileChannel.transferTo, with which Kafka answers a fetch, takes
// that for an I/O error, so the library copies the file to the stream
// itself, with sendfile's contract: headers and file data up to *len bytes,
// or to the file's end when *len is 0, then the trailers, and *len set to
// what it wrote, also when a non-blocking stream fills.
static int fws_sendfile(int fd, int s, off_t offset, off_t *len, struct sf_hdtr *hdtr, int flags) {
    (void)flags;
    if (!len || offset < 0) {
        errno = EINVAL;
        return -1;
    }
    off_t budget = *len, sent = 0;
    *len = 0;
    struct stat st;
    if (fstat(fd, &st)) return -1;
    if (!S_ISREG(st.st_mode)) {
        errno = ENOTSUP;
        return -1;
    }
    int type;
    socklen_t tn = sizeof type;
    if (getsockopt(s, SOL_SOCKET, SO_TYPE, &type, &tn)) return -1;
    if (type != SOCK_STREAM) {
        errno = EINVAL;
        return -1;
    }
    int r = 0;
    for (int i = 0; hdtr && r == 0 && i < hdtr->hdr_cnt; i++) {
        size_t n = hdtr->headers[i].iov_len;
        if (budget && (off_t)n > budget - sent) n = (size_t)(budget - sent);
        r = put(s, hdtr->headers[i].iov_base, n, &sent);
    }
    char buf[32768];
    while (r == 0 && (!budget || sent < budget)) {
        size_t n = sizeof buf;
        if (budget && (off_t)n > budget - sent) n = (size_t)(budget - sent);
        ssize_t got = pread(fd, buf, n, offset);
        if (got < 0) {
            if (errno == EINTR) continue;
            r = -1;
            break;
        }
        if (got == 0) break;
        off_t before = sent;
        r = put(s, buf, (size_t)got, &sent);
        offset += sent - before;
    }
    for (int i = 0; hdtr && r == 0 && i < hdtr->trl_cnt; i++) {
        r = put(s, hdtr->trailers[i].iov_base, hdtr->trailers[i].iov_len, &sent);
    }
    *len = sent;
    return r;
}
INTERPOSE(fws_sendfile, sendfile);

// take_client reads the client's address and port, which the router
// writes before the stream of a connection it makes to a listener on a port
// without a host port. The library's own probes of a listener connect and
// write nothing: such a connection reaches the program as one its loopback
// client closed at once, as Linux hands over a connection its client
// abandoned.
static void take_client(int c) {
    unsigned char h[CLIENT_HEADER];
    size_t got = 0;
    while (got < sizeof h) {
        struct pollfd p = {c, POLLIN, 0};
        int r = poll(&p, 1, 10000);
        if (r == -1 && errno == EINTR) continue;
        if (r != 1) break;
        ssize_t n = recv(c, h + got, sizeof h - got, 0);
        if (n == -1 && (errno == EINTR || errno == EAGAIN)) continue;
        if (n <= 0) break;
        got += (size_t)n;
    }
    if (c >= NET_FDS) return;
    if (got < sizeof h) {
        net_peer[c] = htonl(INADDR_LOOPBACK);
        net_dport[c] = 0;
        return;
    }
    memcpy(&net_peer[c], h, 4);
    net_dport[c] = (unsigned short)(h[4] << 8 | h[5]);
}

// accepted completes a connection c that fd accepted, with k its peer as
// the kernel reports it.
static void accepted(int fd, int c, const struct sockaddr_storage *k, socklen_t kn, struct sockaddr *sa, socklen_t *len) {
    int e = errno;
    if ((state_of(fd) & NET_UNIX) && sock_type(fd) == SOCK_STREAM) take_client(c);
    set_peer(c, k);
    set_state(c, state_of(fd) & (NET_V6 | NET_UNIX));
    if (c < NET_FDS && fd < NET_FDS) {
        net_port[c] = net_port[fd];
        set_name(unix_names, c, unix_names[fd]);
    }
    present(c, k, kn, sa, len, 0);
    errno = e;
}

static int fws_accept(int fd, struct sockaddr *sa, socklen_t *len) {
    struct sockaddr_storage k;
    socklen_t kn = sizeof k;
    int c = accept(fd, (struct sockaddr *)&k, &kn);
    if (c >= 0) accepted(fd, c, &k, kn, sa, len);
    return c;
}
INTERPOSE(fws_accept, accept);

// Darwin refuses options on a socket shut down both ways, where Linux sets
// them on a TCP connection whose client closed; a listener's connections
// inherit this flag, which lifts that.
static int fws_listen(int fd, int backlog) {
    struct so_np_extensions x = {SONPX_SETOPTSHUT, SONPX_SETOPTSHUT};
    setsockopt(fd, SOL_SOCKET, SO_NP_EXTENSIONS, &x, sizeof x);
    return listen(fd, backlog);
}
INTERPOSE(fws_listen, listen);

static int fws_getsockname(int fd, struct sockaddr *sa, socklen_t *len) {
    struct sockaddr_storage k;
    socklen_t kn = sizeof k;
    int r = getsockname(fd, (struct sockaddr *)&k, &kn);
    if (r == 0) present(fd, &k, kn, sa, len, 1);
    return r;
}
INTERPOSE(fws_getsockname, getsockname);

static int fws_getpeername(int fd, struct sockaddr *sa, socklen_t *len) {
    if (dgram_unix(fd)) {
        if (!(state_of(fd) & NET_DCONN)) {
            errno = ENOTCONN;
            return -1;
        }
        struct sockaddr_in k4 = v4_addr(net_peer[fd], net_dport[fd]);
        struct sockaddr_storage k;
        memset(&k, 0, sizeof k);
        memcpy(&k, &k4, sizeof k4);
        present(fd, &k, sizeof k4, sa, len, 0);
        return 0;
    }
    struct sockaddr_storage k;
    socklen_t kn = sizeof k;
    int r = getpeername(fd, (struct sockaddr *)&k, &kn);
    // Darwin refuses the peer of a socket shut down both ways, where Linux
    // names a TCP connection's client after it closed: the library names
    // the client the router recorded, or the one accept named.
    if (r == -1 && errno == EINVAL && (state_of(fd) & (NET_UNIX | NET_HELD)) == NET_UNIX) {
        memset(&k, 0, sizeof k);
        k.ss_family = AF_UNIX;
        kn = sizeof k;
        r = 0;
    } else if (r == -1 && errno == EINVAL && fd >= 0 && fd < NET_FDS && acc_peers[fd]) {
        memcpy(&k, acc_peers[fd], sizeof k);
        kn = acc_peers[fd]->ss_len;
        r = 0;
    }
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
    int cf = fd >= 0 && fd < NET_FDS && dgram_unix(fd) ? cmsg_flag(level, name) : 0;
    if (cf) {
        if (!v || n < (socklen_t)sizeof(int)) {
            errno = EINVAL;
            return -1;
        }
        if (*(const int *)v) {
            net_cmsg[fd] |= (unsigned char)cf;
        } else {
            net_cmsg[fd] &= (unsigned char)~cf;
        }
        return 0;
    }
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
    int cf = fd >= 0 && fd < NET_FDS && dgram_unix(fd) ? cmsg_flag(level, name) : 0;
    if (cf && v && n && *n >= (socklen_t)sizeof(int)) {
        *(int *)v = (net_cmsg[fd] & cf) != 0;
        *n = sizeof(int);
        return 0;
    }
    if ((state_of(fd) & NET_UNIX) && (level == IPPROTO_TCP || level == IPPROTO_IP || level == IPPROTO_IPV6) && v && n &&
        *n >= (socklen_t)sizeof(int)) {
        *(int *)v = (level == IPPROTO_TCP && name == TCP_NODELAY) ||
                    (level == IPPROTO_IPV6 && name == IPV6_V6ONLY && fd < NET_FDS && net_v6only[fd]);
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
        net_dport[to] = net_dport[from];
        net_cmsg[to] = net_cmsg[from];
        set_name(unix_names, to, unix_names[from]);
        set_name(unix_peers, to, unix_peers[from]);
        set_peer(to, acc_peers[from]);
    }
}

// A descriptor's number outlives it, and the next socketpair, pipe or file
// to take it is none of the library's.
static void forget(int fd) {
    set_state(fd, 0);
    if (fd >= 0 && fd < NET_FDS) {
        net_cmsg[fd] = 0;
        set_name(unix_names, fd, NULL);
        set_name(unix_peers, fd, NULL);
        set_peer(fd, NULL);
    }
}

static int fws_close(int fd) {
    forget(fd);
    return close(fd);
}
INTERPOSE(fws_close, close);

extern int close_nocancel(int) __asm("_close$NOCANCEL") __attribute__((weak_import));
static int fws_close_nocancel(int fd) {
    forget(fd);
    return close_nocancel(fd);
}
INTERPOSE(fws_close_nocancel, close_nocancel);

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
    if (relay_bind(fd, sa, len) != 0) {
        return -1;
    }
    if (dgram_unix(fd)) {
        struct iovec v = {(void *)buf, n};
        return dgram_send(fd, &v, 1, flags, sa, len, nocancel_sendmsg);
    }
    if (!sa) {
        return sendto_nocancel(fd, buf, n, flags, sa, len);
    }
    struct iovec v = {(void *)buf, n};
    struct msghdr m;
    memset(&m, 0, sizeof m);
    m.msg_name = (void *)sa;
    m.msg_namelen = len;
    m.msg_iov = &v;
    m.msg_iovlen = 1;
    return send_to(fd, &m, flags, nocancel_sendmsg);
}
INTERPOSE(fws_sendto_nocancel, sendto_nocancel);

extern ssize_t sendmsg_nocancel(int, const struct msghdr *, int) __asm("_sendmsg$NOCANCEL") __attribute__((weak_import));
static ssize_t nocancel_sendmsg(int fd, const struct msghdr *m, int flags) { return sendmsg_nocancel(fd, m, flags); }

static ssize_t fws_sendmsg_nocancel(int fd, const struct msghdr *m, int flags) {
    if (m && relay_bind(fd, m->msg_name, m->msg_namelen) != 0) {
        return -1;
    }
    if (m && dgram_unix(fd)) {
        return dgram_send(fd, m->msg_iov, m->msg_iovlen, flags, m->msg_name, m->msg_namelen, nocancel_sendmsg);
    }
    if (!m || !m->msg_name) {
        return sendmsg_nocancel(fd, m, flags);
    }
    return send_to(fd, m, flags, nocancel_sendmsg);
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
        return dgram_recvmsg(fd, m, flags, nocancel_recvmsg);
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
    if (c >= 0) accepted(fd, c, &k, kn, sa, len);
    return c;
}
INTERPOSE(fws_accept_nocancel, accept_nocancel);

// Members.

static pthread_mutex_t members_lock = PTHREAD_MUTEX_INITIALIZER;
static char *members;
static struct stat members_st;

static const char *members_text(void);
static int in_subnets(const char *text, in_addr_t a);

// member_block reports whether a is an address of the stack's networks:
// the member block's, or one of a subnet a member's network has.
static int member_block(in_addr_t a) {
    if ((ntohl(a) >> 16) == (127u << 8 | 88u)) return 1;
    pthread_mutex_lock(&members_lock);
    int in = in_subnets(members_text(), a);
    pthread_mutex_unlock(&members_lock);
    return in;
}

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
    const char *names, *names_end, *ports, *ports_end, *nets, *nets_end, *links, *links_end, *subs, *subs_end;
    unsigned short router;
    int foreign;
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
            const char *t4 = t3 ? memchr(t3 + 1, '\t', (size_t)(end - t3 - 1)) : NULL;
            const char *t5 = t4 ? memchr(t4 + 1, '\t', (size_t)(end - t4 - 1)) : NULL;
            const char *t6 = t5 ? memchr(t5 + 1, '\t', (size_t)(end - t5 - 1)) : NULL;
            const char *t7 = t6 ? memchr(t6 + 1, '\t', (size_t)(end - t6 - 1)) : NULL;
            const char *t8 = t7 ? memchr(t7 + 1, '\t', (size_t)(end - t7 - 1)) : NULL;
            unsigned long r = 0;
            m->names = t1 + 1;
            m->names_end = t2;
            m->ports = t2 + 1;
            m->ports_end = t3 ? t3 : end;
            m->router = t3 && num(t3 + 1, &r) && r < 65536 ? (unsigned short)r : 0;
            m->nets = t4 ? t4 + 1 : end;
            m->nets_end = t5 ? t5 : end;
            m->foreign = t5 && t5[1] == '1';
            m->links = t7 ? t7 + 1 : end;
            m->links_end = t8 ? t8 : end;
            m->subs = t8 ? t8 + 1 : end;
            m->subs_end = end;
            return nl ? nl + 1 : end;
        }
        p = nl ? nl + 1 : end;
    }
    return NULL;
}

static int lower(int c) { return c >= 'A' && c <= 'Z' ? c + 32 : c; }

// same_name compares a name of n bytes at p with name, as DNS compares
// names: in any case, and with or without a final dot.
static int same_name(const char *p, size_t n, const char *name) {
    size_t k = strlen(name);
    if (k && name[k - 1] == '.') k--;
    if (n != k) return 0;
    for (size_t i = 0; i < n; i++) {
        if (lower(p[i]) != lower(name[i])) return 0;
    }
    return 1;
}

// link_of finds among a member's links, of space-separated viewer/alias
// pairs, the first that viewer makes, or with name, the one of that alias.
static int link_of(const struct member *m, in_addr_t viewer, const char *name) {
    for (const char *p = m->links; p < m->links_end;) {
        const char *e = memchr(p, ' ', (size_t)(m->links_end - p));
        if (!e) e = m->links_end;
        const char *after;
        in_addr_t v;
        if (parse_v4(p, &after, &v) && after < e && *after == '/' && v == viewer &&
            (!name || same_name(after + 1, (size_t)(e - after - 1), name))) {
            return 1;
        }
        p = e + 1;
    }
    return 0;
}

// answers reports whether a member answers to name: one of its
// space-separated names, or an alias a link of the container gives it.
static int answers(const struct member *m, const char *name) {
    for (const char *p = m->names; p < m->names_end;) {
        const char *e = memchr(p, ' ', (size_t)(m->names_end - p));
        if (!e) e = m->names_end;
        if (same_name(p, (size_t)(e - p), name)) return 1;
        p = e + 1;
    }
    return own_address && link_of(m, own_address, name);
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

// shares reports whether two members' space-separated networks meet.
static int shares(const struct member *a, const struct member *b) {
    for (const char *p = a->nets; p < a->nets_end;) {
        const char *e = memchr(p, ' ', (size_t)(a->nets_end - p));
        if (!e) e = a->nets_end;
        for (const char *q = b->nets; q < b->nets_end;) {
            const char *f = memchr(q, ' ', (size_t)(b->nets_end - q));
            if (!f) f = b->nets_end;
            if (e > p && e - p == f - q && memcmp(p, q, (size_t)(e - p)) == 0) return 1;
            q = f + 1;
        }
        p = e + 1;
    }
    return 0;
}

// next_sub reads at p, before end, a member's address on a network with
// a subnet, net=a.b.c.d/bits, and returns where the next one starts, or
// NULL when none is left.
static const char *next_sub(const char *p, const char *end, const char **net, size_t *netlen, in_addr_t *addr, in_addr_t *mask) {
    while (p < end) {
        const char *e = memchr(p, ' ', (size_t)(end - p));
        if (!e) e = end;
        const char *eq = memchr(p, '=', (size_t)(e - p)), *after;
        unsigned long bits;
        if (eq && eq > p && parse_v4(eq + 1, &after, addr) && after < e && *after == '/' && (after = num(after + 1, &bits)) && after == e &&
            bits <= 32) {
            *net = p;
            *netlen = (size_t)(eq - p);
            *mask = bits ? htonl(~0u << (32 - bits)) : 0;
            return e + 1;
        }
        p = e + 1;
    }
    return NULL;
}

// member_on is the member's address on the network of len bytes at net:
// its own there on a network with a subnet, else its member address.
static in_addr_t member_on(const struct member *m, const char *net, size_t len, in_addr_t *mask) {
    const char *n;
    size_t nl;
    in_addr_t a, k;
    for (const char *p = m->subs; (p = next_sub(p, m->subs_end, &n, &nl, &a, &k));) {
        if (nl == len && memcmp(n, net, len) == 0) {
            if (mask) *mask = k;
            return a;
        }
    }
    if (mask) *mask = htonl(MEMBER_MASK);
    return m->addr;
}

// member_has reports whether a is one of the member's addresses.
static int member_has(const struct member *m, in_addr_t a) {
    if (m->addr == a) return 1;
    const char *n;
    size_t nl;
    in_addr_t b, k;
    for (const char *p = m->subs; (p = next_sub(p, m->subs_end, &n, &nl, &b, &k));) {
        if (b == a) return 1;
    }
    return 0;
}

// on_net reports whether a member is on the network of len bytes at net.
static int on_net(const struct member *m, const char *net, size_t len) {
    for (const char *p = m->nets; p < m->nets_end;) {
        const char *e = memchr(p, ' ', (size_t)(m->nets_end - p));
        if (!e) e = m->nets_end;
        if ((size_t)(e - p) == len && memcmp(p, net, len) == 0) return 1;
        p = e + 1;
    }
    return 0;
}

// seen_addr is member m's address as viewer reaches it: on the first of
// its networks the viewer is on too, else its member address.
static in_addr_t seen_addr(const struct member *m, const struct member *viewer, int have_viewer) {
    if (!have_viewer) return m->addr;
    for (const char *p = m->nets; p < m->nets_end;) {
        const char *e = memchr(p, ' ', (size_t)(m->nets_end - p));
        if (!e) e = m->nets_end;
        if (e > p && on_net(viewer, p, (size_t)(e - p))) return member_on(m, p, (size_t)(e - p), NULL);
        p = e + 1;
    }
    return m->addr;
}

// in_subnets reports whether a lies in a subnet a member's network has.
static int in_subnets(const char *text, in_addr_t a) {
    struct member m;
    for (const char *p = text; text && (p = next_member(p, &m));) {
        const char *n;
        size_t nl;
        in_addr_t b, k;
        for (const char *q = m.subs; (q = next_sub(q, m.subs_end, &n, &nl, &b, &k));) {
            if ((a & k) == (b & k)) return 1;
        }
    }
    return 0;
}

// find_self finds the container's own line among the members in text.
static int find_self(const char *text, struct member *out) {
    struct member m;
    for (const char *p = text; own_address && (p = next_member(p, &m));) {
        if (m.addr == own_address) {
            *out = m;
            return 1;
        }
    }
    return 0;
}

// sees reports whether the container sees member m, as a container sees
// the containers on its networks and those it links to: m is the
// container itself, shares a network with it, or is linked to from it. A
// container the file does not list sees its own stack.
static int sees(const struct member *m, const struct member *me, int have_me) {
    if (!have_me) return !m->foreign;
    return m->addr == me->addr || shares(m, me) || link_of(m, me->addr, NULL);
}

// own reports whether a is one of the container's own addresses.
static int own_addr(in_addr_t a) {
    if (!own_address) return 0;
    if (a == own_address) return 1;
    pthread_mutex_lock(&members_lock);
    struct member me;
    int r = find_self(members_text(), &me) && member_has(&me, a);
    pthread_mutex_unlock(&members_lock);
    return r;
}

// own_seen_by is the container's address as the member at from reaches
// it.
static in_addr_t own_seen_by(in_addr_t from) {
    in_addr_t a = own_address;
    pthread_mutex_lock(&members_lock);
    struct member me, m;
    const char *text = members_text();
    if (find_self(text, &me)) {
        for (const char *p = text; (p = next_member(p, &m));) {
            if (member_has(&m, from)) {
                a = seen_addr(&me, &m, 1);
                break;
            }
        }
    }
    pthread_mutex_unlock(&members_lock);
    return a;
}

// next_sub6 reads at p, before end, a member's IPv6 address on a network
// with an IPv6 subnet, net=x:y::z/bits, and returns where the next one
// starts, or NULL when none is left.
static const char *next_sub6(const char *p, const char *end, const char **net, size_t *netlen, struct in6_addr *addr, int *bits) {
    while (p < end) {
        const char *e = memchr(p, ' ', (size_t)(end - p));
        if (!e) e = end;
        const char *eq = memchr(p, '=', (size_t)(e - p));
        const char *sl = eq ? memchr(eq, '/', (size_t)(e - eq)) : NULL;
        char text[INET6_ADDRSTRLEN];
        unsigned long b;
        if (eq && eq > p && sl && (size_t)(sl - eq - 1) < sizeof text && memchr(eq + 1, ':', (size_t)(sl - eq - 1))) {
            memcpy(text, eq + 1, (size_t)(sl - eq - 1));
            text[sl - eq - 1] = 0;
            if (inet_pton(AF_INET6, text, addr) == 1 && num(sl + 1, &b) == e && b <= 128) {
                *net = p;
                *netlen = (size_t)(eq - p);
                *bits = (int)b;
                return e + 1;
            }
        }
        p = e + 1;
    }
    return NULL;
}

// member_on6 finds the member's IPv6 address on the network of len bytes
// at net, with its subnet's length.
static int member_on6(const struct member *m, const char *net, size_t len, struct in6_addr *out, int *bits) {
    const char *n;
    size_t nl;
    struct in6_addr a;
    int b;
    for (const char *p = m->subs; (p = next_sub6(p, m->subs_end, &n, &nl, &a, &b));) {
        if (nl == len && memcmp(n, net, len) == 0) {
            if (out) *out = a;
            if (bits) *bits = b;
            return 1;
        }
    }
    return 0;
}

// member_has6 reports whether a is one of the member's IPv6 addresses.
static int member_has6(const struct member *m, const struct in6_addr *a) {
    const char *n;
    size_t nl;
    struct in6_addr b;
    int k;
    for (const char *p = m->subs; (p = next_sub6(p, m->subs_end, &n, &nl, &b, &k));) {
        if (IN6_ARE_ADDR_EQUAL(&b, a)) return 1;
    }
    return 0;
}

// seen_addr6 finds member m's IPv6 address as viewer reaches it: on the
// first of its networks the viewer is on too that gives it one.
static int seen_addr6(const struct member *m, const struct member *viewer, int have_viewer, struct in6_addr *out) {
    for (const char *p = m->nets; p < m->nets_end;) {
        const char *e = memchr(p, ' ', (size_t)(m->nets_end - p));
        if (!e) e = m->nets_end;
        if (e > p && (!have_viewer || on_net(viewer, p, (size_t)(e - p))) && member_on6(m, p, (size_t)(e - p), out, NULL)) return 1;
        p = e + 1;
    }
    return 0;
}

// in_prefix6 reports whether a lies in the bits-long prefix of b.
static int in_prefix6(const struct in6_addr *a, const struct in6_addr *b, int bits) {
    int whole = bits / 8, rest = bits % 8;
    if (memcmp(a->s6_addr, b->s6_addr, (size_t)whole) != 0) return 0;
    if (!rest) return 1;
    unsigned char mask = (unsigned char)(0xff << (8 - rest));
    return (a->s6_addr[whole] & mask) == (b->s6_addr[whole] & mask);
}

// member_of6 finds the member address of the member the container sees
// that holds IPv6 address a, its own included. It reports -1 where a lies
// in a subnet of a network the members are on and no member it sees
// holds it, and 0 where a lies in none.
static int member_of6(const struct in6_addr *a, in_addr_t *out) {
    int r = 0;
    pthread_mutex_lock(&members_lock);
    struct member m, me;
    const char *text = members_text();
    int have_me = find_self(text, &me);
    for (const char *p = text; text && (p = next_member(p, &m));) {
        const char *n;
        size_t nl;
        struct in6_addr b;
        int bits;
        for (const char *q = m.subs; (q = next_sub6(q, m.subs_end, &n, &nl, &b, &bits));) {
            if (IN6_ARE_ADDR_EQUAL(&b, a) && sees(&m, &me, have_me)) {
                *out = m.addr;
                r = 1;
                break;
            }
            if (in_prefix6(a, &b, bits)) r = -1;
        }
        if (r == 1) break;
    }
    pthread_mutex_unlock(&members_lock);
    return r;
}

// member_v6 finds the IPv6 address, as the container reaches it, of the
// member that holds IPv4 address a, its member address or one of its
// subnets'. The container's own is the one the member at peer reaches it
// at, else its first.
static int member_v6(in_addr_t a, in_addr_t peer, struct in6_addr *out) {
    int r = 0;
    pthread_mutex_lock(&members_lock);
    struct member m, me;
    const char *text = members_text();
    int have_me = find_self(text, &me);
    if (have_me && (a == htonl(INADDR_LOOPBACK) || member_has(&me, a))) {
        for (const char *p = text; peer && (p = next_member(p, &m));) {
            if (member_has(&m, peer)) {
                r = seen_addr6(&me, &m, 1, out);
                break;
            }
        }
        if (!r) r = seen_addr6(&me, &me, 0, out);
    } else {
        for (const char *p = text; text && (p = next_member(p, &m));) {
            if (member_has(&m, a) && sees(&m, &me, have_me)) {
                r = seen_addr6(&m, &me, have_me, out);
                break;
            }
        }
    }
    pthread_mutex_unlock(&members_lock);
    return r;
}

// own_addr6 reports whether a is one of the container's own IPv6
// addresses.
static int own_addr6(const struct in6_addr *a) {
    if (!own_address) return 0;
    pthread_mutex_lock(&members_lock);
    struct member me;
    int r = find_self(members_text(), &me) && member_has6(&me, a);
    pthread_mutex_unlock(&members_lock);
    return r;
}

// own_ifaces lists the container's interfaces, an address and its mask
// each, and an IPv6 address on a network with IPv6: its addresses on its
// networks, in their order, once each.
static int own_ifaces(struct iface *out) {
    int n = 0;
    if (!own_address) return 0;
    pthread_mutex_lock(&members_lock);
    struct member me;
    if (find_self(members_text(), &me)) {
        for (const char *p = me.nets; p < me.nets_end && n < MAX_IFACES;) {
            const char *e = memchr(p, ' ', (size_t)(me.nets_end - p));
            if (!e) e = me.nets_end;
            if (e > p) {
                struct iface f;
                memset(&f, 0, sizeof f);
                f.addr = member_on(&me, p, (size_t)(e - p), &f.mask);
                int has6 = member_on6(&me, p, (size_t)(e - p), &f.addr6, &f.bits6);
                // Networks without IPv4 subnets share the member address,
                // on one interface while they give no more IPv6 ones.
                int i = 0;
                while (i < n && !(out[i].addr == f.addr && (!has6 || !out[i].bits6))) i++;
                if (i == n) {
                    out[n++] = f;
                } else if (has6) {
                    out[i].addr6 = f.addr6;
                    out[i].bits6 = f.bits6;
                }
            }
            p = e + 1;
        }
    }
    pthread_mutex_unlock(&members_lock);
    if (n == 0) out[n++] = (struct iface){own_address, htonl(MEMBER_MASK), IN6ADDR_ANY_INIT, 0};
    return n;
}

static int member_address(const char *name, in_addr_t *out) {
    int found = 0;
    pthread_mutex_lock(&members_lock);
    struct member m, me;
    const char *text = members_text();
    int have_me = find_self(text, &me);
    for (const char *p = text; (p = next_member(p, &m));) {
        if (sees(&m, &me, have_me) && answers(&m, name)) {
            *out = seen_addr(&m, &me, have_me);
            found = 1;
            break;
        }
    }
    pthread_mutex_unlock(&members_lock);
    return found;
}

// member_address6 finds the IPv6 address, as the container reaches it,
// of the member that answers to name, where it holds one.
static int member_address6(const char *name, struct in6_addr *out) {
    int found = 0;
    pthread_mutex_lock(&members_lock);
    struct member m, me;
    const char *text = members_text();
    int have_me = find_self(text, &me);
    for (const char *p = text; (p = next_member(p, &m));) {
        if (sees(&m, &me, have_me) && answers(&m, name)) {
            found = seen_addr6(&m, &me, have_me, out);
            break;
        }
    }
    pthread_mutex_unlock(&members_lock);
    return found;
}

// member_route returns the host port behind a member's container port, and
// whether addr is a member the container sees, 2 for another stack's, and
// its router's port. Another stack's member's host ports lie outside the
// container's reach, so none is returned for one.
static unsigned short member_route(in_addr_t addr, int type, unsigned short cport, int *member, unsigned short *router) {
    unsigned short h = 0;
    *member = 0;
    pthread_mutex_lock(&members_lock);
    struct member m, me;
    const char *text = members_text();
    int have_me = find_self(text, &me);
    for (const char *p = text; (p = next_member(p, &m));) {
        if (member_has(&m, addr)) {
            if (!sees(&m, &me, have_me)) break;
            *member = m.foreign ? 2 : 1;
            h = m.foreign ? 0 : member_ports(&m, type, cport, 0);
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
    struct member m, me;
    const char *text = members_text();
    int have_me = find_self(text, &me);
    for (const char *p = text; (p = next_member(p, &m));) {
        unsigned short c = m.foreign ? 0 : member_ports(&m, type, hport, 1);
        if (c) {
            *addr = seen_addr(&m, &me, have_me);
            *cport = c;
            found = 1;
            break;
        }
    }
    pthread_mutex_unlock(&members_lock);
    return found;
}

// member_info is an entry of a member's address: IPv4 address a, as
// itself or mapped into IPv6 by family, or IPv6 address a6 where given.
static struct addrinfo *member_info(in_addr_t a, const struct in6_addr *a6, int family, int type, unsigned short port) {
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
        if (a6) {
            s.sin6_addr = *a6;
        } else {
            s.sin6_addr.s6_addr[10] = 0xff;
            s.sin6_addr.s6_addr[11] = 0xff;
            memcpy(&s.sin6_addr.s6_addr[12], &a, 4);
        }
        memcpy(ai->ai_addr, &s, sizeof s);
    }
    return ai;
}

#define HOSTS_MAX 8

// hosts_lookup reads the container's /etc/hosts, which the host's
// resolver never reads, as a resolver reads its own: it copies to addrs
// the addresses, up to HOSTS_MAX, of the lines that list name, and to
// canon the first name of the first, and returns how many it found.
static int hosts_lookup(const char *name, char addrs[HOSTS_MAX][INET6_ADDRSTRLEN], char *canon, size_t canon_size) {
    int fd = open(MAP("/etc/hosts"), O_RDONLY | O_CLOEXEC);
    if (fd < 0) return 0;
    struct stat st;
    char *b = NULL;
    ssize_t n = -1;
    if (fstat(fd, &st) == 0 && st.st_size < (1 << 24) && (b = malloc((size_t)st.st_size + 1))) {
        n = read(fd, b, (size_t)st.st_size);
    }
    close(fd);
    if (n < 0) {
        free(b);
        return 0;
    }
    b[n] = 0;
    int found = 0;
    for (char *line = b, *next; line && found < HOSTS_MAX; line = next) {
        next = strchr(line, '\n');
        if (next) *next++ = 0;
        char *hash = strchr(line, '#');
        if (hash) *hash = 0;
        char *save, *addr = strtok_r(line, " \t\r", &save);
        unsigned char bin[sizeof(struct in6_addr)];
        if (!addr || (inet_pton(AF_INET, addr, bin) != 1 && inet_pton(AF_INET6, addr, bin) != 1)) continue;
        const char *first = strtok_r(NULL, " \t\r", &save);
        for (const char *h = first; h; h = strtok_r(NULL, " \t\r", &save)) {
            if (same_name(h, strlen(h), name)) {
                if (!found) strlcpy(canon, first, canon_size);
                strlcpy(addrs[found++], addr, INET6_ADDRSTRLEN);
                break;
            }
        }
    }
    free(b);
    return found;
}

// hosts_addrinfo answers getaddrinfo from the container's /etc/hosts, as
// a resolver whose hosts file lists the name: what the host's resolver
// makes of each address it lists, of the family and type asked for.
static int hosts_addrinfo(const char *node, const char *service, const struct addrinfo *hints, struct addrinfo **res) {
    char addrs[HOSTS_MAX][INET6_ADDRSTRLEN], canon[256];
    int n = hosts_lookup(node, addrs, canon, sizeof canon);
    if (!n) return EAI_NONAME;
    struct addrinfo h = {0};
    if (hints) h = *hints;
    h.ai_flags = (h.ai_flags | AI_NUMERICHOST) & ~(AI_CANONNAME | AI_ADDRCONFIG);
    h.ai_canonname = NULL;
    h.ai_addr = NULL;
    h.ai_next = NULL;
    struct addrinfo *head = NULL, **tail = &head;
    int err = EAI_NONAME;
    for (int i = 0; i < n; i++) {
        struct addrinfo *r;
        if ((err = getaddrinfo(addrs[i], service, &h, &r)) != 0) continue;
        *tail = r;
        while (*tail) tail = &(*tail)->ai_next;
    }
    if (!head) return err == EAI_SERVICE || err == EAI_SOCKTYPE ? err : EAI_NONAME;
    if (hints && (hints->ai_flags & AI_CANONNAME)) {
        free(head->ai_canonname);
        if (!(head->ai_canonname = strdup(canon))) {
            freeaddrinfo(head);
            return EAI_MEMORY;
        }
    }
    *res = head;
    return 0;
}

// hosts_addr finds the first address of family af, IPv4 or IPv6, the
// container's /etc/hosts lists name at.
static int hosts_addr(const char *name, int af, void *out) {
    char addrs[HOSTS_MAX][INET6_ADDRSTRLEN], canon[256];
    int n = hosts_lookup(name, addrs, canon, sizeof canon);
    for (int i = 0; i < n; i++) {
        if (inet_pton(af, addrs[i], out) == 1) return 1;
    }
    return 0;
}

// A name resolves as a container's resolver resolves it: to the addresses
// its /etc/hosts lists it at, then a member's name to its member address,
// as Docker's DNS resolves a container on the network, and any other name
// to what the host resolves.
static int fws_getaddrinfo(const char *node, const char *service, const struct addrinfo *hints, struct addrinfo **res) {
    int flags = hints ? hints->ai_flags : 0;
    in_addr_t a;
    if (!node || (flags & AI_NUMERICHOST)) {
        return getaddrinfo(node, service, hints, res);
    }
    int herr = hosts_addrinfo(node, service, hints, res);
    if (herr != EAI_NONAME) return herr;
    if (!member_address(node, &a)) {
        return getaddrinfo(node, service, hints, res);
    }
    struct in6_addr a6;
    int has6 = member_address6(node, &a6);
    // The member's addresses, in the order RFC 6724 sorts them: a global
    // IPv6 address before IPv4, a unique local one after. An IPv6 caller
    // takes the IPv4 one mapped, when it asks to.
    struct maddr {
        int family;
        const struct in6_addr *a6;
    } addrs[2];
    int naddrs = 0, family = hints ? hints->ai_family : AF_UNSPEC;
    int global = has6 && (a6.s6_addr[0] & 0xe0) == 0x20;
    if (family == AF_UNSPEC) {
        if (global) addrs[naddrs++].family = AF_INET6;
        addrs[naddrs++].family = AF_INET;
        if (has6 && !global) addrs[naddrs++].family = AF_INET6;
        for (int i = 0; i < naddrs; i++) addrs[i].a6 = addrs[i].family == AF_INET6 ? &a6 : NULL;
    } else if (family == AF_INET6) {
        if (has6) addrs[naddrs++] = (struct maddr){AF_INET6, &a6};
        if ((flags & AI_V4MAPPED) && (!has6 || (flags & AI_ALL))) addrs[naddrs++] = (struct maddr){AF_INET6, NULL};
        if (!naddrs) return EAI_NONAME;
    } else if (family == AF_INET) {
        addrs[naddrs++] = (struct maddr){AF_INET, NULL};
    } else {
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
    for (int k = 0; k < naddrs; k++) {
        for (int i = 0; i < ntypes; i++) {
            struct addrinfo *ai = member_info(a, addrs[k].a6, addrs[k].family, types[i], port);
            if (!ai) {
                freeaddrinfo(head);
                return EAI_MEMORY;
            }
            *tail = ai;
            tail = &ai->ai_next;
        }
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
static char member_host_addr[16];
static char *member_host_addrs[2];
static char *member_host_aliases[1];

static struct hostent *member_hostent(const char *name, int af, const void *a) {
    int len = af == AF_INET6 ? 16 : 4;
    strlcpy(member_host_name, name, sizeof member_host_name);
    memcpy(member_host_addr, a, (size_t)len);
    member_host_addrs[0] = member_host_addr;
    member_host_addrs[1] = NULL;
    member_host_aliases[0] = NULL;
    member_host = (struct hostent){member_host_name, member_host_aliases, af, len, member_host_addrs};
    return &member_host;
}

static struct hostent *fws_gethostbyname(const char *name) {
    in_addr_t a;
    return name && (hosts_addr(name, AF_INET, &a) || member_address(name, &a)) ? member_hostent(name, AF_INET, &a) : gethostbyname(name);
}
INTERPOSE(fws_gethostbyname, gethostbyname);

static struct hostent *fws_gethostbyname2(const char *name, int af) {
    in_addr_t a;
    struct in6_addr a6;
    if (af == AF_INET && name && (hosts_addr(name, AF_INET, &a) || member_address(name, &a))) return member_hostent(name, AF_INET, &a);
    if (af == AF_INET6 && name && (hosts_addr(name, AF_INET6, &a6) || member_address6(name, &a6))) return member_hostent(name, AF_INET6, &a6);
    return gethostbyname2(name, af);
}
INTERPOSE(fws_gethostbyname2, gethostbyname2);
