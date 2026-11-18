// hog touches 512 MiB, 16 MiB at a time, and prints "survived" if nothing
// stopped it. With "spawn" it starts itself as a hog through posix_spawn,
// with "forkexec" through fork and execv, and prints how that child ended;
// with "hold" it waits to be killed. Its children name it by argv[0], the
// container's path, which the container library maps.
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

static void ended(int ws) {
    if (WIFSIGNALED(ws)) {
        printf("child: killed by signal %d\n", WTERMSIG(ws));
    } else {
        printf("child: exited %d\n", WEXITSTATUS(ws));
    }
}

int main(int argc, char **argv) {
    setvbuf(stdout, NULL, _IONBF, 0);
    const char *mode = argc > 1 ? argv[1] : "";
    char *child[] = {argv[0], NULL};
    if (strcmp(mode, "hold") == 0) {
        for (;;) pause();
    }
    if (strcmp(mode, "spawn") == 0) {
        pid_t pid;
        int err = posix_spawn(&pid, argv[0], NULL, NULL, child, environ);
        if (err != 0) {
            printf("posix_spawn: %s\n", strerror(err));
            return 1;
        }
        int ws;
        waitpid(pid, &ws, 0);
        ended(ws);
        return 0;
    }
    if (strcmp(mode, "forkexec") == 0) {
        pid_t pid = fork();
        if (pid == 0) {
            execv(argv[0], child);
            _exit(127);
        }
        int ws;
        waitpid(pid, &ws, 0);
        ended(ws);
        return 0;
    }
    int mib = 0;
    for (; mib < 512; mib += 16) {
        char *p = malloc(16 << 20);
        if (!p) return 2;
        memset(p, 1, 16 << 20);
    }
    printf("survived %d\n", mib);
    return 0;
}
