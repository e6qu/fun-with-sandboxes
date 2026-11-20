// nested applies an outer Seatbelt profile, then tries an inner one as
// Chromium's helpers do, and reports what macOS answered and whether the
// inner profile holds: nested OUTER INNER init|apply PROBE-PATH.
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

int sandbox_init(const char *profile, uint64_t flags, char **errorbuf);
void sandbox_free_error(char *errorbuf);
void *sandbox_create_params(void);
void *sandbox_compile_string(const char *data, void *params, char **error);
int sandbox_apply(void *profile);
int sandbox_check(pid_t pid, const char *operation, int type, ...);

static char *slurp(const char *path) {
    FILE *f = fopen(path, "r");
    if (!f) { perror(path); exit(2); }
    static char bufs[2][1 << 20];
    static int which;
    char *b = bufs[which++];
    size_t n = fread(b, 1, (1 << 20) - 1, f);
    b[n] = 0;
    fclose(f);
    return b;
}

static int readable(const char *path) {
    int fd = open(path, O_RDONLY);
    if (fd < 0) return 0;
    close(fd);
    return 1;
}

int main(int argc, char **argv) {
    if (argc != 5) { fprintf(stderr, "usage: nested OUTER INNER init|apply PROBE\n"); return 2; }
    char *outer = slurp(argv[1]), *inner = slurp(argv[2]);
    const char *probe = argv[4];
    char *err = NULL;
    printf("before: sandboxed %d, %s readable %d\n", sandbox_check(getpid(), NULL, 0), probe, readable(probe));
    if (strcmp(argv[1], "/dev/null") != 0) {
        if (sandbox_init(outer, 0, &err) != 0) {
            printf("outer: sandbox_init failed: %s\n", err ? err : "?");
            return 1;
        }
    }
    printf("outer: sandboxed %d, %s readable %d\n", sandbox_check(getpid(), NULL, 0), probe, readable(probe));
    errno = 0;
    if (strcmp(argv[3], "init") == 0) {
        int r = sandbox_init(inner, 0, &err);
        printf("inner sandbox_init: %d errno %d (%s) error %s\n", r, errno, strerror(errno), err ? err : "-");
    } else {
        void *p = sandbox_compile_string(inner, sandbox_create_params(), &err);
        if (!p) {
            printf("inner sandbox_compile_string failed: %s\n", err ? err : "?");
        } else {
            errno = 0;
            int r = sandbox_apply(p);
            printf("inner sandbox_apply: %d errno %d (%s)\n", r, errno, strerror(errno));
        }
    }
    printf("after: %s readable %d\n", probe, readable(probe));
    return 0;
}
