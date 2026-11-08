// uts reports what a container process sees of its identity and namespace
// beyond its user: its supplementary groups, after initgroups too, its NIS
// domain name, and how much POSIX shared memory it may size.
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <limits.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

static void groups(const char *label) {
    gid_t list[NGROUPS_MAX];
    int n = getgroups(NGROUPS_MAX, list);
    printf("%s=", label);
    for (int i = 0; i < n; i++) printf(i ? ",%u" : "%u", list[i]);
    printf("\n");
}

// sized makes a shared memory object of size bytes and reports whether
// the size was taken, or the error.
static const char *sized(const char *name, off_t size) {
    shm_unlink(name);
    int fd = shm_open(name, O_CREAT | O_EXCL | O_RDWR, 0600);
    if (fd < 0) return strerror(errno);
    int r = ftruncate(fd, size);
    int e = errno;
    close(fd);
    return r == 0 ? "ok" : (e == ENOSPC ? "ENOSPC" : strerror(e));
}

int main(void) {
    groups("groups");
    char d[256] = "unset";
    getdomainname(d, sizeof d);
    printf("domain=%s\n", d);
    printf("shm=%s", sized("/fwsuts.a", 3 << 20));
    printf(" %s", sized("/fwsuts.b", 2 << 20));
    shm_unlink("/fwsuts.a");
    printf(" %s\n", sized("/fwsuts.c", 2 << 20));
    shm_unlink("/fwsuts.b");
    shm_unlink("/fwsuts.c");
    if (initgroups("app", 50) != 0) perror("initgroups");
    groups("initgroups");
    return 0;
}
