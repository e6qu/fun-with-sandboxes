// hardened reports the hostname it sees, or, as "exec <program>", executes
// another program, which a test signs with the hardened runtime.
#include <stdio.h>
#include <string.h>
#include <unistd.h>

int main(int argc, char **argv) {
    if (argc > 2 && strcmp(argv[1], "exec") == 0) {
        execv(argv[2], (char *[]){argv[2], NULL});
        perror("execv");
        return 1;
    }
    char host[256];
    gethostname(host, sizeof host);
    printf("hostname=%s\n", host);
    return 0;
}
