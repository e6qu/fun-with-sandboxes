// jetsam asks whether macOS holds an unprivileged process to a memory limit
// its parent sets through posix_spawn's jetsam attributes: it starts itself
// as a child with a fatal 64 MiB limit, the child touches up to 512 MiB in
// 16 MiB steps, printing each, and the parent reports how the child ended.
#include <signal.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

// spawn_private.h and spawn_internal.h, which the SDK does not ship.
int posix_spawnattr_setjetsam_ext(posix_spawnattr_t *attr, short flags, int priority, int memlimit_active, int memlimit_inactive);
enum {
    JETSAM_SET = 0x8000,
    MEMLIMIT_ACTIVE_FATAL = 0x04,
    MEMLIMIT_INACTIVE_FATAL = 0x08,
    PRIORITY_DEFAULT = 180,
    LIMIT_MIB = 64,
    STEP_MIB = 16,
    MAX_MIB = 512,
};

static int hog(void) {
    setvbuf(stdout, NULL, _IONBF, 0);
    for (int mib = STEP_MIB; mib <= MAX_MIB; mib += STEP_MIB) {
        char *p = malloc((size_t)STEP_MIB << 20);
        if (!p) {
            printf("malloc failed at %d MiB\n", mib);
            return 2;
        }
        memset(p, 1, (size_t)STEP_MIB << 20);
        printf("touched %d MiB\n", mib);
    }
    return 0;
}

int main(int argc, char **argv) {
    if (argc > 1 && strcmp(argv[1], "hog") == 0) return hog();
    posix_spawnattr_t attr;
    posix_spawnattr_init(&attr);
    // The flags are a short whose top bit is JETSAM_SET.
    int err = posix_spawnattr_setjetsam_ext(&attr, (short)(JETSAM_SET | MEMLIMIT_ACTIVE_FATAL | MEMLIMIT_INACTIVE_FATAL),
                                            PRIORITY_DEFAULT, LIMIT_MIB, LIMIT_MIB);
    printf("setjetsam_ext: %d\n", err);
    char *child[] = {argv[0], "hog", NULL};
    pid_t pid;
    err = posix_spawn(&pid, argv[0], NULL, &attr, child, environ);
    printf("posix_spawn: %d %s\n", err, err ? strerror(err) : "");
    if (err) {
        printf("result: macOS refused the spawn\n");
        return 0;
    }
    fflush(stdout);
    int ws;
    if (waitpid(pid, &ws, 0) < 0) return 1;
    if (WIFSIGNALED(ws)) {
        printf("result: killed by signal %d\n", WTERMSIG(ws));
    } else {
        printf("result: exited %d\n", WEXITSTATUS(ws));
    }
    return 0;
}
