// jetsam asks what macOS lets an unprivileged process do with the jetsam
// memory limits of its children, which the engine's --memory rests on. A
// "hog" touches up to 512 MiB in 16 MiB steps; under a fatal 64 MiB limit
// the kernel kills it. Each line answers one question by how a hog ended:
//
//   kill:      a hog spawned with the limit;
//   fork:      a hog forked by a process spawned with the limit;
//   exec:      a hog executed by such a fork;
//   spawn:     a hog such a process posix_spawns without attributes;
//   setexec:   a hog such a process replaces itself with through
//              posix_spawn's POSIX_SPAWN_SETEXEC, without attributes;
//   setexec+:  a hog an unlimited process replaces itself with through
//              POSIX_SPAWN_SETEXEC with the limit, as the spawn helper would;
//   control:   whether memorystatus_control reads a process's own limit;
//   exit-parent: what kqueue's NOTE_EXIT_DETAIL tells the parent of a
//              process killed at its limit (NOTE_EXIT_MEMORY is 0x20000);
//   exit-other: and what it tells a process that is not its parent.
//
// A hog the kernel kills at the limit ends "killed by signal 9"; one that
// holds no limit ends "exited 0".
#include <errno.h>
#include <signal.h>
#include <spawn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/event.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

#ifndef POSIX_SPAWN_SETEXEC
#define POSIX_SPAWN_SETEXEC 0x0040
#endif

// spawn_private.h and kern_memorystatus.h, which the SDK does not ship.
int posix_spawnattr_setjetsam_ext(posix_spawnattr_t *attr, short flags, int priority, int memlimit_active, int memlimit_inactive);
int memorystatus_control(uint32_t command, int32_t pid, uint32_t flags, void *buffer, size_t buffersize);
enum {
    JETSAM_SET = 0x8000,
    MEMLIMIT_ACTIVE_FATAL = 0x04,
    MEMLIMIT_INACTIVE_FATAL = 0x08,
    PRIORITY_DEFAULT = 180,
    CMD_GET_MEMLIMIT_PROPERTIES = 8,
    LIMIT_MIB = 64,
    STEP_MIB = 16,
    MAX_MIB = 512,
};

static char *self;

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

static void ended(const char *what, int ws) {
    if (ws < 0) {
        printf("%s: not started\n", what);
    } else if (WIFSIGNALED(ws)) {
        printf("%s: killed by signal %d\n", what, WTERMSIG(ws));
    } else {
        printf("%s: exited %d\n", what, WEXITSTATUS(ws));
    }
}

// start starts this program with mode as a child, with a fatal limit of
// mib MiB unless mib is 0, and reports its pid, or -1.
static pid_t start(const char *mode, int mib) {
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
    return pid;
}

static int run(const char *mode, int mib) {
    pid_t pid = start(mode, mib);
    return pid < 0 ? -1 : wait_for(pid);
}

static int hog(void) {
    for (int mib = STEP_MIB; mib <= MAX_MIB; mib += STEP_MIB) {
        char *p = malloc((size_t)STEP_MIB << 20);
        if (!p) return 2;
        memset(p, 1, (size_t)STEP_MIB << 20);
    }
    return 0;
}

// setexec replaces this process with a hog through POSIX_SPAWN_SETEXEC,
// with a limit of mib MiB unless mib is 0.
static int setexec(int mib) {
    posix_spawnattr_t a;
    attrs(&a, POSIX_SPAWN_SETEXEC, mib);
    char *argv[] = {self, "hog", NULL};
    fflush(stdout);
    int err = posix_spawn(NULL, self, NULL, &a, argv, environ);
    printf("setexec: posix_spawn: %s\n", strerror(err));
    return 3;
}

// watch waits for pid's exit through kqueue, asking for its detail, and
// prints what the event carries.
static void watch(const char *what, pid_t pid) {
    int kq = kqueue();
    struct kevent ev;
    EV_SET(&ev, pid, EVFILT_PROC, EV_ADD | EV_ONESHOT, NOTE_EXIT | NOTE_EXIT_DETAIL, 0, NULL);
    if (kevent(kq, &ev, 1, NULL, 0, NULL) != 0) {
        printf("%s: kevent: %s\n", what, strerror(errno));
        close(kq);
        return;
    }
    struct kevent got;
    struct timespec ts = {30, 0};
    int n = kevent(kq, NULL, 0, &got, 1, &ts);
    if (n == 1) {
        printf("%s: fflags 0x%x data 0x%lx, memory %s\n", what, got.fflags, (long)got.data,
               (got.fflags & NOTE_EXIT_DETAIL) && (got.data & NOTE_EXIT_DETAIL_MASK) == NOTE_EXIT_MEMORY ? "yes" : "no");
    } else {
        printf("%s: no event (%d)\n", what, n);
    }
    close(kq);
}

int main(int argc, char **argv) {
    setvbuf(stdout, NULL, _IONBF, 0);
    self = argv[0];
    if (argc > 1) {
        const char *m = argv[1];
        if (strcmp(m, "hog") == 0) return hog();
        if (strcmp(m, "hog-late") == 0) {
            sleep(1);
            return hog();
        }
        // The children of a process spawned with the limit.
        if (strcmp(m, "fork") == 0) {
            pid_t pid = fork();
            if (pid == 0) _exit(hog());
            ended("fork", wait_for(pid));
            return 0;
        }
        if (strcmp(m, "exec") == 0) {
            pid_t pid = fork();
            if (pid == 0) {
                char *hargv[] = {self, "hog", NULL};
                execv(self, hargv);
                _exit(127);
            }
            ended("exec", wait_for(pid));
            return 0;
        }
        if (strcmp(m, "spawn") == 0) {
            ended("spawn", run("hog", 0));
            return 0;
        }
        if (strcmp(m, "setexec") == 0) return setexec(0);
        if (strcmp(m, "setexec+") == 0) return setexec(LIMIT_MIB);
        if (strcmp(m, "hogwrap") == 0) {
            // Starts a late hog with the limit, tells its pid on descriptor
            // 3, and waits for it.
            pid_t pid = start("hog-late", LIMIT_MIB);
            if (pid < 0) return 1;
            write(3, &pid, sizeof pid);
            close(3);
            wait_for(pid);
            return 0;
        }
        return 2;
    }

    ended("kill", run("hog", LIMIT_MIB));
    run("fork", LIMIT_MIB);
    run("exec", LIMIT_MIB);
    run("spawn", LIMIT_MIB);
    ended("setexec", run("setexec", LIMIT_MIB));
    ended("setexec+", run("setexec+", 0));
    char buf[16];
    printf("control: %s\n", memorystatus_control(CMD_GET_MEMLIMIT_PROPERTIES, getpid(), 0, buf, sizeof buf) == 0 ? "allowed" : strerror(errno));

    // The parent of a process killed at its limit watches it exit.
    pid_t pid = start("hog-late", LIMIT_MIB);
    if (pid > 0) {
        watch("exit-parent", pid);
        wait_for(pid);
    }
    // A process that is not its parent does.
    int fds[2];
    if (pipe(fds) == 0) {
        posix_spawn_file_actions_t fa;
        posix_spawn_file_actions_init(&fa);
        posix_spawn_file_actions_adddup2(&fa, fds[1], 3);
        char *wargv[] = {self, "hogwrap", NULL};
        pid_t wrap, hogpid;
        if (posix_spawn(&wrap, self, &fa, NULL, wargv, environ) == 0) {
            close(fds[1]);
            if (read(fds[0], &hogpid, sizeof hogpid) == sizeof hogpid) watch("exit-other", hogpid);
            wait_for(wrap);
        }
    }
    printf("result: done\n");
    return 0;
}
