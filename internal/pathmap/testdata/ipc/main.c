// ipc creates, reopens and removes POSIX shared memory and a semaphore under
// the names a program on its own host would use, in a sandbox that lets it
// name objects under its IPC prefix alone, as a container's does, and
// reports whether each succeeded.
#include <fcntl.h>
#include <semaphore.h>
#include <stdio.h>
#include <sys/mman.h>
#include <unistd.h>

int sandbox_init(const char *profile, uint64_t flags, char **err);

static int shm(const char *name) {
    int fd = shm_open(name, O_CREAT | O_EXCL | O_RDWR, 0600);
    if (fd < 0) return 0;
    close(fd);
    fd = shm_open(name, O_RDWR, 0);
    if (fd < 0) return 0;
    close(fd);
    return shm_unlink(name) == 0 && shm_open(name, O_RDWR, 0) < 0;
}

int main(void) {
    char *err = NULL;
    if (sandbox_init("(version 1)(allow default)(deny ipc-posix-shm* ipc-posix-sem*)"
                     "(allow ipc-posix-shm* ipc-posix-sem* (ipc-posix-name-prefix \"fwstest.\") (ipc-posix-name-prefix \"/fwstest.\"))",
                     0, &err) != 0) {
        fprintf(stderr, "sandbox_init: %s\n", err);
        return 1;
    }
    char name[32], longname[32];
    snprintf(name, sizeof name, "/app.%d", getpid());
    // 31 bytes, the most macOS takes: too long to carry the prefix.
    snprintf(longname, sizeof longname, "/a.long.shared.memory.%09d", getpid());
    sem_t *s = sem_open(name, O_CREAT | O_EXCL, 0600, 1);
    int sem = s != SEM_FAILED && sem_wait(s) == 0 && sem_post(s) == 0 && sem_close(s) == 0 &&
              (s = sem_open(name, 0)) != SEM_FAILED && sem_close(s) == 0 && sem_unlink(name) == 0;
    printf("%d %d %d\n", shm(name), sem, shm(longname));
    return 0;
}
