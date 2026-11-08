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
    fflush(stdout);
    setenv("DYLD_INSERT_LIBRARIES", "", 1);
    execv(argv[0], (char *[]){argv[0], "again", NULL});
    perror("execv");
    return 1;
}
