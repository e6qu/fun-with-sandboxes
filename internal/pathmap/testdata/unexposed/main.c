// unexposed listens on container port 7000, which has no host port, with a
// dual-stack IPv6 socket, and reaches it on loopback, then reaches port
// 7100 of stack member peer, in a sandbox that lets it bind Unix sockets in
// its port directory and reach its router alone, as a container's does. It
// reports each step.
#include <arpa/inet.h>
#include <netdb.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

int sandbox_init(const char *profile, uint64_t flags, char **err);

static const char *addr(const struct sockaddr_storage *a) {
    static char out[80];
    char b[64];
    if (a->ss_family == AF_INET) {
        const struct sockaddr_in *x = (const void *)a;
        inet_ntop(AF_INET, &x->sin_addr, b, sizeof b);
        snprintf(out, sizeof out, "%s:%u", b, ntohs(x->sin_port));
    } else if (a->ss_family == AF_INET6) {
        const struct sockaddr_in6 *x = (const void *)a;
        inet_ntop(AF_INET6, &x->sin6_addr, b, sizeof b);
        snprintf(out, sizeof out, "[%s]:%u", b, ntohs(x->sin6_port));
    } else {
        snprintf(out, sizeof out, "family %d", a->ss_family);
    }
    return out;
}

static void fail(const char *step) {
    printf("%s=error\n", step);
    exit(0);
}

int main(int argc, char **argv) {
    char profile[1024];
    snprintf(profile, sizeof profile,
             "(version 1)(allow default)(deny network-bind)(deny network-outbound)"
             "(allow network-bind (local unix-socket (subpath \"%s\")))"
             "(allow network-outbound (remote unix-socket (subpath \"%s\")) (remote ip \"localhost:%s\"))",
             argv[1], argv[1], argv[2]);
    char *err = NULL;
    if (sandbox_init(profile, 0, &err) != 0) {
        fprintf(stderr, "sandbox_init: %s\n", err);
        return 1;
    }
    struct sockaddr_storage s;
    socklen_t n;

    int l = socket(AF_INET6, SOCK_STREAM, 0), off = 0, on = 1;
    setsockopt(l, IPPROTO_IPV6, IPV6_V6ONLY, &off, sizeof off);
    setsockopt(l, SOL_SOCKET, SO_REUSEADDR, &on, sizeof on);
    struct sockaddr_in6 any;
    memset(&any, 0, sizeof any);
    any.sin6_len = sizeof any;
    any.sin6_family = AF_INET6;
    any.sin6_port = htons(7000);
    if (bind(l, (void *)&any, sizeof any) || listen(l, 4)) fail("listen");
    n = sizeof s;
    getsockname(l, (void *)&s, &n);
    printf("listen=%s\n", addr(&s));

    int c = socket(AF_INET, SOCK_STREAM, 0);
    struct sockaddr_in lo;
    memset(&lo, 0, sizeof lo);
    lo.sin_len = sizeof lo;
    lo.sin_family = AF_INET;
    lo.sin_port = htons(7000);
    lo.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    if (connect(c, (void *)&lo, sizeof lo)) fail("local");
    setsockopt(c, IPPROTO_TCP, TCP_NODELAY, &on, sizeof on);
    n = sizeof s;
    int k = accept(l, (void *)&s, &n);
    if (k < 0) fail("local");
    char buf[8] = {0};
    if (write(c, "ping", 4) != 4 || read(k, buf, 4) != 4) fail("local");
    n = sizeof s;
    getpeername(c, (void *)&s, &n);
    printf("local=%s %s\n", buf, addr(&s));

    struct addrinfo hints, *res;
    memset(&hints, 0, sizeof hints);
    hints.ai_socktype = SOCK_STREAM;
    if (getaddrinfo("peer", "7100", &hints, &res)) fail("peer");
    int p = socket(res->ai_family, SOCK_STREAM, 0);
    if (connect(p, res->ai_addr, res->ai_addrlen)) fail("peer");
    freeaddrinfo(res);
    memset(buf, 0, sizeof buf);
    if (read(p, buf, 5) != 5) fail("peer");
    n = sizeof s;
    getpeername(p, (void *)&s, &n);
    printf("peer=%s %s\n", buf, addr(&s));

    if (getaddrinfo("peer", "7101", &hints, &res)) fail("refused");
    int q = socket(res->ai_family, SOCK_STREAM, 0);
    printf("refused=%d\n", connect(q, res->ai_addr, res->ai_addrlen));
    freeaddrinfo(res);
    return 0;
}
