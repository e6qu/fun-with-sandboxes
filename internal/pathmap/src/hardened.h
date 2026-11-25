// A program signed with the hardened runtime, with library validation, or
// as restricted starts without the libraries DYLD_INSERT_LIBRARIES names:
// dyld drops the variable for it, so the program would run outside the
// container's view. fws_unharden makes such a program one that loads the
// container library, as its container could with codesign: it turns the
// program's signature into an ad hoc one without those flags. The code
// directory's page hashes cover the file up to the signature, which stays
// where and as large as it was, so they hold; only the directory's flags
// and team change, and the Developer ID signature over the directory,
// which no longer matches it, leaves the signature's index.
//
// The file is replaced, not written: the kernel keeps the signature it
// read with the file's vnode, and a process running the old file keeps it.
// A file the container cannot write, on a read-only mount, stays as it
// is, and runs as it did.
//
// Included by the container library and by the spawn helper; every
// function is static.
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>
#ifdef __APPLE__
#include <sys/clonefile.h>
#endif

#define FWS_MH_MAGIC_64 0xfeedfacfu
#define FWS_FAT_MAGIC 0xcafebabeu
#define FWS_FAT_MAGIC_64 0xcafebabfu
#define FWS_LC_CODE_SIGNATURE 0x1du
#define FWS_CSMAGIC_EMBEDDED_SIGNATURE 0xfade0cc0u
#define FWS_CSMAGIC_CODEDIRECTORY 0xfade0c02u
#define FWS_CSSLOT_CODEDIRECTORY 0u
#define FWS_CSSLOT_ALTERNATE_CODEDIRECTORIES 0x1000u
#define FWS_CSSLOT_ALTERNATE_CODEDIRECTORY_LIMIT 0x1005u
#define FWS_CSSLOT_SIGNATURESLOT 0x10000u
#define FWS_CS_ADHOC 0x2u
// CS_RESTRICT, CS_REQUIRE_LV and CS_RUNTIME.
#define FWS_CS_HARDENED (0x800u | 0x2000u | 0x10000u)
// The most blobs a signature's index holds here, and the most slices of a
// universal file.
#define FWS_CS_MAX_BLOBS 64
#define FWS_FAT_MAX_SLICES 16

static uint32_t fws_be32(const unsigned char *p) {
    return (uint32_t)p[0] << 24 | (uint32_t)p[1] << 16 | (uint32_t)p[2] << 8 | (uint32_t)p[3];
}
static uint64_t fws_be64(const unsigned char *p) { return (uint64_t)fws_be32(p) << 32 | fws_be32(p + 4); }
static uint32_t fws_le32(const unsigned char *p) {
    return (uint32_t)p[3] << 24 | (uint32_t)p[2] << 16 | (uint32_t)p[1] << 8 | (uint32_t)p[0];
}
static void fws_put_be32(unsigned char *p, uint32_t v) {
    p[0] = (unsigned char)(v >> 24);
    p[1] = (unsigned char)(v >> 16);
    p[2] = (unsigned char)(v >> 8);
    p[3] = (unsigned char)v;
}

// fws_signature finds the signature of the 64-bit Mach-O that starts at
// base in fd: its offset in the file and its size. It returns 0 where
// there is none.
static int fws_signature(int fd, off_t base, off_t *sig, uint32_t *size) {
    unsigned char h[32];
    if (pread(fd, h, sizeof h, base) != (ssize_t)sizeof h || fws_le32(h) != FWS_MH_MAGIC_64) return 0;
    uint32_t ncmds = fws_le32(h + 16), cmdsize = fws_le32(h + 20);
    if (cmdsize < 8 || cmdsize > (1u << 20)) return 0;
    unsigned char *cmds = malloc(cmdsize);
    if (!cmds) return 0;
    int found = 0;
    if (pread(fd, cmds, cmdsize, base + 32) == (ssize_t)cmdsize) {
        uint32_t off = 0;
        for (uint32_t i = 0; i < ncmds && off + 8 <= cmdsize; i++) {
            uint32_t cmd = fws_le32(cmds + off), len = fws_le32(cmds + off + 4);
            if (len < 8 || len > cmdsize - off) break;
            if (cmd == FWS_LC_CODE_SIGNATURE && len >= 16) {
                *sig = base + fws_le32(cmds + off + 8);
                *size = fws_le32(cmds + off + 12);
                found = 1;
                break;
            }
            off += len;
        }
    }
    free(cmds);
    return found;
}

// fws_unharden_slice rewrites, when out is a file, the signature of the
// Mach-O at base, read from fd, and returns whether the signature is one
// to rewrite: one whose code directories carry a hardening flag.
static int fws_unharden_slice(int fd, off_t base, int out) {
    off_t sig;
    uint32_t size;
    if (!fws_signature(fd, base, &sig, &size) || size < 12) return 0;
    unsigned char head[12 + 8 * FWS_CS_MAX_BLOBS];
    if (pread(fd, head, 12, sig) != 12 || fws_be32(head) != FWS_CSMAGIC_EMBEDDED_SIGNATURE) return 0;
    uint32_t count = fws_be32(head + 8);
    if (count == 0 || count > FWS_CS_MAX_BLOBS || 12 + 8 * (uint64_t)count > size) return 0;
    if (pread(fd, head + 12, 8 * count, sig + 12) != (ssize_t)(8 * count)) return 0;

    int hardened = 0;
    uint32_t kept = 0;
    for (uint32_t i = 0; i < count; i++) {
        unsigned char *e = head + 12 + 8 * i;
        uint32_t type = fws_be32(e), off = fws_be32(e + 4);
        if (type == FWS_CSSLOT_SIGNATURESLOT) {
            // The signature over the code directory goes: what is left is
            // an ad hoc signature.
            continue;
        }
        if (type == FWS_CSSLOT_CODEDIRECTORY ||
            (type >= FWS_CSSLOT_ALTERNATE_CODEDIRECTORIES && type < FWS_CSSLOT_ALTERNATE_CODEDIRECTORY_LIMIT)) {
            // A code directory: magic, length, version, flags, and, from
            // version 0x20200, the offset of its team identifier at 48.
            unsigned char cd[52];
            if ((uint64_t)off + sizeof cd > size || pread(fd, cd, sizeof cd, sig + off) != (ssize_t)sizeof cd ||
                fws_be32(cd) != FWS_CSMAGIC_CODEDIRECTORY) {
                return 0;
            }
            uint32_t flags = fws_be32(cd + 12);
            if (flags & FWS_CS_HARDENED) hardened = 1;
            if (out >= 0) {
                fws_put_be32(cd + 12, (flags & ~FWS_CS_HARDENED) | FWS_CS_ADHOC);
                if (fws_be32(cd + 8) >= 0x20200) fws_put_be32(cd + 48, 0);
                if (pwrite(out, cd, sizeof cd, sig + off) != (ssize_t)sizeof cd) return -1;
            }
        }
        memmove(head + 12 + 8 * kept, e, 8);
        kept++;
    }
    if (out >= 0 && hardened) {
        fws_put_be32(head + 8, kept);
        memset(head + 12 + 8 * kept, 0, 8 * (count - kept));
        if (pwrite(out, head, 12 + 8 * count, sig) != (ssize_t)(12 + 8 * count)) return -1;
    }
    return hardened;
}

// fws_unharden_file visits the Mach-O in fd, or each slice of a universal
// file, as fws_unharden_slice does.
static int fws_unharden_file(int fd, int out) {
    unsigned char h[8];
    if (pread(fd, h, sizeof h, 0) != (ssize_t)sizeof h) return 0;
    uint32_t magic = fws_be32(h);
    if (magic != FWS_FAT_MAGIC && magic != FWS_FAT_MAGIC_64) return fws_unharden_slice(fd, 0, out);
    uint32_t n = fws_be32(h + 4);
    // A Java class file starts as a universal file does, with a count no
    // universal file holds.
    if (n == 0 || n > FWS_FAT_MAX_SLICES) return 0;
    size_t entry = magic == FWS_FAT_MAGIC ? 20 : 32;
    int hardened = 0;
    for (uint32_t i = 0; i < n; i++) {
        unsigned char a[32];
        if (pread(fd, a, entry, (off_t)(8 + entry * i)) != (ssize_t)entry) return hardened;
        off_t base = magic == FWS_FAT_MAGIC ? (off_t)fws_be32(a + 8) : (off_t)fws_be64(a + 8);
        int r = fws_unharden_slice(fd, base, out);
        if (r < 0) return r;
        hardened |= r;
    }
    return hardened;
}

// fws_unharden makes the program at path, a host path, load the container
// library where its signature would keep it from doing so. It returns 1
// when it replaced the file, 0 when the file needs nothing or cannot be
// replaced.
static int fws_unharden(const char *path) {
    int saved = errno;
    int done = 0;
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) {
        errno = saved;
        return 0;
    }
    int hardened = fws_unharden_file(fd, -1);
    close(fd);
    if (hardened <= 0) {
        errno = saved;
        return 0;
    }
    // The file itself, not a link that names it.
    char real[PATH_MAX], tmp[PATH_MAX];
    struct stat st;
    if (!realpath(path, real) || stat(real, &st) != 0 || !S_ISREG(st.st_mode) ||
        snprintf(tmp, sizeof tmp, "%s.fws-%d", real, (int)getpid()) >= (int)sizeof tmp) {
        errno = saved;
        return 0;
    }
    unlink(tmp);
#ifdef __APPLE__
    // A clone carries the file's mode, flags and extended attributes, and
    // shares its blocks.
    if (clonefile(real, tmp, 0) != 0) {
        errno = saved;
        return 0;
    }
#else
    {
        char cmd[2 * PATH_MAX + 16];
        snprintf(cmd, sizeof cmd, "cp -p '%s' '%s'", real, tmp);
        if (system(cmd) != 0) {
            errno = saved;
            return 0;
        }
    }
#endif
    if (chmod(tmp, (st.st_mode & 07777) | S_IWUSR) == 0) {
        int out = open(tmp, O_RDWR | O_CLOEXEC);
        if (out >= 0) {
            int ok = fws_unharden_file(out, out) > 0;
            close(out);
            if (ok && chmod(tmp, st.st_mode & 07777) == 0 && rename(tmp, real) == 0) done = 1;
        }
    }
    if (!done) unlink(tmp);
    errno = saved;
    return done;
}
