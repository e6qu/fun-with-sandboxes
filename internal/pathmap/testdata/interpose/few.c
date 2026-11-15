// few interposes read, write, close and fstat, which nearly every image
// binds: the loader's cost for interposing functions bound everywhere.
#include <sys/stat.h>
#include <unistd.h>

#define INTERPOSE(replacement, replacee)                                                       \
    __attribute__((used)) static struct {                                                     \
        const void *replacement;                                                              \
        const void *replacee;                                                                 \
    } interpose_##replacee __attribute__((section("__DATA,__interpose"))) = {(const void *)(unsigned long)&replacement, \
                                                                             (const void *)(unsigned long)&replacee}

static ssize_t my_read(int fd, void *b, size_t n) { return read(fd, b, n); }
INTERPOSE(my_read, read);
static ssize_t my_write(int fd, const void *b, size_t n) { return write(fd, b, n); }
INTERPOSE(my_write, write);
static int my_close(int fd) { return close(fd); }
INTERPOSE(my_close, close);
static int my_fstat(int fd, struct stat *st) { return fstat(fd, st); }
INTERPOSE(my_fstat, fstat);
