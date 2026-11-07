// Command tempdir prints the per-user directories confstr names, and makes
// a file in the temporary one, as Rust's std::env::temp_dir has a program do.
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

int main(void) {
    char dir[1024], cache[1024], user[1024];
    confstr(_CS_DARWIN_USER_TEMP_DIR, dir, sizeof dir);
    confstr(_CS_DARWIN_USER_CACHE_DIR, cache, sizeof cache);
    confstr(_CS_DARWIN_USER_DIR, user, sizeof user);
    char tmpl[1100];
    snprintf(tmpl, sizeof tmpl, "%sprobe.XXXXXX", dir);
    int fd = mkstemp(tmpl);
    printf("temp=%s cache=%s user=%s made=%s\n", dir, cache, user, fd < 0 ? "no" : "yes");
    return 0;
}
