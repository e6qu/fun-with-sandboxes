// fws-spawn starts a container's process, in the Go spawn helper's place:
// it applies the process's limits and Seatbelt profile to itself, starts
// the container's exec anchor where asked, and executes the command. A C
// program reaches its exec in a millisecond, where the Go helper's runtime
// took several, and every container start waits for it.
//
//   fws-spawn [-t] [-n nice] [-i iopolicy] [-q clamp] [-r resource:soft:hard]...
//             -N nproc -c compiled -L library [-a anchor]
//             -- path argv0 [arg]...
//
// -t makes standard input the controlling terminal; -q is the QoS clamp
// (POSIX_SPAWN_PROC_CLAMP_*) every thread of the command and of what it
// starts runs under, which the helper sets by executing the command with
// posix_spawn's POSIX_SPAWN_SETEXEC, the one exec that takes it; -N is the fork
// ceiling, the soft process limit the container's processes get at most
// (runtime.ForkCeiling); -c names the compiled profile, the bytecode of
// the container's Seatbelt profile after its text's SHA-256; -a starts the
// anchor, that binary run with this process's pid, on the listener at
// descriptor 4. The environment is the command's, which gets the container library
// -L names in DYLD_INSERT_LIBRARIES, which the helper itself does not load,
// and with -a FWS_INIT, its own pid. A failure is written to descriptor 3, which closes on a
// successful exec, and ends the helper with status 127.
#include <ctype.h>
#include <spawn.h>
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

int setiopolicy_np(int iotype, int scope, int policy);
int posix_spawnattr_set_qos_clamp_np(posix_spawnattr_t *attr, uint64_t clamp);

#ifndef POSIX_SPAWN_SETEXEC
#define POSIX_SPAWN_SETEXEC 0x0040
#endif

struct sandbox_profile {
    uint32_t type;
    const void *bytecode;
    size_t bytecode_length;
    uint64_t zero[5];
};
int sandbox_apply(struct sandbox_profile *profile);

enum { statusFD = 3, anchorFD = 4, hashLen = 32, rlimitNproc = 7 };

static void fail(const char *what, const char *detail) {
    char msg[1024];
    int n = snprintf(msg, sizeof msg, "%s: %s", what, detail);
    // As Go spells system errors: lower case.
    for (char *p = strstr(msg, ": "); p && p[2]; p = NULL) p[2] = (char)tolower((unsigned char)p[2]);
    if (n > 0) write(statusFD, msg, strlen(msg));
    _exit(127);
}

static void failErrno(const char *what) { fail(what, strerror(errno)); }

static char *slurp(const char *path, size_t *len) {
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return NULL;
    struct stat st;
    if (fstat(fd, &st) != 0) {
        close(fd);
        return NULL;
    }
    char *buf = malloc((size_t)st.st_size + 1);
    size_t got = 0;
    while (buf && got < (size_t)st.st_size) {
        ssize_t n = read(fd, buf + got, (size_t)st.st_size - got);
        if (n <= 0) break;
        got += (size_t)n;
    }
    close(fd);
    if (!buf) return NULL;
    buf[got] = 0;
    *len = got;
    return buf;
}

// applyCompiled applies the bytecode after the hash in the file at path.
static int applyCompiled(const char *path) {
    size_t len;
    char *b = slurp(path, &len);
    if (!b) return -1;
    if (len <= hashLen + 4) {
        errno = EINVAL;
        return -1;
    }
    struct sandbox_profile p;
    memset(&p, 0, sizeof p);
    memcpy(&p.type, b + hashLen, 4);
    p.bytecode = b + hashLen + 4;
    p.bytecode_length = len - hashLen - 4;
    return sandbox_apply(&p);
}

// forkCeiling lowers the soft process limit to ceiling, never raising it.
static void forkCeiling(unsigned long long ceiling) {
    struct rlimit rl;
    if (getrlimit(rlimitNproc, &rl) != 0) failErrno("getrlimit nproc");
    if (rl.rlim_cur <= ceiling) return;
    rl.rlim_cur = ceiling;
    if (setrlimit(rlimitNproc, &rl) != 0) failErrno("setrlimit nproc");
}

// startAnchor starts the anchor through a child that exits at once, so the
// anchor is no child of the main process this one becomes.
static void startAnchor(const char *anchor) {
    char mainpid[32];
    snprintf(mainpid, sizeof mainpid, "%d", (int)getpid());
    if (fcntl(anchorFD, F_SETFD, FD_CLOEXEC) != 0) failErrno("start the exec anchor");
    pid_t pid = fork();
    if (pid < 0) failErrno("start the exec anchor");
    if (pid == 0) {
        setsid();
        pid_t anchorPid = fork();
        if (anchorPid != 0) _exit(anchorPid < 0 ? 1 : 0);
        int null = open("/dev/null", O_RDWR);
        if (null < 0 || dup2(null, 0) < 0 || dup2(null, 1) < 0 || dup2(null, 2) < 0 || dup2(anchorFD, 3) < 0) _exit(1);
        char *argv[] = {"fws-anchor", mainpid, NULL};
        char *envp[] = {"FWS_ROLE=anchor", NULL};
        execve(anchor, argv, envp);
        _exit(127);
    }
    int ws;
    while (waitpid(pid, &ws, 0) < 0) {
        if (errno != EINTR) failErrno("start the exec anchor");
    }
    if (!WIFEXITED(ws) || WEXITSTATUS(ws) != 0) fail("start the exec anchor", "its launcher failed");
    close(anchorFD);
}

int main(int argc, char **argv) {
    int tty = 0, i = 1;
    const char *compiled = NULL, *anchor = NULL, *library = NULL;
    unsigned long long ceiling = 0;
    int nice = 0, haveNice = 0, iopolicy = 0, haveIO = 0, clamp = 0;
    struct { int res; struct rlimit rl; } limits[16];
    int nlimits = 0;
    for (; i < argc; i++) {
        const char *a = argv[i];
        if (strcmp(a, "--") == 0) {
            i++;
            break;
        }
        if (strcmp(a, "-t") == 0) {
            tty = 1;
            continue;
        }
        if (i + 1 >= argc) fail("fws-spawn", "an option without its value");
        const char *v = argv[++i];
        if (strcmp(a, "-n") == 0) {
            nice = atoi(v), haveNice = 1;
        } else if (strcmp(a, "-i") == 0) {
            iopolicy = atoi(v), haveIO = 1;
        } else if (strcmp(a, "-q") == 0) {
            clamp = atoi(v);
        } else if (strcmp(a, "-N") == 0) {
            ceiling = strtoull(v, NULL, 10);
        } else if (strcmp(a, "-c") == 0) {
            compiled = v;
        } else if (strcmp(a, "-a") == 0) {
            anchor = v;
        } else if (strcmp(a, "-L") == 0) {
            library = v;
        } else if (strcmp(a, "-r") == 0 && nlimits < 16) {
            char *end;
            limits[nlimits].res = (int)strtol(v, &end, 10);
            limits[nlimits].rl.rlim_cur = strtoull(end + 1, &end, 10);
            limits[nlimits].rl.rlim_max = strtoull(end + 1, &end, 10);
            nlimits++;
        } else {
            fail("fws-spawn", "an unknown option");
        }
    }
    if (i + 1 >= argc) fail("spawn helper", "no command");
    if (!compiled || !library || ceiling == 0) fail("spawn helper", "-c, -L and -N are required");
    const char *path = argv[i];
    char **cmd = argv + i + 1;

    if (tty && ioctl(0, TIOCSCTTY, 0) != 0) failErrno("set the controlling terminal");
    if (haveNice && setpriority(PRIO_PROCESS, 0, nice) != 0) {
        char what[64];
        snprintf(what, sizeof what, "setpriority %d", nice);
        failErrno(what);
    }
    if (haveIO && setiopolicy_np(0, 0, iopolicy) != 0) {
        char what[64];
        snprintf(what, sizeof what, "setiopolicy_np %d", iopolicy);
        failErrno(what);
    }
    for (int k = 0; k < nlimits; k++) {
        if (setrlimit(limits[k].res, &limits[k].rl) != 0) {
            // As runtime.RlimitName names them.
            static const char *names[] = {"cpu", "fsize", "data", "stack", "core", "as", "memlock", "nproc", "nofile"};
            char res[32], what[128];
            if (limits[k].res >= 0 && limits[k].res < (int)(sizeof names / sizeof *names)) {
                snprintf(res, sizeof res, "%s", names[limits[k].res]);
            } else {
                snprintf(res, sizeof res, "resource %d", limits[k].res);
            }
            snprintf(what, sizeof what, "setrlimit %s soft %llu hard %llu", res,
                     (unsigned long long)limits[k].rl.rlim_cur, (unsigned long long)limits[k].rl.rlim_max);
            failErrno(what);
        }
    }
    forkCeiling(ceiling);
    if (applyCompiled(compiled) != 0) failErrno("apply the compiled profile");

    fcntl(statusFD, F_SETFD, FD_CLOEXEC);
    if (anchor) startAnchor(anchor);
    size_t n = 0;
    while (environ[n]) n++;
    char **envp = calloc(n + 3, sizeof *envp);
    if (!envp) failErrno("environment");
    memcpy(envp, environ, n * sizeof *envp);
    {
        size_t len = strlen(library) + sizeof "DYLD_INSERT_LIBRARIES=";
        char *v = malloc(len);
        if (!v) failErrno("environment");
        snprintf(v, len, "DYLD_INSERT_LIBRARIES=%s", library);
        envp[n++] = v;
    }
    if (anchor) {
        char *init = malloc(32);
        if (!init) failErrno("environment");
        snprintf(init, 32, "FWS_INIT=%d", (int)getpid());
        envp[n++] = init;
    }
    char what[1100];
    snprintf(what, sizeof what, "exec %s", cmd[0]);
    posix_spawnattr_t attr;
    int err = posix_spawnattr_init(&attr);
    if (err == 0) err = posix_spawnattr_setflags(&attr, POSIX_SPAWN_SETEXEC);
    if (err != 0) fail("posix_spawnattr", strerror(err));
    if (clamp != 0 && (err = posix_spawnattr_set_qos_clamp_np(&attr, (uint64_t)clamp)) != 0) {
        char q[64];
        snprintf(q, sizeof q, "set the QoS clamp %d", clamp);
        fail(q, strerror(err));
    }
    // With POSIX_SPAWN_SETEXEC, posix_spawn returns only on failure.
    err = posix_spawn(NULL, path, NULL, &attr, cmd, envp);
    fail(what, strerror(err));
}
