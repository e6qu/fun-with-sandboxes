// fws-launch starts a container's exec anchor for the spawn helper: it
// forks the anchor, argv[1] run with the main process's pid argv[2], and
// exits at once, so the anchor is no child of the main process the helper
// becomes. A C program starts in about a millisecond, where the Go
// launcher it stands in for took several, and every container start waits
// for it.
#include <unistd.h>

extern char **environ;

int main(int argc, char **argv) {
    if (argc != 3) return 2;
    pid_t pid = fork();
    if (pid < 0) return 1;
    if (pid == 0) {
        char *args[] = {"fws-anchor", argv[2], NULL};
        execve(argv[1], args, environ);
        _exit(127);
    }
    return 0;
}
