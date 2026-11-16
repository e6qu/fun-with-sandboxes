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
