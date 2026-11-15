// one interposes getpid alone, which few images call: the loader's cost
// for interposing one rarely bound function.
#include <unistd.h>

#define INTERPOSE(replacement, replacee)                                                       \
    __attribute__((used)) static struct {                                                     \
        const void *replacement;                                                              \
        const void *replacee;                                                                 \
    } interpose_##replacee __attribute__((section("__DATA,__interpose"))) = {(const void *)(unsigned long)&replacement, \
                                                                             (const void *)(unsigned long)&replacee}

static pid_t my_getpid(void) { return getpid(); }
INTERPOSE(my_getpid, getpid);
