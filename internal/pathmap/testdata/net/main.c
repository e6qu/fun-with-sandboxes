// net listens and connects as a stock server and client do, on container
// ports 8080, 8081 and 8083 (TCP) and 9090 (UDP), with dual-stack IPv6 sockets
// and IPv4 ones, and reaches a stack member by name, in a sandbox that lets
// it bind and reach the host ports behind them alone, as a container's
// does, by an alias a link gives it, and a name its /etc/hosts lists. It
// reports each step.
#include <arpa/inet.h>
#include <errno.h>
#include <ifaddrs.h>
#include <net/if.h>
#include <sys/ioctl.h>
#include <sys/sockio.h>
#include <sys/un.h>
#include <limits.h>
#include <netdb.h>
#include <netinet/in.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
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
    fprintf(stderr, "%s: %s\n", step, strerror(errno));
    printf("%s=error\n", step);
    exit(0);
}

int main(int argc, char **argv) {
    // The container's root holds its Unix sockets, as a container's
    // profile lets it bind and reach them there.
    char root[PATH_MAX] = "/nonexistent";
    const char *rootenv = getenv("SANDBOX_ROOT");
    if (rootenv && !realpath(rootenv, root)) strlcpy(root, "/nonexistent", sizeof root);
    char profile[2048];
    snprintf(profile, sizeof profile,
             "(version 1)(allow default)(deny network-bind)(deny network-outbound)"
             "(allow network-bind (local ip \"localhost:%s\") (local ip \"localhost:%s\") (local ip \"localhost:%s\") (local ip \"localhost:%s\") (local ip \"localhost:%s\") (local unix-socket (subpath \"%s\")))"
             "(allow network-outbound (remote ip \"localhost:%s\") (remote ip \"localhost:%s\") (remote ip \"localhost:%s\") (remote ip \"localhost:%s\") (remote ip \"localhost:%s\") (remote unix-socket (subpath \"%s\")))",
             argv[1], argv[2], argv[3], argv[5], argv[6], root, argv[1], argv[2], argv[3], argv[4], argv[6], root);
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

    // An IPv6 client of ::1 reaches the dual-stack listener, and both its
    // ends read as ::1.
    int c6lo = socket(AF_INET6, SOCK_STREAM, 0);
    struct sockaddr_in6 lo6 = v6("::1", 8080);
    if (connect(c6lo, (void *)&lo6, sizeof lo6)) fail("lo6");
    n = sizeof s;
    getpeername(c6lo, (void *)&s, &n);
    char peer6[80];
    strlcpy(peer6, addr(&s), sizeof peer6);
    n = sizeof s;
    getsockname(c6lo, (void *)&s, &n);
    printf("lo6=%s %s\n", peer6, strncmp(addr(&s), "[::1]:", 6) == 0 ? "own-v6" : addr(&s));

    // An IPv6-only listener on ::1 takes its clients on IPv6.
    int l6 = socket(AF_INET6, SOCK_STREAM, 0), on = 1;
    setsockopt(l6, IPPROTO_IPV6, IPV6_V6ONLY, &on, sizeof on);
    struct sockaddr_in6 lo6only = v6("::1", 8083);
    if (bind(l6, (void *)&lo6only, sizeof lo6only) || listen(l6, 4)) fail("only6");
    int c6only = socket(AF_INET6, SOCK_STREAM, 0);
    if (connect(c6only, (void *)&lo6only, sizeof lo6only)) fail("only6");
    n = sizeof s;
    getpeername(c6only, (void *)&s, &n);
    printf("only6=%s\n", addr(&s));

    // An IPv4 listener on the wildcard.
    int l4 = socket(AF_INET, SOCK_STREAM, 0);
    struct sockaddr_in any4 = v4("0.0.0.0", 8081);
    if (bind(l4, (void *)&any4, sizeof any4) || listen(l4, 4)) fail("listen4");
    n = sizeof s;
    getsockname(l4, (void *)&s, &n);
    printf("listen4=%s\n", addr(&s));

    // A program that binds its host port itself, as the container contract
    // has it, binds it on loopback, though a port directory exists.
    int d = socket(AF_INET, SOCK_STREAM, 0);
    struct sockaddr_in own = v4("127.0.0.1", (unsigned short)atoi(argv[5]));
    if (bind(d, (void *)&own, sizeof own) || listen(d, 4)) fail("direct");
    n = sizeof s;
    getsockname(d, (void *)&s, &n);
    printf("direct=%s\n", addr(&s));

    // A stack member by name: the network file puts db's port 5432 on a
    // host port of its own, where the test's listener stands in for db.
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

    // An alias a link of the container gives db; one another container's
    // link gives it resolves for that container alone.
    if (getaddrinfo("database", "5432", &hints, &res)) fail("alias");
    int ca = socket(res->ai_family, SOCK_STREAM, 0);
    if (connect(ca, res->ai_addr, res->ai_addrlen)) fail("alias");
    freeaddrinfo(res);
    n = sizeof s;
    getpeername(ca, (void *)&s, &n);
    printf("alias=%s\n", addr(&s));
    if (getaddrinfo("theirs", "5432", &hints, &res) == 0) {
        freeaddrinfo(res);
        printf("hidden=resolved\n");
    } else {
        printf("hidden=unknown\n");
    }

    // A name the container's /etc/hosts lists, as --add-host writes it.
    struct addrinfo h4;
    memset(&h4, 0, sizeof h4);
    h4.ai_family = AF_INET;
    h4.ai_socktype = SOCK_STREAM;
    h4.ai_flags = AI_CANONNAME;
    if (getaddrinfo("Extra.Local.", "80", &h4, &res)) fail("hosts");
    memcpy(&s, res->ai_addr, res->ai_addrlen);
    printf("hosts=%s %s\n", addr(&s), res->ai_canonname);
    freeaddrinfo(res);
    struct hostent *he = gethostbyname("extra.local");
    char hb[64];
    if (!he || !inet_ntop(AF_INET, he->h_addr_list[0], hb, sizeof hb)) fail("hostent");
    printf("hostent=%s\n", hb);

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

    // The container's interfaces: loopback, and eth0 at its member address.
    struct ifaddrs *ifs;
    if (getifaddrs(&ifs)) fail("ifaddrs");
    char list[512] = "";
    for (struct ifaddrs *a = ifs; a; a = a->ifa_next) {
        if (!a->ifa_addr || a->ifa_addr->sa_family != AF_INET) continue;
        char b4[INET_ADDRSTRLEN];
        inet_ntop(AF_INET, &((struct sockaddr_in *)(void *)a->ifa_addr)->sin_addr, b4, sizeof b4);
        if (list[0]) strlcat(list, ",", sizeof list);
        strlcat(list, a->ifa_name, sizeof list);
        strlcat(list, "=", sizeof list);
        strlcat(list, b4, sizeof list);
    }
    freeifaddrs(ifs);
    printf("ifaddrs=%s\n", list);
    unsigned idx = if_nametoindex("eth0");
    char back[IF_NAMESIZE] = "";
    printf("ifindex=%s\n", idx && if_indextoname(idx, back) && strcmp(back, "eth0") == 0 ? "eth0" : "none");
    int q = socket(AF_INET, SOCK_DGRAM, 0);
    struct ifreq r;
    memset(&r, 0, sizeof r);
    strlcpy(r.ifr_name, "eth0", sizeof r.ifr_name);
    if (ioctl(q, SIOCGIFADDR, &r)) fail("ifreq");
    char b4[INET_ADDRSTRLEN];
    inet_ntop(AF_INET, &((struct sockaddr_in *)(void *)&r.ifr_addr)->sin_addr, b4, sizeof b4);
    strlcpy(r.ifr_name, "en0", sizeof r.ifr_name);
    int hidden = ioctl(q, SIOCGIFFLAGS, &r) == -1 && errno == ENXIO;
    printf("ifreq=%s %s\n", b4, hidden ? "en0-hidden" : "en0-seen");

    // A Unix socket at a path of the container's.
    int ul = socket(AF_UNIX, SOCK_STREAM, 0);
    struct sockaddr_un un;
    memset(&un, 0, sizeof un);
    un.sun_family = AF_UNIX;
    strlcpy(un.sun_path, "/probe.sock", sizeof un.sun_path);
    un.sun_len = (unsigned char)SUN_LEN(&un);
    if (bind(ul, (void *)&un, un.sun_len) || listen(ul, 1)) fail("unix");
    int uc2 = socket(AF_UNIX, SOCK_STREAM, 0);
    if (connect(uc2, (void *)&un, un.sun_len)) fail("unix");
    int ua = accept(ul, NULL, NULL);
    if (ua < 0) fail("unix");
    struct sockaddr_un named;
    socklen_t nl = sizeof named;
    memset(&named, 0, sizeof named);
    getsockname(ul, (void *)&named, &nl);
    printf("unix=%s\n", named.sun_path);

    // A Unix socket whose path fits the container's address but whose
    // host path, under the root, does not.
    const char *deep = "/a-directory-whose-name-is-long-enough-to-take-the-host-path-past-the-socket-address-cap";
    mkdir(deep, 0755);
    int dl = socket(AF_UNIX, SOCK_STREAM, 0);
    memset(&un, 0, sizeof un);
    un.sun_family = AF_UNIX;
    snprintf(un.sun_path, sizeof un.sun_path, "%s/s.sock", deep);
    un.sun_len = (unsigned char)SUN_LEN(&un);
    if (bind(dl, (void *)&un, un.sun_len) || listen(dl, 1)) fail("unix-deep");
    int dc = socket(AF_UNIX, SOCK_STREAM, 0);
    if (connect(dc, (void *)&un, un.sun_len)) fail("unix-deep");
    int da = accept(dl, NULL, NULL);
    if (da < 0 || write(dc, "x", 1) != 1) fail("unix-deep");
    char got;
    if (read(da, &got, 1) != 1) fail("unix-deep");
    struct sockaddr_un self, peer;
    socklen_t sl = sizeof self, pl = sizeof peer;
    memset(&self, 0, sizeof self);
    memset(&peer, 0, sizeof peer);
    getsockname(dl, (void *)&self, &sl);
    getpeername(dc, (void *)&peer, &pl);
    printf("unix-deep=%s %s\n", self.sun_path, strcmp(self.sun_path, peer.sun_path) == 0 ? "peer-same" : peer.sun_path);

    // Datagrams to a Unix socket at such a path, by sendto and sendmsg.
    int gr = socket(AF_UNIX, SOCK_DGRAM, 0);
    memset(&un, 0, sizeof un);
    un.sun_family = AF_UNIX;
    snprintf(un.sun_path, sizeof un.sun_path, "%s/d.sock", deep);
    un.sun_len = (unsigned char)SUN_LEN(&un);
    if (bind(gr, (void *)&un, un.sun_len)) fail("unix-dgram-deep");
    int gs = socket(AF_UNIX, SOCK_DGRAM, 0);
    if (sendto(gs, "a", 1, 0, (void *)&un, un.sun_len) != 1) fail("unix-dgram-deep");
    struct iovec gv = {"b", 1};
    struct msghdr gm;
    memset(&gm, 0, sizeof gm);
    gm.msg_name = &un;
    gm.msg_namelen = un.sun_len;
    gm.msg_iov = &gv;
    gm.msg_iovlen = 1;
    if (sendmsg(gs, &gm, 0) != 1) fail("unix-dgram-deep");
    char g[2];
    if (recv(gr, &g[0], 1, 0) != 1 || recv(gr, &g[1], 1, 0) != 1) fail("unix-dgram-deep");
    printf("unix-dgram-deep=%.2s\n", g);

    // A datagram from a socket bound at such a path reads as sent from its
    // container path.
    int gb = socket(AF_UNIX, SOCK_DGRAM, 0);
    struct sockaddr_un from;
    memset(&from, 0, sizeof from);
    from.sun_family = AF_UNIX;
    snprintf(from.sun_path, sizeof from.sun_path, "%s/from.sock", deep);
    from.sun_len = (unsigned char)SUN_LEN(&from);
    if (bind(gb, (void *)&from, from.sun_len)) fail("unix-dgram-deep-from");
    if (sendto(gb, "c", 1, 0, (void *)&un, un.sun_len) != 1) fail("unix-dgram-deep-from");
    struct sockaddr_un src;
    socklen_t srcl = sizeof src;
    memset(&src, 0, sizeof src);
    if (recvfrom(gr, &g[0], 1, 0, (void *)&src, &srcl) != 1) fail("unix-dgram-deep-from");
    printf("unix-dgram-deep-from=%s\n", src.sun_path);
    return 0;
}
