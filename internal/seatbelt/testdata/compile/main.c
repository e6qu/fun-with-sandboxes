// compile times libsandbox's compile of the profile in argv[1], argv[2]
// times, and prints the nanoseconds one compile took on average.
#include <stdio.h>
#include <stdlib.h>
#include <time.h>

void *sandbox_create_params(void);
void *sandbox_compile_string(const char *profile, void *params, char **error);
void sandbox_free_profile(void *profile);

int main(int argc, char **argv) {
    if (argc != 3) return 2;
    FILE *f = fopen(argv[1], "r");
    if (!f) return 2;
    static char text[1 << 20];
    size_t n = fread(text, 1, sizeof text - 1, f);
    text[n] = 0;
    fclose(f);
    long times = atol(argv[2]);
    struct timespec a, b;
    clock_gettime(CLOCK_MONOTONIC, &a);
    for (long i = 0; i < times; i++) {
        char *err = NULL;
        void *p = sandbox_compile_string(text, sandbox_create_params(), &err);
        if (!p) {
            fprintf(stderr, "compile: %s\n", err ? err : "unknown error");
            return 1;
        }
        sandbox_free_profile(p);
    }
    clock_gettime(CLOCK_MONOTONIC, &b);
    printf("%ld\n", ((b.tv_sec - a.tv_sec) * 1000000000L + (b.tv_nsec - a.tv_nsec)) / times);
    return 0;
}
