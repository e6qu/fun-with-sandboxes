// jetsam asks what macOS lets an unprivileged process do with the jetsam
// memory limits of itself and its children, which the engine's --memory
// would rest on. Each line it prints answers one question:
//
//   kill:      a child spawned with a fatal 64 MiB limit touches up to 512
//              MiB in 16 MiB steps; is it killed?
//   spawned:   the limit the child reads back on itself;
//   fork:      the limit of a child that child forks;
//   exec:      after that fork child executes a program;
//   spawn:     of a program the child posix_spawns without attributes;
//   setexec:   after the child replaces itself through posix_spawn's
//              POSIX_SPAWN_SETEXEC, without attributes;
//   setexec+:  after an unlimited process replaces itself through
//              POSIX_SPAWN_SETEXEC with a 96 MiB limit;
//   self-set:  whether an unlimited process may set its own limit;
//   self-raise: whether a limited one may raise its own, from 64 to 128 MiB;
//   self-lower: or lower it, from 64 to 48 MiB;
//   child-set: whether a parent may set a running child's limit.
//
// A limit reads as active/inactive MiB, each with F when fatal, or -1 for
// none.
#include <errno.h>
#include <signal.h>
#include <spawn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

#ifndef POSIX_SPAWN_SETEXEC
#define POSIX_SPAWN_SETEXEC 0x0040
#endif

// spawn_private.h and kern_memorystatus.h, which the SDK does not ship.
int posix_spawnattr_setjetsam_ext(posix_spawnattr_t *attr, short flags, int priority, int memlimit_active, int memlimit_inactive);
int memorystatus_control(uint32_t command, int32_t pid, uint32_t flags, void *buffer, size_t buffersize);
typedef struct {
    int32_t memlimit_active;
    uint32_t memlimit_active_attr;
    int32_t memlimit_inactive;
    uint32_t memlimit_inactive_attr;
} memlimit_properties;
enum {
    JETSAM_SET = 0x8000,
    MEMLIMIT_ACTIVE_FATAL = 0x04,
    MEMLIMIT_INACTIVE_FATAL = 0x08,
    PRIORITY_DEFAULT = 180,
    CMD_SET_MEMLIMIT_PROPERTIES = 7,
    CMD_GET_MEMLIMIT_PROPERTIES = 8,
    MEMLIMIT_ATTR_FATAL = 0x1,
    LIMIT_MIB = 64,
    STEP_MIB = 16,
    MAX_MIB = 512,
};

static char *self;

static void limits(char *out, size_t n, pid_t pid) {
    memlimit_properties p;
    memset(&p, 0, sizeof p);
    if (memorystatus_control(CMD_GET_MEMLIMIT_PROPERTIES, pid, 0, &p, sizeof p) != 0) {
        snprintf(out, n, "unreadable (%s)", strerror(errno));
        return;
    }
    snprintf(out, n, "%d%s/%d%s", p.memlimit_active, p.memlimit_active_attr & MEMLIMIT_ATTR_FATAL ? "F" : "",
             p.memlimit_inactive, p.memlimit_inactive_attr & MEMLIMIT_ATTR_FATAL ? "F" : "");
}

static void report(const char *what) {
    char l[128];
    limits(l, sizeof l, 0);
    printf("%s: %s\n", what, l);
}

static int set(pid_t pid, int mib) {
    memlimit_properties p = {mib, MEMLIMIT_ATTR_FATAL, mib, MEMLIMIT_ATTR_FATAL};
    return memorystatus_control(CMD_SET_MEMLIMIT_PROPERTIES, pid, 0, &p, sizeof p) == 0 ? 0 : errno;
}

static void attrs(posix_spawnattr_t *a, short flags, int mib) {
    posix_spawnattr_init(a);
    if (flags) posix_spawnattr_setflags(a, flags);
    if (mib) {
        posix_spawnattr_setjetsam_ext(a, (short)(JETSAM_SET | MEMLIMIT_ACTIVE_FATAL | MEMLIMIT_INACTIVE_FATAL),
                                      PRIORITY_DEFAULT, mib, mib);
    }
}

static int wait_for(pid_t pid) {
    int ws;
    while (waitpid(pid, &ws, 0) < 0) {
        if (errno != EINTR) return -1;
    }
    return ws;
}

// run starts this program with mode as a child, with a fatal limit of mib
// MiB unless mib is 0, and waits for it.
static int run(const char *mode, int mib) {
    posix_spawnattr_t a;
    attrs(&a, 0, mib);
    char *argv[] = {self, (char *)mode, NULL};
    pid_t pid;
    fflush(stdout);
    int err = posix_spawn(&pid, self, NULL, &a, argv, environ);
    posix_spawnattr_destroy(&a);
    if (err) {
        printf("%s: posix_spawn: %s\n", mode, strerror(err));
        return -1;
    }
    return wait_for(pid);
}

static int hog(void) {
    for (int mib = STEP_MIB; mib <= MAX_MIB; mib += STEP_MIB) {
        char *p = malloc((size_t)STEP_MIB << 20);
        if (!p) return 2;
        memset(p, 1, (size_t)STEP_MIB << 20);
    }
    return 0;
}

// tree runs in a child spawned with a limit: it reports its own, a fork's,
// an exec's and a plain spawn's, and then replaces itself.
static int tree(void) {
    report("spawned");
    fflush(stdout);
    pid_t pid = fork();
    if (pid == 0) {
        report("fork");
        fflush(stdout);
        char *argv[] = {self, "report-exec", NULL};
        execv(self, argv);
        _exit(127);
    }
    wait_for(pid);
    run("report-spawn", 0);
    posix_spawnattr_t a;
    attrs(&a, POSIX_SPAWN_SETEXEC, 0);
    char *argv[] = {self, "report-setexec", NULL};
    fflush(stdout);
    int err = posix_spawn(NULL, self, NULL, &a, argv, environ);
    printf("setexec: posix_spawn: %s\n", strerror(err));
    return 1;
}

// setexec replaces this unlimited process with a 96 MiB one.
static int setexec(void) {
    posix_spawnattr_t a;
    attrs(&a, POSIX_SPAWN_SETEXEC, 96);
    char *argv[] = {self, "report-setexec+", NULL};
    fflush(stdout);
    int err = posix_spawn(NULL, self, NULL, &a, argv, environ);
    printf("setexec+: posix_spawn: %s\n", strerror(err));
    return 1;
}

static int self_set(const char *what, int mib) {
    int err = set(0, mib);
    char l[128];
    limits(l, sizeof l, 0);
    printf("%s: %s, now %s\n", what, err ? strerror(err) : "allowed", l);
    return 0;
}

int main(int argc, char **argv) {
    setvbuf(stdout, NULL, _IONBF, 0);
    self = argv[0];
    if (argc > 1) {
        const char *m = argv[1];
        if (strcmp(m, "hog") == 0) return hog();
        if (strcmp(m, "tree") == 0) return tree();
        if (strcmp(m, "setexec") == 0) return setexec();
        if (strncmp(m, "report-", 7) == 0) {
            report(m + 7);
            return 0;
        }
        if (strcmp(m, "self-set") == 0) return self_set(m, 80);
        if (strcmp(m, "self-raise") == 0) return self_set(m, 128);
        if (strcmp(m, "self-lower") == 0) return self_set(m, 48);
        if (strcmp(m, "sleep") == 0) {
            sleep(5);
            return 0;
        }
        return 2;
    }

    int ws = run("hog", LIMIT_MIB);
    if (ws < 0) {
        printf("result: macOS refused the spawn\n");
        return 0;
    }
    if (WIFSIGNALED(ws)) {
        printf("kill: killed by signal %d\n", WTERMSIG(ws));
    } else {
        printf("kill: exited %d\n", WEXITSTATUS(ws));
    }
    run("tree", LIMIT_MIB);
    run("setexec", 0);
    run("self-set", 0);
    run("self-raise", LIMIT_MIB);
    run("self-lower", LIMIT_MIB);

    posix_spawnattr_t a;
    attrs(&a, 0, LIMIT_MIB);
    char *sargv[] = {self, "sleep", NULL};
    pid_t pid;
    if (posix_spawn(&pid, self, NULL, &a, sargv, environ) == 0) {
        int err = set(pid, 128);
        char l[128];
        limits(l, sizeof l, pid);
        printf("child-set: %s, now %s\n", err ? strerror(err) : "allowed", l);
        kill(pid, SIGKILL);
        wait_for(pid);
    }
    printf("result: done\n");
    return 0;
}
