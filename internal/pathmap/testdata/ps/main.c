// ps lists processes as ps and top do, through sysctl and libproc, in a
// sandbox that lets it inspect its own sandbox's processes alone, as a
// container's does, with a child of its own running. It reports whether
// each listing holds itself, its child, and launchd.
#include <libproc.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/sysctl.h>
#include <unistd.h>

int sandbox_init(const char *profile, uint64_t flags, char **err);

static void report(const char *how, pid_t *pids, int n, pid_t child) {
    int self = 0, kid = 0, launchd = 0;
    for (int i = 0; i < n; i++) {
        self |= pids[i] == getpid();
        kid |= pids[i] == child;
        launchd |= pids[i] == 1;
    }
    printf("%s=%d%d%d\n", how, self, kid, launchd);
}

int main(void) {
    char *err = NULL;
    if (sandbox_init("(version 1)(allow default)(deny process-info*)(allow process-info* (target same-sandbox))", 0, &err) != 0) {
        fprintf(stderr, "sandbox_init: %s\n", err);
        return 1;
    }
    pid_t child = fork();
    if (child == 0) {
        pause();
        _exit(0);
    }
    int mib[3] = {CTL_KERN, KERN_PROC, KERN_PROC_ALL};
    size_t len = 0;
    sysctl(mib, 3, NULL, &len, NULL, 0);
    len += 64 * sizeof(struct kinfo_proc);
    struct kinfo_proc *kp = malloc(len);
    pid_t pids[8192];
    int n = 0;
    if (sysctl(mib, 3, kp, &len, NULL, 0) == 0) {
        for (size_t i = 0; i < len / sizeof *kp && n < 8192; i++) pids[n++] = kp[i].kp_proc.p_pid;
    }
    report("sysctl", pids, n, child);
    n = proc_listallpids(pids, sizeof pids);
    report("libproc", pids, n < 0 ? 0 : n, child);
    kill(child, SIGKILL);
    return 0;
}
