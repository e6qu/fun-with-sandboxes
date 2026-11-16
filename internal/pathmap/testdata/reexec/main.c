// reexec does what ClickHouse does with the variable that loads the
// container library: it looks for it, empties it and executes itself
// again, which then reports the hostname the library presents.
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

int main(int argc, char **argv) {
    char host[256];
    if (argc > 1) {
        gethostname(host, sizeof host);
        printf("hostname=%s\n", host);
        return 0;
    }
    printf("%s\n", getenv("DYLD_INSERT_LIBRARIES") ? "visible" : "hidden");
    // What a program lists of its environment it finds with getenv: the
    // variable is not listed, and every one listed is found.
    extern char **environ;
    int listed = 0;
    for (char **e = environ; *e; e++) {
        if (strncmp(*e, "DYLD_INSERT_LIBRARIES=", 22) == 0) listed = 1;
        char name[256];
        size_t n = strcspn(*e, "=");
        if (n < sizeof name) {
            memcpy(name, *e, n);
            name[n] = 0;
            if (!getenv(name)) listed = 1;
        }
    }
    printf("%s\n", listed ? "listed" : "unlisted");
    fflush(stdout);
    setenv("DYLD_INSERT_LIBRARIES", "", 1);
    execv(argv[0], (char *[]){argv[0], "again", NULL});
    perror("execv");
    return 1;
}
