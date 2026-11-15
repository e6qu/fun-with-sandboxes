// icmp pings as an unprivileged ping does, through datagram ICMP sockets:
// loopback, the container's own address, a member, and an outside host,
// on IPv4, and IPv6's loopback. It reports each reply's source, type and
// sequence number, or the error.
#include <arpa/inet.h>
#include <errno.h>
#include <netinet/in.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>

static void ping4(const char *name, const char *to, unsigned short seq) {
    int s = socket(AF_INET, SOCK_DGRAM, IPPROTO_ICMP);
    if (s < 0) {
        printf("%s=socket %s\n", name, strerror(errno));
        return;
    }
    struct timeval tv = {2, 0};
    setsockopt(s, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv);
    int ttl = 32;
    setsockopt(s, IPPROTO_IP, IP_TTL, &ttl, sizeof ttl);
    struct sockaddr_in a;
    memset(&a, 0, sizeof a);
    a.sin_len = sizeof a;
    a.sin_family = AF_INET;
    inet_pton(AF_INET, to, &a.sin_addr);
    unsigned char req[16] = {8, 0, 0, 0, 0x12, 0x34, (unsigned char)(seq >> 8), (unsigned char)seq, 'f', 'w', 's'};
    if (sendto(s, req, sizeof req, 0, (struct sockaddr *)&a, sizeof a) < 0) {
        printf("%s=send %s\n", name, strerror(errno));
        close(s);
        return;
    }
    unsigned char b[256];
    struct sockaddr_in from;
    socklen_t fl = sizeof from;
    ssize_t n = recvfrom(s, b, sizeof b, 0, (struct sockaddr *)&from, &fl);
    close(s);
    if (n < 0) {
        printf("%s=recv %s\n", name, strerror(errno));
        return;
    }
    // IPv4's datagram ICMP socket hands over the IP header too.
    int hl = (b[0] & 0x0f) * 4;
    if (n < hl + 8) {
        printf("%s=short %zd\n", name, n);
        return;
    }
    const unsigned char *ic = b + hl;
    char src[32];
    inet_ntop(AF_INET, &from.sin_addr, src, sizeof src);
    printf("%s=%s type=%d seq=%d data=%.3s\n", name, src, ic[0], ic[6] << 8 | ic[7], (const char *)ic + 8);
}

static void ping6(const char *name, const char *to, unsigned short seq) {
    int s = socket(AF_INET6, SOCK_DGRAM, IPPROTO_ICMPV6);
    if (s < 0) {
        printf("%s=socket %s\n", name, strerror(errno));
        return;
    }
    struct timeval tv = {2, 0};
    setsockopt(s, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv);
    struct sockaddr_in6 a;
    memset(&a, 0, sizeof a);
    a.sin6_len = sizeof a;
    a.sin6_family = AF_INET6;
    inet_pton(AF_INET6, to, &a.sin6_addr);
    if (connect(s, (struct sockaddr *)&a, sizeof a) != 0) {
        printf("%s=connect %s\n", name, strerror(errno));
        close(s);
        return;
    }
    unsigned char req[12] = {128, 0, 0, 0, 0x12, 0x34, (unsigned char)(seq >> 8), (unsigned char)seq, 'f', 'w', 's'};
    if (send(s, req, sizeof req, 0) < 0) {
        printf("%s=send %s\n", name, strerror(errno));
        close(s);
        return;
    }
    unsigned char b[256];
    struct sockaddr_in6 from;
    socklen_t fl = sizeof from;
    ssize_t n = recvfrom(s, b, sizeof b, 0, (struct sockaddr *)&from, &fl);
    close(s);
    if (n < 8) {
        printf("%s=recv %s\n", name, n < 0 ? strerror(errno) : "short");
        return;
    }
    char src[64];
    inet_ntop(AF_INET6, &from.sin6_addr, src, sizeof src);
    printf("%s=%s type=%d seq=%d data=%.3s\n", name, src, b[0], b[6] << 8 | b[7], (const char *)b + 8);
}

int main(int argc, char **argv) {
    ping4("loopback", "127.0.0.1", 1);
    ping4("own", "127.88.0.2", 2);
    ping4("member", "127.88.0.3", 3);
    ping4("outside", "192.0.2.1", 4);
    ping6("loopback6", "::1", 5);
    return 0;
}
