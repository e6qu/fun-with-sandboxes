// nwprobe connects to HOST port PORT through Network.framework, as an app's
// URLSession does, and prints where the connection went, or how it failed:
// "ready <address>", "failed <error>", or "timeout". With "exists" it
// prints which of the names that follow libSystem exports, by dlsym.
#include <Network/Network.h>
#include <arpa/inet.h>
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

int main(int argc, char **argv) {
    setvbuf(stdout, NULL, _IONBF, 0);
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
