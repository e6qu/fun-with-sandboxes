// apply measures applying a precompiled Seatbelt profile against compiling
// it in the process that applies it.
//
//   apply compile <profile> <blob>   compile the profile, write its bytecode
//   apply bytecode <blob> <path>     apply the bytecode, then open path
//   apply text <profile> <path>      compile and apply the text, then open path
//
// The last two print the nanoseconds the sandbox took to apply and exit 0
// when the open was denied, 3 when it was let through.
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

// libsandbox's compiled profile, as far as a dump of one shows it: its
// kind, its bytecode, a word that was zero, and a second, smaller blob.
struct sandbox_profile {
    uint32_t type;
    const void *bytecode;
    size_t bytecode_length;
    uint64_t reserved;
    const void *extra;
    size_t extra_length;
    uint64_t pad[2];
};

void *sandbox_create_params(void);
struct sandbox_profile *sandbox_compile_string(const char *profile, void *params, char **error);
int sandbox_apply(struct sandbox_profile *profile);
void sandbox_free_profile(struct sandbox_profile *profile);

static char *slurp(const char *path, size_t *len) {
    FILE *f = fopen(path, "rb");
    if (!f) return NULL;
    static char buf[1 << 22];
    size_t n = fread(buf, 1, sizeof buf - 1, f);
    fclose(f);
    buf[n] = 0;
    *len = n;
    return buf;
}

static long now(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return t.tv_sec * 1000000000L + t.tv_nsec;
}

static int probe(const char *path) {
    int fd = open(path, O_RDONLY);
    if (fd >= 0) {
        close(fd);
        return 3;
    }
    return errno == EPERM || errno == EACCES ? 0 : 4;
}

int main(int argc, char **argv) {
    if (argc != 4) return 2;
    size_t len;
    if (strcmp(argv[1], "compile") == 0) {
        char *text = slurp(argv[2], &len);
        if (!text) return 2;
        char *err = NULL;
        struct sandbox_profile *p = sandbox_compile_string(text, sandbox_create_params(), &err);
        if (!p) {
            fprintf(stderr, "compile: %s\n", err ? err : "unknown error");
            return 1;
        }
        FILE *f = fopen(argv[3], "wb");
        if (!f) return 2;
        uint64_t n1 = p->bytecode_length, n2 = p->extra ? p->extra_length : 0;
        fwrite(&p->type, sizeof p->type, 1, f);
        fwrite(&n1, sizeof n1, 1, f);
        fwrite(p->bytecode, 1, n1, f);
        fwrite(&n2, sizeof n2, 1, f);
        if (n2) fwrite(p->extra, 1, n2, f);
        fclose(f);
        fprintf(stderr, "reserved %llu; extra: ", (unsigned long long)p->reserved);
        for (size_t i = 0; i < n2; i++) {
            unsigned char c = ((unsigned char *)p->extra)[i];
            fprintf(stderr, c >= 32 && c < 127 ? "%c" : "\\x%02x", c);
        }
        fprintf(stderr, "\n");
        fprintf(stderr, "type %u, %zu bytes; the struct's first 64 bytes:", p->type, p->bytecode_length);
        for (int i = 0; i < 64; i++) fprintf(stderr, "%s%02x", i % 8 ? "" : " ", ((unsigned char *)p)[i]);
        fprintf(stderr, "; bytecode at %p\n", p->bytecode);
        return 0;
    }
    if (strcmp(argv[1], "bytecode") == 0 || strcmp(argv[1], "bytecode-only") == 0) {
        char *blob = slurp(argv[2], &len);
        uint64_t n1, n2;
        struct sandbox_profile p;
        memset(&p, 0, sizeof p);
        if (!blob || len < 4 + 8) return 2;
        memcpy(&p.type, blob, 4);
        memcpy(&n1, blob + 4, 8);
        p.bytecode = blob + 12;
        p.bytecode_length = n1;
        memcpy(&n2, blob + 12 + n1, 8);
        if (n2 && strcmp(argv[1], "bytecode") == 0) {
            p.extra = blob + 20 + n1;
            p.extra_length = n2;
        }
        long a = now();
        int r = sandbox_apply(&p);
        if (r != 0) {
            printf("sandbox_apply returned %d: %s\n", r, strerror(errno));
            return 1;
        }
        printf("%ld\n", now() - a);
        return probe(argv[3]);
    }
    if (strcmp(argv[1], "text") == 0) {
        char *text = slurp(argv[2], &len);
        if (!text) return 2;
        long a = now();
        char *err = NULL;
        struct sandbox_profile *p = sandbox_compile_string(text, sandbox_create_params(), &err);
        if (!p || sandbox_apply(p) != 0) {
            fprintf(stderr, "compile or apply: %s\n", err ? err : strerror(errno));
            return 1;
        }
        printf("%ld\n", now() - a);
        return probe(argv[3]);
    }
    return 2;
}
