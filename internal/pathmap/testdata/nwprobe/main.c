// nwprobe connects to HOST port PORT through Network.framework, as an app's
// URLSession does, and prints where the connection went, or how it failed:
// "ready <address>", "failed <error>", or "timeout". With "exists" it
// prints which of the names that follow libSystem exports, by dlsym; with
// "exports NAME" every symbol the loaded library whose path holds NAME
// exports, read from its export trie.
#include <Network/Network.h>
#include <arpa/inet.h>
#include <dlfcn.h>
#include <mach-o/dyld.h>
#include <mach-o/loader.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static uint64_t uleb(const uint8_t **p) {
    uint64_t v = 0;
    int shift = 0;
    while (**p & 0x80) {
        v |= (uint64_t)(**p & 0x7f) << shift;
        shift += 7;
        (*p)++;
    }
    v |= (uint64_t)**p << shift;
    (*p)++;
    return v;
}

// walk prints the names of the export trie's node at off, under prefix.
static void walk(const uint8_t *trie, uint64_t size, uint64_t off, char *prefix, size_t plen, int depth) {
    if (off >= size || depth > 128) return;
    const uint8_t *p = trie + off;
    uint64_t term = uleb(&p);
    if (term) printf("%.*s\n", (int)plen, prefix);
    p += term;
    uint8_t children = *p++;
    for (uint8_t i = 0; i < children; i++) {
        size_t n = strlen((const char *)p);
        if (plen + n < 1024) memcpy(prefix + plen, p, n);
        p += n + 1;
        uint64_t child = uleb(&p);
        if (plen + n < 1024) walk(trie, size, child, prefix, plen + n, depth + 1);
    }
}

static int exports(const char *name) {
    for (uint32_t i = 0; i < _dyld_image_count(); i++) {
        const char *path = _dyld_get_image_name(i);
        if (!path || !strstr(path, name)) continue;
        const struct mach_header_64 *h = (const struct mach_header_64 *)_dyld_get_image_header(i);
        intptr_t slide = _dyld_get_image_vmaddr_slide(i);
        const struct load_command *lc = (const struct load_command *)(h + 1);
        uint64_t le_vm = 0, le_off = 0;
        uint32_t trie_off = 0, trie_size = 0;
        for (uint32_t j = 0; j < h->ncmds; j++) {
            if (lc->cmd == LC_SEGMENT_64 && strcmp(((const struct segment_command_64 *)lc)->segname, "__LINKEDIT") == 0) {
                le_vm = ((const struct segment_command_64 *)lc)->vmaddr;
                le_off = ((const struct segment_command_64 *)lc)->fileoff;
            } else if (lc->cmd == LC_DYLD_EXPORTS_TRIE) {
                trie_off = ((const struct linkedit_data_command *)lc)->dataoff;
                trie_size = ((const struct linkedit_data_command *)lc)->datasize;
            } else if ((lc->cmd == LC_DYLD_INFO || lc->cmd == LC_DYLD_INFO_ONLY) && !trie_off) {
                trie_off = ((const struct dyld_info_command *)lc)->export_off;
                trie_size = ((const struct dyld_info_command *)lc)->export_size;
            }
            lc = (const struct load_command *)((const char *)lc + lc->cmdsize);
        }
        if (!le_vm || !trie_off) {
            printf("%s: no export trie\n", path);
            return 1;
        }
        const uint8_t *trie = (const uint8_t *)(le_vm + slide + trie_off - le_off);
        char prefix[1024];
        walk(trie, trie_size, 0, prefix, 0, 0);
        return 0;
    }
    printf("no image %s\n", name);
    return 1;
}

int main(int argc, char **argv) {
    setvbuf(stdout, NULL, _IONBF, 0);
    if (argc == 3 && strcmp(argv[1], "exports") == 0) return exports(argv[2]);
    if (argc > 1 && strcmp(argv[1], "exists") == 0) {
        for (int i = 2; i < argc; i++) {
            printf("%s %s\n", argv[i], dlsym(RTLD_DEFAULT, argv[i]) ? "exported" : "missing");
        }
        return 0;
    }
    if (argc != 3) {
        fprintf(stderr, "usage: nwprobe HOST PORT | nwprobe exists NAME...\n");
        return 2;
    }
    nw_endpoint_t ep = nw_endpoint_create_host(argv[1], argv[2]);
    nw_parameters_t p = nw_parameters_create_secure_tcp(NW_PARAMETERS_DISABLE_PROTOCOL, NW_PARAMETERS_DEFAULT_CONFIGURATION);
    nw_connection_t c = nw_connection_create(ep, p);
    dispatch_queue_t q = dispatch_queue_create("nwprobe", NULL);
    nw_connection_set_queue(c, q);
    nw_connection_set_state_changed_handler(c, ^(nw_connection_state_t state, nw_error_t err) {
        if (state == nw_connection_state_ready) {
            nw_path_t path = nw_connection_copy_current_path(c);
            nw_endpoint_t remote = path ? nw_path_copy_effective_remote_endpoint(path) : NULL;
            const struct sockaddr *sa = remote ? nw_endpoint_get_address(remote) : NULL;
            char buf[INET6_ADDRSTRLEN] = "?";
            if (sa && sa->sa_family == AF_INET) inet_ntop(AF_INET, &((const struct sockaddr_in *)sa)->sin_addr, buf, sizeof buf);
            if (sa && sa->sa_family == AF_INET6) inet_ntop(AF_INET6, &((const struct sockaddr_in6 *)sa)->sin6_addr, buf, sizeof buf);
            printf("ready %s\n", buf);
            exit(0);
        }
        if (state == nw_connection_state_failed || state == nw_connection_state_waiting) {
            printf("%s %d\n", state == nw_connection_state_failed ? "failed" : "waiting", err ? nw_error_get_error_code(err) : 0);
            exit(0);
        }
    });
    nw_connection_start(c);
    dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 20 * NSEC_PER_SEC), dispatch_get_main_queue(), ^{
        printf("timeout\n");
        exit(0);
    });
    dispatch_main();
}
