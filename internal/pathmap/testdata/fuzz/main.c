// The path mapping's fuzzer: it maps paths it makes up at random, from the
// parts that trip a mapping up, through the library's fws_map, in a root
// whose links climb out of it, loop, and name it and a directory beside it
// by their host paths, beside a mount, under the sanitizers the build
// names. A path the root holds maps into the root or the mount's source,
// with no link on its way that leads out of them; one it does not hold
// maps to itself, resolved in the container, as the host's. A .. stays
// only past a component that names nothing, where resolution stops, as
// the kernel's does.
//
//   fuzz SECONDS [SEED]
//
// It prints its seed, and a path it faults on, and aborts.
#define FWS_FUZZ 1
#include "../../src/fwsroot.c"

#include <time.h>

static char base[PATH_MAX], rootdir[PATH_MAX], outside[PATH_MAX], msrc[PATH_MAX];

static void must(int ok, const char *what) {
    if (!ok) {
        fprintf(stderr, "fuzz setup: %s: %s\n", what, strerror(errno));
        exit(2);
    }
}

static void mkdir_in(const char *dir, const char *rel) {
    char p[PATH_MAX];
    snprintf(p, sizeof p, "%s/%s", dir, rel);
    must(mkdir(p, 0755) == 0, p);
}

static void file_in(const char *dir, const char *rel) {
    char p[PATH_MAX];
    snprintf(p, sizeof p, "%s/%s", dir, rel);
    int fd = open(p, O_CREAT | O_WRONLY, 0644);
    must(fd >= 0, p);
    close(fd);
}

static void link_in(const char *dir, const char *rel, const char *target) {
    char p[PATH_MAX];
    snprintf(p, sizeof p, "%s/%s", dir, rel);
    must(symlink(target, p) == 0, p);
}

static void setup(void) {
    char tmp[PATH_MAX];
    const char *t = getenv("TMPDIR");
    snprintf(tmp, sizeof tmp, "%s/fws-fuzz.XXXXXX", t && *t ? t : "/tmp");
    must(mkdtemp(tmp) != NULL, "mkdtemp");
    must(realpath(tmp, base) != NULL, "realpath");
    snprintf(rootdir, sizeof rootdir, "%s/root", base);
    snprintf(outside, sizeof outside, "%s/outside", base);
    snprintf(msrc, sizeof msrc, "%s/msrc", base);
    must(mkdir(rootdir, 0755) == 0 && mkdir(outside, 0755) == 0 && mkdir(msrc, 0755) == 0, "mkdir");
    mkdir_in(rootdir, "a");
    mkdir_in(rootdir, "a/b");
    mkdir_in(rootdir, "a/b/c");
    mkdir_in(rootdir, "etc");
    file_in(rootdir, "file");
    file_in(rootdir, "a/b/c/file");
    mkdir_in(outside, "secret");
    file_in(outside, "secret/file");
    mkdir_in(msrc, "in");
    file_in(msrc, "in/file");
    link_in(rootdir, "up", "../../..");
    link_in(rootdir, "a/b/up", "../../../..");
    link_in(rootdir, "a/esc", "../../../outside");
    link_in(rootdir, "abs", "/a/b");
    link_in(rootdir, "loop", "loop");
    link_in(rootdir, "self", ".");
    link_in(rootdir, "dangling", "/nonexistent/file");
    link_in(rootdir, "deep", "a/b/c/../../b/up");
    link_in(rootdir, "hostpath", outside);
    link_in(rootdir, "toroot", rootdir);
    link_in(rootdir, "tomount", "/mnt/in");
    // The engine places a mount as a link to its source.
    link_in(rootdir, "mnt", msrc);
    link_in(msrc, "in/up", "../../..");
    link_in(msrc, "in/back", "/a/b/c");
    must(chdir(rootdir) == 0, "chdir");
    strlcpy(root, rootdir, sizeof root);
    rootlen = strlen(root);
    strlcpy(croot, rootdir, sizeof croot);
    char m[2 * PATH_MAX];
    snprintf(m, sizeof m, "/mnt\t%s", msrc);
    parse_mounts(m);
}

static const char *const parts[] = {"a", "b", "c", "..", ".", "", "up", "abs", "loop", "self", "dangling", "deep", "hostpath", "toroot",
                                    "tomount", "mnt", "in", "back", "esc", "file", "etc", "outside", "secret", "root", "msrc", "tmp",
                                    "private", "fws-fuzz", "x"};
#define NPARTS (sizeof parts / sizeof parts[0])

static uint64_t rng;
static uint32_t next(void) {
    rng ^= rng << 13;
    rng ^= rng >> 7;
    rng ^= rng << 17;
    return (uint32_t)rng;
}

// make writes a path of up to 12 parts to out: absolute mostly, relative
// to the root's a now and then, with a base part's host path at times.
static void make(char *out, size_t size) {
    size_t n = 0;
    out[0] = 0;
    uint32_t kind = next() % 10;
    if (kind == 0) {
        n = (size_t)snprintf(out, size, "%s/", base);
    } else if (kind > 1) {
        out[n++] = '/';
        out[n] = 0;
    }
    int k = 1 + (int)(next() % 12);
    for (int i = 0; i < k && n + 64 < size; i++) {
        const char *part = parts[next() % NPARTS];
        n += (size_t)snprintf(out + n, size - n, "%s%s", part, next() % 8 == 0 ? "//" : "/");
    }
    if (n > 1 && next() % 2) out[--n] = 0;
}

// under_dir reports whether path p is dir or below it.
static int under_dir(const char *p, const char *dir) {
    size_t n = strlen(dir);
    return strncmp(p, dir, n) == 0 && (p[n] == '/' || p[n] == 0);
}

// lands reports where the longest existing part of q leads, following its
// links, in out; 0 where none of it does, or a link loops.
static int lands(const char *q, char *out) {
    char p[PATH_MAX];
    strlcpy(p, q, sizeof p);
    for (;;) {
        if (realpath(p, out)) return 1;
        if (errno == ELOOP) return 0;
        char *s = strrchr(p, '/');
        if (!s || s == p) return 0;
        *s = 0;
    }
}

// dotdot returns where path p's first .. component starts, at its
// slash, or -1 where it holds none.
static long dotdot(const char *p) {
    for (const char *s = p; (s = strstr(s, "/..")); s++) {
        if (s[3] == '/' || s[3] == 0) return s - p;
    }
    return -1;
}

// stops reports whether the kernel stops before path p's first ..: the
// part before it names nothing on the host, as when resolution stopped at
// a component the root lacks, which it leaves as it is. prefix, if not
// NULL, goes before p's part.
static int stops(const char *p, const char *prefix) {
    long i = dotdot(p);
    if (i < 0) return 1;
    char part[PATH_MAX];
    struct stat st;
    snprintf(part, sizeof part, "%s%.*s", prefix ? prefix : "", (int)i, p);
    return lstat(part, &st) != 0;
}

static const char *fault(const char *p, int follow, const char *q) {
    if (q == p) return NULL;
    int in_root_dir = under_dir(q, rootdir), in_mount = under_dir(q, msrc);
    if (!in_root_dir && !in_mount) {
        if (q[0] != '/') return "maps to a relative path";
        if (!stops(q, rootdir)) return "keeps a .. past a file the root holds";
        return NULL;
    }
    if (!stops(q, NULL)) return "keeps a .. past a file in the root";
    char dir[PATH_MAX], land[PATH_MAX];
    strlcpy(dir, q, sizeof dir);
    // Without follow a link the final component names stays, so its
    // directory is what must stay in: the root's or the mount source's
    // own, where the path is one of them.
    if (!follow) {
        char *s = strrchr(dir, '/');
        if (s && s > dir) *s = 0;
        if (!under_dir(dir, rootdir) && !under_dir(dir, msrc)) strlcpy(dir, q, sizeof dir);
    }
    if (lands(dir, land) && !under_dir(land, rootdir) && !under_dir(land, msrc)) return "leads out of the root through a link";
    return NULL;
}

int main(int argc, char **argv) {
    if (argc < 2) {
        fprintf(stderr, "usage: fuzz SECONDS [SEED]\n");
        return 2;
    }
    double secs = atof(argv[1]);
    rng = argc > 2 ? strtoull(argv[2], NULL, 10) : (uint64_t)time(NULL) ^ (uint64_t)getpid() << 32;
    if (!rng) rng = 1;
    printf("seed %llu\n", (unsigned long long)rng);
    fflush(stdout);
    setup();
    struct timespec start, now;
    clock_gettime(CLOCK_MONOTONIC, &start);
    unsigned long runs = 0;
    for (;;) {
        for (int i = 0; i < 256; i++, runs++) {
            char p[PATH_MAX / 2], buf[PATH_MAX];
            make(p, sizeof p);
            for (int follow = 0; follow < 2; follow++) {
                const char *q = fws_map(p, buf, follow);
                const char *why = q ? fault(p, follow, q) : NULL;
                if (why) {
                    fprintf(stderr, "path %s (follow %d) %s: %s\n", p, follow, why, q);
                    abort();
                }
            }
        }
        clock_gettime(CLOCK_MONOTONIC, &now);
        if ((double)(now.tv_sec - start.tv_sec) + (double)(now.tv_nsec - start.tv_nsec) / 1e9 >= secs) break;
    }
    printf("%lu paths\n", runs);
    return 0;
}
