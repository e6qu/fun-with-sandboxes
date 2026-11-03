// sysv creates a System V shared memory segment, semaphore set and message
// queue under one fixed key each, exclusively, and reports whether each
// creation succeeded; with "remove" it removes them.
#include <stdio.h>
#include <string.h>
#include <sys/ipc.h>
#include <sys/msg.h>
#include <sys/sem.h>
#include <sys/shm.h>

#define KEY 0x46575301

int main(int argc, char **argv) {
    int rm = argc > 1 && strcmp(argv[1], "remove") == 0;
    int shm = shmget(KEY, 4096, rm ? 0 : IPC_CREAT | IPC_EXCL | 0600);
    int sem = semget(KEY, 1, rm ? 0 : IPC_CREAT | IPC_EXCL | 0600);
    int msg = msgget(KEY, rm ? 0 : IPC_CREAT | IPC_EXCL | 0600);
    if (rm) {
        if (shm >= 0) shmctl(shm, IPC_RMID, NULL);
        if (sem >= 0) semctl(sem, 0, IPC_RMID);
        if (msg >= 0) msgctl(msg, IPC_RMID, NULL);
    }
    printf("%d%d%d\n", shm >= 0, sem >= 0, msg >= 0);
    return 0;
}
