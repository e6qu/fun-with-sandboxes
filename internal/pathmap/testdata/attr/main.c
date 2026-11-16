// attr reports whether getattrlist calls each directory named a mount
// point.
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/attr.h>
#include <unistd.h>

int main(int argc, char **argv) {
    for (int i = 1; i < argc; i++) {
        struct attrlist al;
        memset(&al, 0, sizeof al);
        al.bitmapcount = ATTR_BIT_MAP_COUNT;
        al.dirattr = ATTR_DIR_MOUNTSTATUS;
        struct {
            uint32_t length;
            uint32_t status;
        } __attribute__((packed)) buf;
        if (getattrlist(argv[i], &al, &buf, sizeof buf, 0) != 0) {
            perror(argv[i]);
            return 1;
        }
        printf("%s mountpoint=%d\n", argv[i], (buf.status & DIR_MNTSTATUS_MNTPOINT) != 0);
    }
    return 0;
}
