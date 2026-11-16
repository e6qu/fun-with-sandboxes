// spawnactions starts itself again with posix_spawn file actions that name
// container paths: the directory the child starts in and a file its
// standard output opens. The child writes its working directory there.
#include <fcntl.h>
#include <spawn.h>
#include <stdio.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

int main(int argc, char **argv) {
    if (argc > 1) {
        char dir[1024];
        if (!getcwd(dir, sizeof dir)) return 2;
        printf("cwd=%s\n", dir);
        return 0;
    }
    posix_spawn_file_actions_t fa;
    posix_spawn_file_actions_init(&fa);
    // The name programs built before macOS 26 call, which that one calls
    // deprecated.
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
    int err = posix_spawn_file_actions_addchdir_np(&fa, "/made");
#pragma clang diagnostic pop
    if (!err) err = posix_spawn_file_actions_addopen(&fa, 1, "/made/out", O_WRONLY | O_CREAT | O_TRUNC, 0644);
    pid_t pid;
    if (!err) err = posix_spawn(&pid, argv[0], &fa, NULL, (char *[]){argv[0], "child", NULL}, environ);
    if (err) {
        fprintf(stderr, "spawn: %d\n", err);
        return 1;
    }
    int st;
    waitpid(pid, &st, 0);
    return WIFEXITED(st) ? WEXITSTATUS(st) : 3;
}
