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

struct sandbox_profile {
    uint32_t type;
    const void *bytecode;
    size_t bytecode_length;
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
        fwrite(&p->type, sizeof p->type, 1, f);
        fwrite(p->bytecode, 1, p->bytecode_length, f);
        fclose(f);
        fprintf(stderr, "type %u, %zu bytes; the struct's first 64 bytes:", p->type, p->bytecode_length);
        for (int i = 0; i < 64; i++) fprintf(stderr, "%s%02x", i % 8 ? "" : " ", ((unsigned char *)p)[i]);
        fprintf(stderr, "; bytecode at %p\n", p->bytecode);
        return 0;
    }
    if (strcmp(argv[1], "bytecode") == 0) {
        char *blob = slurp(argv[2], &len);
        if (!blob || len < sizeof(uint32_t)) return 2;
        struct sandbox_profile p;
        memcpy(&p.type, blob, sizeof p.type);
        p.bytecode = blob + sizeof p.type;
        p.bytecode_length = len - sizeof p.type;
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
