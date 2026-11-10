// init runs as a container's main process does, with its own host pid as
// FWS_INIT, and reports how it and a child of its see pid 1: its pid, its
// parent's, the child's parent, a signal to pid 1 from the child, and the
// process table's entry for pid 1, and whether it names its thread.
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/sysctl.h>
#include <sys/wait.h>
#include <unistd.h>

int main(int argc, char **argv) {
    if (!getenv("FWS_INIT")) {
        char pid[16];
        snprintf(pid, sizeof pid, "%d", getpid());
        setenv("FWS_INIT", pid, 1);
        execv(argv[0], argv);
        return 1;
    }
    printf("pid=%d ppid=%d\n", getpid(), getppid());
    fflush(stdout);
    pid_t c = fork();
    if (c == 0) {
        printf("child-ppid=%d kill1=%d\n", getppid(), kill(1, 0));
        fflush(stdout);
        _exit(0);
    }
    waitpid(c, NULL, 0);
    int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_PID, 1};
    struct kinfo_proc kp;
    size_t len = sizeof kp;
    int found = sysctl(mib, 4, &kp, &len, NULL, 0) == 0 && len == sizeof kp;
    printf("table1=%d\n", found ? kp.kp_proc.p_pid : -1);
    int named = pthread_setname_np("fws-init");
    char name[64] = "";
    pthread_getname_np(pthread_self(), name, sizeof name);
    printf("thread-name=%d %s\n", named, name);
    return 0;
}
