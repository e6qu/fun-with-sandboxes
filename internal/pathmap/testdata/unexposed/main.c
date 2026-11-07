// unexposed listens on container port 7000, which has no host port, with a
// dual-stack IPv6 socket, TCP and UDP, and reaches it on loopback, then
// reaches port 7100 of stack member peer, TCP and UDP, in a sandbox that
// lets it bind Unix sockets in its port directory and reach its router and
// the two flow ports it gives the UDP ports alone, as a container's does,
// and listens on published port 7200, whose host port the sandbox lets it
// bind and reach.
// It asks UDP sockets for their datagrams' destinations, as IPv6 and IPv4
// control messages. It reports each step.
#define __APPLE_USE_RFC_3542
#include <arpa/inet.h>
#include <errno.h>
#include <net/if.h>
#include <netdb.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/uio.h>
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
             "(allow network-bind (local unix-socket (subpath \"%s\")) (local ip \"localhost:%s\"))"
             "(allow network-outbound (remote unix-socket (subpath \"%s\")) (remote ip \"localhost:%s\") (remote ip \"localhost:%s\") (remote ip \"localhost:%s\") (remote ip \"localhost:%s\") (remote ip \"localhost:%s\"))",
             argv[1], argv[6], argv[1], argv[2], argv[3], argv[4], argv[5], argv[6]);
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
    char client[80], seen[80];
    strlcpy(client, addr(&s), sizeof client);
    n = sizeof s;
    getpeername(k, (void *)&s, &n);
    strlcpy(seen, addr(&s), sizeof seen);
    n = sizeof s;
    getsockname(c, (void *)&s, &n);
    char buf[8] = {0};
    if (write(c, "ping", 4) != 4 || read(k, buf, 4) != 4) fail("local");
    // The server sees its client at the port the client has, on the
    // IPv4-mapped loopback address a dual-stack listener reports.
    const char *own = strrchr(addr(&s), ':');
    printf("client=%s\n", strcmp(seen, client) == 0 && strncmp(client, "[::ffff:127.0.0.1]:", 19) == 0 &&
                               strcmp(strrchr(client, ':'), own) == 0 && strcmp(own, ":0") != 0
                           ? "its-own"
                           : client);
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

    // A bind to port 0, which the sandbox would refuse the kernel, takes a
    // port of the ephemeral range, which the container reaches.
    int e = socket(AF_INET, SOCK_STREAM, 0);
    struct sockaddr_in zero = lo;
    zero.sin_port = 0;
    if (bind(e, (void *)&zero, sizeof zero) || listen(e, 4)) fail("ephemeral");
    struct sockaddr_in ebound;
    n = sizeof ebound;
    getsockname(e, (void *)&ebound, &n);
    unsigned short eport = ntohs(ebound.sin_port);
    struct sockaddr_in to = lo;
    to.sin_port = ebound.sin_port;
    int ec = socket(AF_INET, SOCK_STREAM, 0);
    if (connect(ec, (void *)&to, sizeof to)) fail("ephemeral");
    int ek = accept(e, NULL, NULL);
    memset(buf, 0, sizeof buf);
    if (ek < 0 || write(ec, "ping", 4) != 4 || read(ek, buf, 4) != 4) fail("ephemeral");
    printf("ephemeral=%s %s\n", buf, eport >= 49152 ? "in-range" : "out-of-range");
    int ue = socket(AF_INET6, SOCK_DGRAM, 0);
    struct sockaddr_in6 uzero = any;
    uzero.sin6_port = 0;
    if (bind(ue, (void *)&uzero, sizeof uzero)) fail("udp-ephemeral");
    struct sockaddr_in6 ugot;
    n = sizeof ugot;
    getsockname(ue, (void *)&ugot, &n);
    printf("udp-ephemeral=%s\n", ntohs(ugot.sin6_port) >= 49152 ? "in-range" : "out-of-range");

    // UDP on port 7000, which has no host port either.
    int u = socket(AF_INET6, SOCK_DGRAM, 0);
    setsockopt(u, IPPROTO_IPV6, IPV6_V6ONLY, &off, sizeof off);
    if (bind(u, (void *)&any, sizeof any)) fail("udp-listen");
    n = sizeof s;
    getsockname(u, (void *)&s, &n);
    printf("udp-listen=%s\n", addr(&s));
    int uc = socket(AF_INET, SOCK_DGRAM, 0);
    if (sendto(uc, "ping", 4, 0, (void *)&lo, sizeof lo) != 4) fail("udp-local");
    memset(buf, 0, sizeof buf);
    n = sizeof s;
    if (recvfrom(u, buf, sizeof buf, 0, (void *)&s, &n) != 4) fail("udp-local");
    printf("udp-local=%s %s\n", buf, strncmp(addr(&s), "[::ffff:127.0.0.1]:", 19) == 0 ? "from-loopback" : addr(&s));
    struct sockaddr_storage from = s;
    socklen_t fromn = n;
    if (sendto(u, "pong", 4, 0, (void *)&s, n) != 4) fail("udp-reply");
    memset(buf, 0, sizeof buf);
    n = sizeof s;
    if (recvfrom(uc, buf, sizeof buf, 0, (void *)&s, &n) != 4) fail("udp-reply");
    printf("udp-reply=%s %s\n", buf, addr(&s));

    // Connected to the client, the socket sends without a destination and
    // takes the client's datagrams alone.
    char want[80];
    snprintf(want, sizeof want, "%s", addr(&from));
    int cr = connect(u, (void *)&from, fromn);
    n = sizeof s;
    int pr = getpeername(u, (void *)&s, &n);
    printf("udp-connect=%d %s\n", cr, pr == 0 && strcmp(addr(&s), want) == 0 ? "peer-is-client" : addr(&s));
    struct iovec halves[2] = {{"wr", 2}, {"iv", 2}};
    if (send(u, "send", 4, 0) != 4 || write(u, "writ", 4) != 4 || writev(u, halves, 2) != 4) fail("udp-connected-send");
    char b1[8] = {0}, b2[8] = {0}, b3[8] = {0};
    if (recv(uc, b1, 4, 0) != 4 || recv(uc, b2, 4, 0) != 4 || recv(uc, b3, 4, 0) != 4) fail("udp-connected-send");
    printf("udp-connected-send=%s %s %s\n", b1, b2, b3);
    errno = 0;
    int ir = (int)sendto(u, "x", 1, 0, (void *)&from, fromn);
    printf("udp-isconn=%d %s\n", ir, errno == EISCONN ? "EISCONN" : strerror(errno));
    int other = socket(AF_INET, SOCK_DGRAM, 0);
    if (sendto(other, "othr", 4, 0, (void *)&lo, sizeof lo) != 4 || sendto(uc, "mine", 4, 0, (void *)&lo, sizeof lo) != 4) fail("udp-filter");
    memset(buf, 0, sizeof buf);
    if (read(u, buf, sizeof buf) != 4) fail("udp-filter");
    printf("udp-filter=%s\n", buf);
    struct sockaddr unspec;
    memset(&unspec, 0, sizeof unspec);
    unspec.sa_len = sizeof unspec;
    unspec.sa_family = AF_UNSPEC;
    cr = connect(u, &unspec, sizeof unspec);
    n = sizeof s;
    errno = 0;
    pr = getpeername(u, (void *)&s, &n);
    printf("udp-disconnect=%d %s\n", cr, pr == -1 && errno == ENOTCONN ? "ENOTCONN" : "connected");
    if (sendto(other, "othr", 4, 0, (void *)&lo, sizeof lo) != 4) fail("udp-disconnected");
    char h1[4] = {0}, h2[4] = {0};
    struct iovec parts[2] = {{h1, 2}, {h2, 2}};
    if (readv(u, parts, 2) != 4) fail("udp-disconnected");
    printf("udp-disconnected=%s %s\n", h1, h2);

    // The dual-stack socket's datagrams carry their destination, as
    // IPV6_PKTINFO on loopback's interface.
    unsigned int lo0 = if_nametoindex("lo0");
    char cbuf[256];
    struct iovec iv = {buf, sizeof buf};
    struct msghdr mh;
    int got = 0;
    socklen_t gl = sizeof got;
    if (setsockopt(u, IPPROTO_IPV6, IPV6_RECVPKTINFO, &on, sizeof on) || getsockopt(u, IPPROTO_IPV6, IPV6_RECVPKTINFO, &got, &gl)) fail("udp-pktinfo6");
    if (sendto(uc, "pk6", 3, 0, (void *)&lo, sizeof lo) != 3) fail("udp-pktinfo6");
    memset(buf, 0, sizeof buf);
    memset(&mh, 0, sizeof mh);
    mh.msg_iov = &iv;
    mh.msg_iovlen = 1;
    mh.msg_control = cbuf;
    mh.msg_controllen = sizeof cbuf;
    if (recvmsg(u, &mh, 0) != 3) fail("udp-pktinfo6");
    char dst[64] = "none";
    for (struct cmsghdr *cm = CMSG_FIRSTHDR(&mh); cm; cm = CMSG_NXTHDR(&mh, cm)) {
        if (cm->cmsg_level == IPPROTO_IPV6 && cm->cmsg_type == IPV6_PKTINFO) {
            struct in6_pktinfo pi;
            memcpy(&pi, CMSG_DATA(cm), sizeof pi);
            inet_ntop(AF_INET6, &pi.ipi6_addr, dst, sizeof dst);
            if (pi.ipi6_ifindex != lo0) strcpy(dst, "other-interface");
        }
    }
    printf("udp-pktinfo6=%d %s %s\n", got, buf, dst);

    // An IPv4 socket on port 7001, which has no host port, takes
    // IP_RECVDSTADDR and IP_PKTINFO, and a short buffer truncates them.
    int u4 = socket(AF_INET, SOCK_DGRAM, 0);
    struct sockaddr_in any4 = lo;
    any4.sin_port = htons(7001);
    any4.sin_addr.s_addr = htonl(INADDR_ANY);
    if (bind(u4, (void *)&any4, sizeof any4)) fail("udp-dstaddr");
    if (setsockopt(u4, IPPROTO_IP, IP_RECVDSTADDR, &on, sizeof on) || setsockopt(u4, IPPROTO_IP, IP_PKTINFO, &on, sizeof on)) fail("udp-dstaddr");
    struct sockaddr_in lo4 = lo;
    lo4.sin_port = htons(7001);
    if (sendto(uc, "pk4", 3, 0, (void *)&lo4, sizeof lo4) != 3) fail("udp-dstaddr");
    memset(buf, 0, sizeof buf);
    mh.msg_controllen = sizeof cbuf;
    if (recvmsg(u4, &mh, 0) != 3) fail("udp-dstaddr");
    char da[32] = "none", pa[32] = "none";
    for (struct cmsghdr *cm = CMSG_FIRSTHDR(&mh); cm; cm = CMSG_NXTHDR(&mh, cm)) {
        if (cm->cmsg_level == IPPROTO_IP && cm->cmsg_type == IP_RECVDSTADDR) {
            inet_ntop(AF_INET, CMSG_DATA(cm), da, sizeof da);
        } else if (cm->cmsg_level == IPPROTO_IP && cm->cmsg_type == IP_PKTINFO) {
            struct in_pktinfo pi;
            memcpy(&pi, CMSG_DATA(cm), sizeof pi);
            inet_ntop(AF_INET, &pi.ipi_addr, pa, sizeof pa);
            if (pi.ipi_ifindex != lo0) strcpy(pa, "other-interface");
        }
    }
    if (sendto(uc, "cut", 3, 0, (void *)&lo4, sizeof lo4) != 3) fail("udp-dstaddr");
    mh.msg_controllen = CMSG_SPACE(sizeof(struct in_addr));
    if (recvmsg(u4, &mh, 0) != 3) fail("udp-dstaddr");
    printf("udp-dstaddr=%s pktinfo=%s truncated=%s\n", da, pa, (mh.msg_flags & MSG_CTRUNC) ? "yes" : "no");

    hints.ai_socktype = SOCK_DGRAM;
    if (getaddrinfo("peer", "7100", &hints, &res)) fail("udp-peer");
    int up = socket(res->ai_family, SOCK_DGRAM, 0);
    if (sendto(up, "hi", 2, 0, res->ai_addr, res->ai_addrlen) != 2) fail("udp-peer");
    freeaddrinfo(res);
    memset(buf, 0, sizeof buf);
    n = sizeof s;
    if (recvfrom(up, buf, sizeof buf, 0, (void *)&s, &n) != 5) fail("udp-peer");
    printf("udp-peer=%s %s\n", buf, addr(&s));

    // A listener on loopback alone on published port 7200 stays the
    // container's: loopback reaches it, the container's own address and
    // so the host port do not, and the wildcard binds beside it only once
    // it has closed.
    int pl = socket(AF_INET, SOCK_STREAM, 0);
    struct sockaddr_in pub = lo;
    pub.sin_port = htons(7200);
    if (bind(pl, (void *)&pub, sizeof pub) || listen(pl, 4)) fail("loopback-only");
    n = sizeof s;
    getsockname(pl, (void *)&s, &n);
    printf("loopback-only=%s", addr(&s));
    int pc = socket(AF_INET, SOCK_STREAM, 0);
    if (connect(pc, (void *)&pub, sizeof pub)) fail("loopback-only");
    int pk = accept(pl, NULL, NULL);
    memset(buf, 0, sizeof buf);
    if (pk < 0 || write(pc, "ping", 4) != 4 || read(pk, buf, 4) != 4) fail("loopback-only");
    printf(" %s", buf);
    struct sockaddr_in mine = pub;
    inet_pton(AF_INET, "127.88.0.2", &mine.sin_addr);
    int po = socket(AF_INET, SOCK_STREAM, 0);
    int orr = connect(po, (void *)&mine, sizeof mine);
    printf(" own=%s", orr == -1 && errno == ECONNREFUSED ? "refused" : "reached");
    struct sockaddr_in wild = pub;
    wild.sin_addr.s_addr = htonl(INADDR_ANY);
    int pw = socket(AF_INET, SOCK_STREAM, 0);
    int wr = bind(pw, (void *)&wild, sizeof wild);
    printf(" wildcard=%s", wr == -1 && errno == EADDRINUSE ? "EADDRINUSE" : "bound");
    close(pk);
    close(pc);
    close(pl);
    close(pw);
    pw = socket(AF_INET, SOCK_STREAM, 0);
    if (bind(pw, (void *)&wild, sizeof wild) || listen(pw, 4)) fail("loopback-only");
    n = sizeof s;
    getsockname(pw, (void *)&s, &n);
    printf(" then=%s\n", addr(&s));
    return 0;
}
