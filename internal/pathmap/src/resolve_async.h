// Asynchronous lookups, included at the end of fwsroot.c.
//
// libinfo's getaddrinfo_async_start, which CFNetwork and Foundation use,
// and dns_sd's DNSServiceGetAddrInfo, which mDNSResponder serves, resolve
// through the host's resolver, out of the reach of fws_getaddrinfo. A name
// the container answers itself (own_name) resolves here as fws_getaddrinfo
// resolves it, on a thread of its own, and the result reaches the caller
// as the real calls deliver theirs: getaddrinfo_async_start's through a
// message on the Mach port it hands back, which the caller passes to
// getaddrinfo_async_handle_reply; DNSServiceGetAddrInfo's through
// DNSServiceProcessResult, once the descriptor DNSServiceRefSockFD gives
// reads, or on the queue DNSServiceSetDispatchQueue names. Other names go
// to the real calls.

#include <dispatch/dispatch.h>
#include <dns_sd.h>

typedef void (*fws_gai_async_callback)(int32_t status, struct addrinfo *res, void *context);
extern int32_t getaddrinfo_async_start(mach_port_t *p, const char *nodename, const char *servname, const struct addrinfo *hints,
                                       fws_gai_async_callback callback, void *context);
extern int32_t getaddrinfo_async_handle_reply(void *msg);
extern void getaddrinfo_async_cancel(mach_port_t p);

// own_name reports whether the container answers name itself rather than
// the host's resolver: its hosts file lists it, its stack answers it or a
// name its search domains make of it, or its router resolves its names.
static int own_name(const char *name) {
    if (!name || !*name || numeric_host(name)) return 0;
    char addrs[HOSTS_MAX][INET6_ADDRSTRLEN], canon[256], c[NAME_MAX_LEN + 3];
    if (hosts_lookup(name, addrs, canon, sizeof canon) > 0) return 1;
    if (cluster_name(name, c, sizeof c) != name || stack_answers(name)) return 1;
    return dns_via_router;
}

// A lookup getaddrinfo_async_start runs: its receive right, the message
// that says it is done, and what it found.
struct gai_job {
    struct gai_job *next;
    mach_port_t port;
    char *node, *service;
    struct addrinfo hints;
    int has_hints;
    fws_gai_async_callback callback;
    void *context;
    int32_t status;
    struct addrinfo *res;
    int done, cancelled;
};

#define GAI_DONE_ID 0x66777331

static pthread_mutex_t gai_lock = PTHREAD_MUTEX_INITIALIZER;
static struct gai_job *gai_jobs;

static void gai_free(struct gai_job *j) {
    if (j->res) freeaddrinfo(j->res);
    free(j->node);
    free(j->service);
    free(j);
}

// gai_unlink removes j from the jobs; gai_lock is held.
static void gai_unlink(struct gai_job *j) {
    for (struct gai_job **p = &gai_jobs; *p; p = &(*p)->next) {
        if (*p == j) {
            *p = j->next;
            return;
        }
    }
}

static void gai_drop_port(mach_port_t port) { mach_port_mod_refs(mach_task_self(), port, MACH_PORT_RIGHT_RECEIVE, -1); }

static void *gai_run(void *arg) {
    struct gai_job *j = arg;
    struct addrinfo *res = NULL;
    int32_t status = fws_getaddrinfo(j->node, j->service, j->has_hints ? &j->hints : NULL, &res);
    pthread_mutex_lock(&gai_lock);
    j->status = status;
    j->res = status == 0 ? res : NULL;
    j->done = 1;
    if (j->cancelled) {
        gai_unlink(j);
        pthread_mutex_unlock(&gai_lock);
        gai_drop_port(j->port);
        gai_free(j);
        return NULL;
    }
    mach_port_t port = j->port;
    pthread_mutex_unlock(&gai_lock);
    mach_msg_header_t m;
    memset(&m, 0, sizeof m);
    m.msgh_bits = MACH_MSGH_BITS(MACH_MSG_TYPE_MAKE_SEND, 0);
    m.msgh_size = sizeof m;
    m.msgh_remote_port = port;
    m.msgh_local_port = MACH_PORT_NULL;
    m.msgh_id = GAI_DONE_ID;
    mach_msg(&m, MACH_SEND_MSG | MACH_SEND_TIMEOUT, sizeof m, 0, MACH_PORT_NULL, 0, MACH_PORT_NULL);
    return NULL;
}

static int32_t fws_getaddrinfo_async_start(mach_port_t *p, const char *nodename, const char *servname, const struct addrinfo *hints,
                                           fws_gai_async_callback callback, void *context) {
    if (!p || !callback || !own_name(nodename) || (hints && (hints->ai_flags & AI_NUMERICHOST))) {
        return getaddrinfo_async_start(p, nodename, servname, hints, callback, context);
    }
    struct gai_job *j = calloc(1, sizeof *j);
    if (!j || !(j->node = strdup(nodename)) || (servname && !(j->service = strdup(servname)))) {
        if (j) gai_free(j);
        return EAI_MEMORY;
    }
    if (hints) {
        j->hints = *hints;
        j->hints.ai_canonname = NULL;
        j->hints.ai_addr = NULL;
        j->hints.ai_next = NULL;
        j->has_hints = 1;
    }
    j->callback = callback;
    j->context = context;
    if (mach_port_allocate(mach_task_self(), MACH_PORT_RIGHT_RECEIVE, &j->port) != KERN_SUCCESS) {
        gai_free(j);
        return EAI_SYSTEM;
    }
    pthread_mutex_lock(&gai_lock);
    j->next = gai_jobs;
    gai_jobs = j;
    pthread_mutex_unlock(&gai_lock);
    *p = j->port;
    pthread_t t;
    pthread_attr_t a;
    pthread_attr_init(&a);
    pthread_attr_setdetachstate(&a, PTHREAD_CREATE_DETACHED);
    int err = pthread_create(&t, &a, gai_run, j);
    pthread_attr_destroy(&a);
    if (err != 0) {
        pthread_mutex_lock(&gai_lock);
        gai_unlink(j);
        pthread_mutex_unlock(&gai_lock);
        gai_drop_port(j->port);
        gai_free(j);
        *p = MACH_PORT_NULL;
        return EAI_SYSTEM;
    }
    return 0;
}
INTERPOSE(fws_getaddrinfo_async_start, getaddrinfo_async_start);

// fws_getaddrinfo_async_handle_reply delivers a job's result, on the
// message gai_run sent its port, to its callback, which then owns it, and
// lets the port go, as libinfo does.
static int32_t fws_getaddrinfo_async_handle_reply(void *msg) {
    mach_msg_header_t *m = msg;
    if (!m || m->msgh_id != GAI_DONE_ID) return getaddrinfo_async_handle_reply(msg);
    pthread_mutex_lock(&gai_lock);
    struct gai_job *j = gai_jobs;
    while (j && j->port != m->msgh_local_port) j = j->next;
    if (!j || !j->done) {
        pthread_mutex_unlock(&gai_lock);
        return j ? 0 : getaddrinfo_async_handle_reply(msg);
    }
    gai_unlink(j);
    pthread_mutex_unlock(&gai_lock);
    struct addrinfo *res = j->res;
    j->res = NULL;
    j->callback(j->status, res, j->context);
    gai_drop_port(j->port);
    gai_free(j);
    return 0;
}
INTERPOSE(fws_getaddrinfo_async_handle_reply, getaddrinfo_async_handle_reply);

static void fws_getaddrinfo_async_cancel(mach_port_t port) {
    pthread_mutex_lock(&gai_lock);
    struct gai_job *j = gai_jobs;
    while (j && j->port != port) j = j->next;
    if (!j) {
        pthread_mutex_unlock(&gai_lock);
        getaddrinfo_async_cancel(port);
        return;
    }
    if (!j->done) {
        // The lookup's thread lets it go as it ends.
        j->cancelled = 1;
        pthread_mutex_unlock(&gai_lock);
        return;
    }
    gai_unlink(j);
    pthread_mutex_unlock(&gai_lock);
    gai_drop_port(j->port);
    gai_free(j);
}
INTERPOSE(fws_getaddrinfo_async_cancel, getaddrinfo_async_cancel);

// A lookup DNSServiceGetAddrInfo runs. Its DNSServiceRef is the job
// itself, which the calls below know from the real service's by the list
// of jobs. A pipe reads once the lookup is done. refs counts the caller's
// hold, until DNSServiceRefDeallocate, the thread's, until it is done, and
// a delivery queued on a dispatch queue's.
#define DNS_RESULTS_MAX 32

struct dns_job {
    struct dns_job *next;
    int refs, done, delivered, dead;
    int pipe[2];
    char *name;
    DNSServiceFlags flags;
    uint32_t iface;
    DNSServiceProtocol protocol;
    DNSServiceGetAddrInfoReply callback;
    void *context;
    dispatch_queue_t queue;
    struct sockaddr_storage addrs[DNS_RESULTS_MAX];
    int naddrs;
};

static pthread_mutex_t dns_lock = PTHREAD_MUTEX_INITIALIZER;
static struct dns_job *dns_jobs;

// dns_find is the job whose ref is ref; dns_lock is held.
static struct dns_job *dns_find(DNSServiceRef ref) {
    struct dns_job *j = dns_jobs;
    while (j && (DNSServiceRef)(void *)j != ref) j = j->next;
    return j;
}

// dns_release lets one hold of j go; dns_lock is held, and is let go.
static void dns_release(struct dns_job *j) {
    if (--j->refs > 0) {
        pthread_mutex_unlock(&dns_lock);
        return;
    }
    for (struct dns_job **p = &dns_jobs; *p; p = &(*p)->next) {
        if (*p == j) {
            *p = j->next;
            break;
        }
    }
    pthread_mutex_unlock(&dns_lock);
    close(j->pipe[0]);
    close(j->pipe[1]);
    if (j->queue) dispatch_release(j->queue);
    free(j->name);
    free(j);
}

// dns_deliver calls j's callback with each address found, or with
// kDNSServiceErr_NoSuchRecord where none was, once.
static void dns_deliver(struct dns_job *j) {
    pthread_mutex_lock(&dns_lock);
    if (j->dead || !j->done || j->delivered) {
        pthread_mutex_unlock(&dns_lock);
        return;
    }
    j->delivered = 1;
    pthread_mutex_unlock(&dns_lock);
    char b;
    while (read(j->pipe[0], &b, 1) < 0 && errno == EINTR) {
    }
    DNSServiceRef ref = (DNSServiceRef)(void *)j;
    if (j->naddrs == 0) {
        struct sockaddr_in none;
        memset(&none, 0, sizeof none);
        none.sin_len = sizeof none;
        none.sin_family = AF_INET;
        j->callback(ref, 0, j->iface, kDNSServiceErr_NoSuchRecord, j->name, (const struct sockaddr *)&none, 0, j->context);
        return;
    }
    for (int i = 0; i < j->naddrs; i++) {
        DNSServiceFlags f = kDNSServiceFlagsAdd | (i + 1 < j->naddrs ? kDNSServiceFlagsMoreComing : 0);
        j->callback(ref, f, j->iface, kDNSServiceErr_NoError, j->name, (const struct sockaddr *)&j->addrs[i], 60, j->context);
    }
}

static void dns_deliver_queued(void *arg) {
    struct dns_job *j = arg;
    dns_deliver(j);
    pthread_mutex_lock(&dns_lock);
    dns_release(j);
}

// dns_queue puts j's delivery on its queue, once it has one and is done;
// dns_lock is held.
static void dns_queue(struct dns_job *j) {
    if (!j->queue || !j->done || j->dead) return;
    j->refs++;
    dispatch_async_f(j->queue, j, dns_deliver_queued);
}

static void *dns_run(void *arg) {
    struct dns_job *j = arg;
    struct addrinfo hints, *res = NULL;
    memset(&hints, 0, sizeof hints);
    hints.ai_socktype = SOCK_STREAM;
    int want4 = !j->protocol || (j->protocol & kDNSServiceProtocol_IPv4);
    int want6 = !j->protocol || (j->protocol & kDNSServiceProtocol_IPv6);
    hints.ai_family = want4 && want6 ? AF_UNSPEC : want6 ? AF_INET6 : AF_INET;
    int n = 0;
    if (fws_getaddrinfo(j->name, NULL, &hints, &res) == 0) {
        for (struct addrinfo *a = res; a && n < DNS_RESULTS_MAX; a = a->ai_next) {
            if (a->ai_addrlen > sizeof j->addrs[n]) continue;
            int seen = 0;
            for (int i = 0; i < n && !seen; i++) seen = memcmp(&j->addrs[i], a->ai_addr, a->ai_addrlen) == 0;
            if (!seen) memcpy(&j->addrs[n++], a->ai_addr, a->ai_addrlen);
        }
        freeaddrinfo(res);
    }
    pthread_mutex_lock(&dns_lock);
    j->naddrs = n;
    j->done = 1;
    char b = 0;
    while (write(j->pipe[1], &b, 1) < 0 && errno == EINTR) {
    }
    dns_queue(j);
    dns_release(j);
    return NULL;
}

static DNSServiceErrorType fws_DNSServiceGetAddrInfo(DNSServiceRef *sdRef, DNSServiceFlags flags, uint32_t interfaceIndex,
                                                     DNSServiceProtocol protocol, const char *hostname, DNSServiceGetAddrInfoReply callBack,
                                                     void *context) {
    // A shared connection's ref is the caller's, made by the real service.
    if (!sdRef || !callBack || (flags & kDNSServiceFlagsShareConnection) || !own_name(hostname)) {
        return DNSServiceGetAddrInfo(sdRef, flags, interfaceIndex, protocol, hostname, callBack, context);
    }
    struct dns_job *j = calloc(1, sizeof *j);
    if (!j) return kDNSServiceErr_NoMemory;
    if (!(j->name = strdup(hostname))) {
        free(j);
        return kDNSServiceErr_NoMemory;
    }
    if (pipe(j->pipe) != 0) {
        free(j->name);
        free(j);
        return kDNSServiceErr_NoMemory;
    }
    fcntl(j->pipe[0], F_SETFD, FD_CLOEXEC);
    fcntl(j->pipe[1], F_SETFD, FD_CLOEXEC);
    j->flags = flags;
    j->iface = interfaceIndex;
    j->protocol = protocol;
    j->callback = callBack;
    j->context = context;
    j->refs = 2;
    pthread_mutex_lock(&dns_lock);
    j->next = dns_jobs;
    dns_jobs = j;
    pthread_mutex_unlock(&dns_lock);
    pthread_t t;
    pthread_attr_t a;
    pthread_attr_init(&a);
    pthread_attr_setdetachstate(&a, PTHREAD_CREATE_DETACHED);
    int err = pthread_create(&t, &a, dns_run, j);
    pthread_attr_destroy(&a);
    if (err != 0) {
        pthread_mutex_lock(&dns_lock);
        j->refs = 1;
        dns_release(j);
        return kDNSServiceErr_NoMemory;
    }
    *sdRef = (DNSServiceRef)(void *)j;
    return kDNSServiceErr_NoError;
}
INTERPOSE(fws_DNSServiceGetAddrInfo, DNSServiceGetAddrInfo);

static dnssd_sock_t fws_DNSServiceRefSockFD(DNSServiceRef sdRef) {
    pthread_mutex_lock(&dns_lock);
    struct dns_job *j = dns_find(sdRef);
    int fd = j ? j->pipe[0] : -1;
    pthread_mutex_unlock(&dns_lock);
    return j ? fd : DNSServiceRefSockFD(sdRef);
}
INTERPOSE(fws_DNSServiceRefSockFD, DNSServiceRefSockFD);

// fws_DNSServiceProcessResult waits for the lookup, as the real call waits
// for a reply, and delivers it.
static DNSServiceErrorType fws_DNSServiceProcessResult(DNSServiceRef sdRef) {
    pthread_mutex_lock(&dns_lock);
    struct dns_job *j = dns_find(sdRef);
    if (!j) {
        pthread_mutex_unlock(&dns_lock);
        return DNSServiceProcessResult(sdRef);
    }
    if (j->queue) {
        pthread_mutex_unlock(&dns_lock);
        return kDNSServiceErr_BadParam;
    }
    int fd = j->pipe[0];
    pthread_mutex_unlock(&dns_lock);
    struct pollfd p = {fd, POLLIN, 0};
    while (poll(&p, 1, -1) < 0 && errno == EINTR) {
    }
    dns_deliver(j);
    return kDNSServiceErr_NoError;
}
INTERPOSE(fws_DNSServiceProcessResult, DNSServiceProcessResult);

static DNSServiceErrorType fws_DNSServiceSetDispatchQueue(DNSServiceRef service, dispatch_queue_t queue) {
    pthread_mutex_lock(&dns_lock);
    struct dns_job *j = dns_find(service);
    if (!j) {
        pthread_mutex_unlock(&dns_lock);
        return DNSServiceSetDispatchQueue(service, queue);
    }
    if (!queue || j->queue) {
        pthread_mutex_unlock(&dns_lock);
        return kDNSServiceErr_BadParam;
    }
    dispatch_retain(queue);
    j->queue = queue;
    dns_queue(j);
    pthread_mutex_unlock(&dns_lock);
    return kDNSServiceErr_NoError;
}
INTERPOSE(fws_DNSServiceSetDispatchQueue, DNSServiceSetDispatchQueue);

// fws_DNSServiceRefDeallocate ends the caller's hold: no callback follows,
// though the lookup's thread and a queued delivery may hold the job a
// while longer.
static void fws_DNSServiceRefDeallocate(DNSServiceRef sdRef) {
    pthread_mutex_lock(&dns_lock);
    struct dns_job *j = dns_find(sdRef);
    if (!j || j->dead) {
        pthread_mutex_unlock(&dns_lock);
        if (!j) DNSServiceRefDeallocate(sdRef);
        return;
    }
    j->dead = 1;
    dns_release(j);
}
INTERPOSE(fws_DNSServiceRefDeallocate, DNSServiceRefDeallocate);

// Network.framework resolves through libsystem_dnssd's private client,
// dnssd_getaddrinfo, which asks mDNSResponder and hands each address back
// as a result object Network.framework reads through the client's getters
// (testdata/nwprobe, which TestNetworkFrameworkExperiment traced). A name
// the container answers from its hosts file or its stack is asked of
// mDNSResponder as localhost instead, whose answers come at once, and the
// lookup's results stand for the container's addresses: the getters give
// a result's address as the container's, of the same family, and its
// hostname as the name asked, and a result of a family the container has
// no address in is left out. Names the container's router resolves, for
// a container with DNS settings of its own, go to the host's resolver.
typedef struct fws_dnssd_gai *fws_dnssd_gai_t;
typedef struct fws_dnssd_result *fws_dnssd_result_t;
typedef void (^fws_dnssd_result_handler_t)(fws_dnssd_result_t *results, size_t count);
extern void dnssd_getaddrinfo_set_hostname(fws_dnssd_gai_t gai, const char *hostname) __attribute__((weak_import));
extern void dnssd_getaddrinfo_set_result_handler(fws_dnssd_gai_t gai, fws_dnssd_result_handler_t handler) __attribute__((weak_import));
extern void dnssd_getaddrinfo_invalidate(fws_dnssd_gai_t gai) __attribute__((weak_import));
extern const struct sockaddr *dnssd_getaddrinfo_result_get_address(fws_dnssd_result_t r) __attribute__((weak_import));
extern const char *dnssd_getaddrinfo_result_get_hostname(fws_dnssd_result_t r) __attribute__((weak_import));
extern const char *dnssd_getaddrinfo_result_get_actual_hostname(fws_dnssd_result_t r) __attribute__((weak_import));
extern uint32_t dnssd_getaddrinfo_result_get_interface_index(fws_dnssd_result_t r) __attribute__((weak_import));

// A lookup of a name the container answers, and its addresses.
struct nw_lookup {
    fws_dnssd_gai_t gai;
    char name[256];
    int has4, has6;
    in_addr_t a4;
    struct in6_addr a6;
};

// A result that stands for one of the container's addresses. The slots
// are reused in turn: a result Network.framework still holds once
// NW_RESULTS others came after it reads as mDNSResponder's.
struct nw_result {
    fws_dnssd_result_t r;
    char name[256];
    struct sockaddr_storage sa;
};

#define NW_LOOKUPS 64
#define NW_RESULTS 256
static pthread_mutex_t nw_lock = PTHREAD_MUTEX_INITIALIZER;
static struct nw_lookup nw_lookups[NW_LOOKUPS];
static struct nw_result nw_results[NW_RESULTS];
static unsigned nw_next;

// nw_addresses finds the addresses the container answers name with
// itself, from its hosts file or its stack, as fws_getaddrinfo does, and
// reports whether it has any.
static int nw_addresses(const char *name, struct nw_lookup *l) {
    char n[256], c[NAME_MAX_LEN + 3];
    size_t len = name ? strlen(name) : 0;
    if (len == 0 || len >= sizeof n) return 0;
    memcpy(n, name, len + 1);
    if (n[len - 1] == '.') n[len - 1] = 0;
    if (!*n || numeric_host(n)) return 0;
    for (int pass = 0; pass < 2; pass++) {
        l->has4 = hosts_addr(n, AF_INET, &l->a4);
        l->has6 = hosts_addr(n, AF_INET6, &l->a6);
        if (l->has4 || l->has6) return 1;
        const char *cn = cluster_name(n, c, sizeof c);
        if (cn == n) break;
        strlcpy(n, cn, sizeof n);
    }
    l->has4 = member_address(n, &l->a4) == 1;
    l->has6 = member_address6(n, &l->a6);
    return l->has4 || l->has6;
}

// nw_lookup_of is gai's slot, or with take, a free one; nw_lock is held.
static struct nw_lookup *nw_lookup_of(fws_dnssd_gai_t gai, int take) {
    struct nw_lookup *free_slot = NULL;
    for (int i = 0; i < NW_LOOKUPS; i++) {
        if (nw_lookups[i].gai == gai) return &nw_lookups[i];
        if (!nw_lookups[i].gai && !free_slot) free_slot = &nw_lookups[i];
    }
    return take ? free_slot : NULL;
}

// nw_result_of is the stand-in result r has, or NULL; nw_lock is held.
static struct nw_result *nw_result_of(fws_dnssd_result_t r) {
    for (int i = 0; i < NW_RESULTS; i++) {
        if (nw_results[i].r == r) return &nw_results[i];
    }
    return NULL;
}

static void fws_dnssd_set_hostname(fws_dnssd_gai_t gai, const char *hostname) {
    struct nw_lookup l = {gai};
    int own = nw_addresses(hostname, &l);
    pthread_mutex_lock(&nw_lock);
    struct nw_lookup *slot = nw_lookup_of(gai, own);
    if (own && slot) {
        strlcpy(l.name, hostname, sizeof l.name);
        *slot = l;
    } else {
        // A name the container does not answer, or one more lookup than
        // there are slots for, is the host's.
        if (slot) slot->gai = NULL;
        own = 0;
    }
    pthread_mutex_unlock(&nw_lock);
    dnssd_getaddrinfo_set_hostname(gai, own ? "localhost" : hostname);
}
INTERPOSE(fws_dnssd_set_hostname, dnssd_getaddrinfo_set_hostname);

static void fws_dnssd_invalidate(fws_dnssd_gai_t gai) {
    pthread_mutex_lock(&nw_lock);
    struct nw_lookup *slot = nw_lookup_of(gai, 0);
    if (slot) slot->gai = NULL;
    pthread_mutex_unlock(&nw_lock);
    dnssd_getaddrinfo_invalidate(gai);
}
INTERPOSE(fws_dnssd_invalidate, dnssd_getaddrinfo_invalidate);

// nw_deliver passes a lookup's results to its handler: as they are for a
// name the host answers, each forgetting what it stood for before, as a
// result object freed may come again at the same address; for one the
// container answers, those of the families it has addresses in, which
// then stand for its addresses.
static void nw_deliver(fws_dnssd_gai_t gai, fws_dnssd_result_handler_t handler, fws_dnssd_result_t *results, size_t count) {
    fws_dnssd_result_t *kept = count ? malloc(count * sizeof *kept) : NULL;
    size_t n = 0;
    pthread_mutex_lock(&nw_lock);
    struct nw_lookup *slot = nw_lookup_of(gai, 0), l = {0};
    if (slot) l = *slot;
    for (size_t i = 0; i < count; i++) {
        struct nw_result *e = nw_result_of(results[i]);
        if (e) e->r = NULL;
        if (!slot || !kept) continue;
        const struct sockaddr *sa = dnssd_getaddrinfo_result_get_address(results[i]);
        struct sockaddr_storage ss = {0};
        if (!sa) {
            kept[n++] = results[i];
            continue;
        }
        if (sa->sa_family == AF_INET && l.has4 && sa->sa_len >= sizeof(struct sockaddr_in) && sa->sa_len <= sizeof ss) {
            memcpy(&ss, sa, sa->sa_len);
            ((struct sockaddr_in *)&ss)->sin_addr.s_addr = l.a4;
        } else if (sa->sa_family == AF_INET6 && l.has6 && sa->sa_len >= sizeof(struct sockaddr_in6) && sa->sa_len <= sizeof ss) {
            memcpy(&ss, sa, sa->sa_len);
            ((struct sockaddr_in6 *)&ss)->sin6_addr = l.a6;
            ((struct sockaddr_in6 *)&ss)->sin6_scope_id = 0;
        } else {
            continue;
        }
        e = &nw_results[nw_next++ % NW_RESULTS];
        e->r = results[i];
        strlcpy(e->name, l.name, sizeof e->name);
        e->sa = ss;
        kept[n++] = results[i];
    }
    pthread_mutex_unlock(&nw_lock);
    if (!slot || !kept) {
        handler(results, count);
    } else if (n) {
        handler(kept, n);
    }
    free(kept);
}

static void fws_dnssd_set_result_handler(fws_dnssd_gai_t gai, fws_dnssd_result_handler_t handler) {
    if (!handler) {
        dnssd_getaddrinfo_set_result_handler(gai, handler);
        return;
    }
    // The client copies the handler, and with it the caller's, which this
    // one holds.
    dnssd_getaddrinfo_set_result_handler(gai, ^(fws_dnssd_result_t *results, size_t count) {
        nw_deliver(gai, handler, results, count);
    });
}
INTERPOSE(fws_dnssd_set_result_handler, dnssd_getaddrinfo_set_result_handler);

static const struct sockaddr *fws_dnssd_result_get_address(fws_dnssd_result_t r) {
    pthread_mutex_lock(&nw_lock);
    struct nw_result *e = nw_result_of(r);
    const struct sockaddr *sa = e ? (const struct sockaddr *)&e->sa : NULL;
    pthread_mutex_unlock(&nw_lock);
    return sa ? sa : dnssd_getaddrinfo_result_get_address(r);
}
INTERPOSE(fws_dnssd_result_get_address, dnssd_getaddrinfo_result_get_address);

static const char *fws_dnssd_result_get_hostname(fws_dnssd_result_t r) {
    pthread_mutex_lock(&nw_lock);
    struct nw_result *e = nw_result_of(r);
    const char *name = e ? e->name : NULL;
    pthread_mutex_unlock(&nw_lock);
    return name ? name : dnssd_getaddrinfo_result_get_hostname(r);
}
INTERPOSE(fws_dnssd_result_get_hostname, dnssd_getaddrinfo_result_get_hostname);

static const char *fws_dnssd_result_get_actual_hostname(fws_dnssd_result_t r) {
    pthread_mutex_lock(&nw_lock);
    struct nw_result *e = nw_result_of(r);
    const char *name = e ? e->name : NULL;
    pthread_mutex_unlock(&nw_lock);
    return name ? name : dnssd_getaddrinfo_result_get_actual_hostname(r);
}
INTERPOSE(fws_dnssd_result_get_actual_hostname, dnssd_getaddrinfo_result_get_actual_hostname);

// A stand-in result came on no interface of the host's: localhost's come
// on loopback, which would scope the connection to it.
static uint32_t fws_dnssd_result_get_interface_index(fws_dnssd_result_t r) {
    pthread_mutex_lock(&nw_lock);
    int own = nw_result_of(r) != NULL;
    pthread_mutex_unlock(&nw_lock);
    return own ? 0 : dnssd_getaddrinfo_result_get_interface_index(r);
}
INTERPOSE(fws_dnssd_result_get_interface_index, dnssd_getaddrinfo_result_get_interface_index);
