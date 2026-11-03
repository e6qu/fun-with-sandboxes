// net listens and connects as a stock server and client do, on container
// ports 8080 and 8081 (TCP) and 9090 (UDP), with dual-stack IPv6 sockets
// and IPv4 ones, and reaches a stack member by name, in a sandbox that lets
// it bind and reach the host ports behind them alone, as a container's
// does. It reports each step.
#include <arpa/inet.h>
#include <netdb.h>
#include <netinet/in.h>
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
    } else {
        const struct sockaddr_in6 *x = (const void *)a;
        inet_ntop(AF_INET6, &x->sin6_addr, b, sizeof b);
        snprintf(out, sizeof out, "[%s]:%u", b, ntohs(x->sin6_port));
    }
    return out;
}

static struct sockaddr_in6 v6(const char *a, unsigned short port) {
    struct sockaddr_in6 s;
    memset(&s, 0, sizeof s);
    s.sin6_len = sizeof s;
    s.sin6_family = AF_INET6;
    s.sin6_port = htons(port);
    inet_pton(AF_INET6, a, &s.sin6_addr);
    return s;
}

static struct sockaddr_in v4(const char *a, unsigned short port) {
    struct sockaddr_in s;
    memset(&s, 0, sizeof s);
    s.sin_len = sizeof s;
    s.sin_family = AF_INET;
    s.sin_port = htons(port);
    inet_pton(AF_INET, a, &s.sin_addr);
    return s;
}

static void fail(const char *step) {
    printf("%s=error\n", step);
    exit(0);
}

int main(int argc, char **argv) {
    char profile[1024];
    snprintf(profile, sizeof profile,
             "(version 1)(allow default)(deny network-bind)(deny network-outbound)"
             "(allow network-bind (local ip \"localhost:%s\") (local ip \"localhost:%s\") (local ip \"localhost:%s\") (local ip \"localhost:%s\"))"
             "(allow network-outbound (remote ip \"localhost:%s\") (remote ip \"localhost:%s\") (remote ip \"localhost:%s\") (remote ip \"localhost:%s\"))",
             argv[1], argv[2], argv[3], argv[4], argv[1], argv[2], argv[3], argv[4]);
    char *err = NULL;
    if (sandbox_init(profile, 0, &err) != 0) {
        fprintf(stderr, "sandbox_init: %s\n", err);
        return 1;
    }
    struct sockaddr_storage s;
    socklen_t n;

    // A dual-stack listener on the IPv6 wildcard, as Go, Node and Python
    // listen by default.
    int l = socket(AF_INET6, SOCK_STREAM, 0), off = 0;
    setsockopt(l, IPPROTO_IPV6, IPV6_V6ONLY, &off, sizeof off);
    struct sockaddr_in6 any = v6("::", 8080);
    if (bind(l, (void *)&any, sizeof any) || listen(l, 4)) fail("listen6");
    n = sizeof s;
    getsockname(l, (void *)&s, &n);
    printf("listen6=%s\n", addr(&s));

    int c = socket(AF_INET, SOCK_STREAM, 0);
    struct sockaddr_in lo = v4("127.0.0.1", 8080);
    if (connect(c, (void *)&lo, sizeof lo)) fail("client4");
    n = sizeof s;
    getpeername(c, (void *)&s, &n);
    printf("client4=%s\n", addr(&s));
    n = sizeof s;
    int k = accept(l, (void *)&s, &n);
    if (k < 0) fail("accept");
    printf("accepted=%s\n", s.ss_family == AF_INET6 ? "v6" : "v4");

    // A client through an IPv6 socket to an IPv4 address, as Java connects.
    int c6 = socket(AF_INET6, SOCK_STREAM, 0);
    struct sockaddr_in6 mapped = v6("::ffff:127.0.0.1", 8080);
    if (connect(c6, (void *)&mapped, sizeof mapped)) fail("client6");
    n = sizeof s;
    getpeername(c6, (void *)&s, &n);
    printf("client6=%s\n", addr(&s));

    // An IPv4 listener on the wildcard.
    int l4 = socket(AF_INET, SOCK_STREAM, 0);
    struct sockaddr_in any4 = v4("0.0.0.0", 8081);
    if (bind(l4, (void *)&any4, sizeof any4) || listen(l4, 4)) fail("listen4");
    n = sizeof s;
    getsockname(l4, (void *)&s, &n);
    printf("listen4=%s\n", addr(&s));

    // A stack member by name: the network file puts db's port 5432 on a
    // host port of its own, where a listener stands in for db.
    int peer = socket(AF_INET, SOCK_STREAM, 0);
    struct sockaddr_in peeraddr = v4("127.0.0.1", (unsigned short)atoi(argv[4]));
    if (bind(peer, (void *)&peeraddr, sizeof peeraddr) || listen(peer, 4)) fail("member");
    struct addrinfo hints, *res;
    memset(&hints, 0, sizeof hints);
    hints.ai_socktype = SOCK_STREAM;
    if (getaddrinfo("db", "5432", &hints, &res)) fail("member");
    int cm = socket(res->ai_family, SOCK_STREAM, 0);
    if (connect(cm, res->ai_addr, res->ai_addrlen)) fail("member");
    freeaddrinfo(res);
    n = sizeof s;
    getpeername(cm, (void *)&s, &n);
    printf("member=%s\n", addr(&s));

    // UDP: an IPv4 server, an IPv6 client to its IPv4 address.
    int u = socket(AF_INET, SOCK_DGRAM, 0);
    struct sockaddr_in uany = v4("0.0.0.0", 9090);
    if (bind(u, (void *)&uany, sizeof uany)) fail("udp");
    int u6 = socket(AF_INET6, SOCK_DGRAM, 0);
    struct sockaddr_in6 udst = v6("::ffff:127.0.0.1", 9090);
    if (sendto(u6, "ping", 4, 0, (void *)&udst, sizeof udst) != 4) fail("udp");
    char buf[8];
    n = sizeof s;
    if (recvfrom(u, buf, sizeof buf, 0, (void *)&s, &n) != 4) fail("udp");
    // The client's socket still reads as IPv6: an unconnected one's own
    // address is the wildcard.
    n = sizeof s;
    getsockname(u6, (void *)&s, &n);
    char host[80];
    strlcpy(host, addr(&s), sizeof host);
    char *port = strrchr(host, ':');
    if (port) *port = 0;
    printf("udp=%s\n", host);
    return 0;
}
