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
// to getaddrinfo_async_start; DNSServiceGetAddrInfo, whose service the
// container's profile keeps it from, resolves every name here.

#include <Block.h>
#include <dispatch/dispatch.h>
#include <stdbool.h>
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
                                                     DNSServiceProtocol protocol, const char *host, DNSServiceGetAddrInfoReply callBack,
                                                     void *context) {
    // A shared connection's ref is the caller's, made by the real service.
    // mDNSResponder's socket is out of a container's reach: every name
    // resolves here.
    if (!sdRef || !callBack || (flags & kDNSServiceFlagsShareConnection) || !host || !*host) {
        return DNSServiceGetAddrInfo(sdRef, flags, interfaceIndex, protocol, host, callBack, context);
    }
    struct dns_job *j = calloc(1, sizeof *j);
    if (!j) return kDNSServiceErr_NoMemory;
    if (!(j->name = strdup(host))) {
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
// dnssd_getaddrinfo, which asks mDNSResponder over XPC, and reads each
// address from a result object through the client's getters (testdata/
// nwprobe, which TestNetworkFrameworkExperiment traced). A container's
// profile lets it reach no mDNSResponder, so the library answers in the
// client's place: a lookup it has the name, queue and handler of is never
// activated, and the name resolves as getaddrinfo resolves it in the
// container (fws_getaddrinfo), on a thread of its own, its addresses
// reaching the handler on the lookup's queue as results of the library's
// own, which the getters answer for, laid out as the client lays out its
// own (the enumerations below), so that a name that resolves to nothing
// fails at once.
typedef struct fws_dnssd_gai *fws_dnssd_gai_t;
typedef struct fws_dnssd_result *fws_dnssd_result_t;
typedef void (^fws_dnssd_result_handler_t)(fws_dnssd_result_t *results, size_t count);
typedef void (^fws_dnssd_event_handler_t)(int event, int error);
#define DNSSD_WEAK __attribute__((weak_import))
extern void dnssd_getaddrinfo_set_hostname(fws_dnssd_gai_t gai, const char *name) DNSSD_WEAK;
extern void dnssd_getaddrinfo_set_queue(fws_dnssd_gai_t gai, dispatch_queue_t queue) DNSSD_WEAK;
extern void dnssd_getaddrinfo_set_result_handler(fws_dnssd_gai_t gai, fws_dnssd_result_handler_t handler) DNSSD_WEAK;
extern void dnssd_getaddrinfo_set_event_handler(fws_dnssd_gai_t gai, fws_dnssd_event_handler_t handler) DNSSD_WEAK;
extern void dnssd_getaddrinfo_set_protocols(fws_dnssd_gai_t gai, uint32_t protocols) DNSSD_WEAK;
extern void dnssd_getaddrinfo_activate(fws_dnssd_gai_t gai) DNSSD_WEAK;
extern void dnssd_getaddrinfo_invalidate(fws_dnssd_gai_t gai) DNSSD_WEAK;
extern const struct sockaddr *dnssd_getaddrinfo_result_get_address(fws_dnssd_result_t r) DNSSD_WEAK;
extern const char *dnssd_getaddrinfo_result_get_hostname(fws_dnssd_result_t r) DNSSD_WEAK;
extern const char *dnssd_getaddrinfo_result_get_actual_hostname(fws_dnssd_result_t r) DNSSD_WEAK;
extern int dnssd_getaddrinfo_result_get_type(fws_dnssd_result_t r) DNSSD_WEAK;
extern uint32_t dnssd_getaddrinfo_result_get_interface_index(fws_dnssd_result_t r) DNSSD_WEAK;
extern int dnssd_getaddrinfo_result_get_protocol(fws_dnssd_result_t r) DNSSD_WEAK;
extern const char *dnssd_getaddrinfo_result_get_provider_name(fws_dnssd_result_t r) DNSSD_WEAK;
extern const char *dnssd_getaddrinfo_result_get_tracker_hostname(fws_dnssd_result_t r) DNSSD_WEAK;
extern const char *dnssd_getaddrinfo_result_get_tracker_owner(fws_dnssd_result_t r) DNSSD_WEAK;
extern bool dnssd_getaddrinfo_result_get_tracker_is_approved(fws_dnssd_result_t r) DNSSD_WEAK;
extern const void *dnssd_getaddrinfo_result_get_validation_data(fws_dnssd_result_t r, size_t *len) DNSSD_WEAK;
extern bool dnssd_getaddrinfo_result_is_from_cache(fws_dnssd_result_t r) DNSSD_WEAK;
extern const void *dnssd_getaddrinfo_result_get_ech_config(fws_dnssd_result_t r, size_t *len) DNSSD_WEAK;
extern int dnssd_getaddrinfo_result_get_negative_reason(fws_dnssd_result_t r) DNSSD_WEAK;
extern bool dnssd_getaddrinfo_result_enumerate_alpn_values(fws_dnssd_result_t r, void *block) DNSSD_WEAK;
extern const char *dnssd_getaddrinfo_result_get_doh_uri(fws_dnssd_result_t r) DNSSD_WEAK;

// The values of the client's enumerations the library answers with, as
// TestNetworkFrameworkExperiment printed the client's own: a lookup's
// results are first one of type 5, for the service binding a lookup also
// asks for, with negative reason 3, as for a name without the record;
// then, for each family asked for, its addresses (type 1) or a result of
// type 3 with an address of no more than the family, and the negative
// reason 2, for a name that is not there, or 3; then event 3. A name the
// host answers itself, as localhost, comes with protocol 0. The protocols a
// lookup asks for are DNSServiceProtocol's.
enum {
    NW_RESULT_ADD = 1,
    NW_RESULT_NO_ADDRESS = 3,
    NW_RESULT_SERVICE_BINDING = 5,
    NW_NEGATIVE_NO_NAME = 2,
    NW_NEGATIVE_NO_DATA = 3,
    NW_EVENT_DONE = 3,
    NW_PROTOCOL_V4 = 1,
    NW_PROTOCOL_V6 = 2,
};

// A lookup: what its caller set, until it is invalidated. gen tells a
// lookup from one that took its slot since.
struct nw_lookup {
    fws_dnssd_gai_t gai;
    unsigned gen;
    char name[256];
    dispatch_queue_t queue;
    fws_dnssd_result_handler_t handler;
    fws_dnssd_event_handler_t events;
    uint32_t protocols;
};

// A result of the library's own: an Objective-C object, as the client's
// are, which Network.framework retains and releases as it does those, of
// a class the library makes at run time, whose indexed ivars hold the
// result. The library holds one of each of the last NW_RESULTS it made,
// in turn.
struct nw_result {
    char name[256];
    struct sockaddr_storage sa;
    int type, negative;
};

#define NW_LOOKUPS 64
#define NW_RESULTS 256
#define NW_ADDRS 16
static pthread_mutex_t nw_lock = PTHREAD_MUTEX_INITIALIZER;
static struct nw_lookup nw_lookups[NW_LOOKUPS];
static void *nw_objs[NW_RESULTS];
static unsigned nw_next, nw_gen;

// The Objective-C runtime's functions, from a process that loaded it, as
// one that uses Network.framework has: the library links none.
static struct {
    void *cls;
    void *(*create)(void *cls, size_t extra);
    void *(*ivars)(void *obj);
    void (*release)(void *obj);
} nw_objc;
static pthread_once_t nw_objc_once = PTHREAD_ONCE_INIT;

static void nw_objc_load(void) {
    void *(*get_class)(const char *) = (void *(*)(const char *))dlsym(RTLD_DEFAULT, "objc_getClass");
    void *(*allocate)(void *, const char *, size_t) = (void *(*)(void *, const char *, size_t))dlsym(RTLD_DEFAULT, "objc_allocateClassPair");
    void (*register_class)(void *) = (void (*)(void *))dlsym(RTLD_DEFAULT, "objc_registerClassPair");
    nw_objc.create = (void *(*)(void *, size_t))dlsym(RTLD_DEFAULT, "class_createInstance");
    nw_objc.ivars = (void *(*)(void *))dlsym(RTLD_DEFAULT, "object_getIndexedIvars");
    nw_objc.release = (void (*)(void *))dlsym(RTLD_DEFAULT, "objc_release");
    if (!get_class || !allocate || !register_class || !nw_objc.create || !nw_objc.ivars || !nw_objc.release) return;
    void *nsobject = get_class("NSObject");
    void *cls = nsobject ? allocate(nsobject, "FWSDNSSDResult", 0) : NULL;
    if (cls) {
        register_class(cls);
    } else {
        cls = get_class("FWSDNSSDResult");
    }
    nw_objc.cls = cls;
}

// nw_objc_ready reports whether the library can make results of its own.
static int nw_objc_ready(void) {
    pthread_once(&nw_objc_once, nw_objc_load);
    return nw_objc.cls != NULL;
}

// nw_own is r's contents when r is one of the results the library holds.
static struct nw_result *nw_own(fws_dnssd_result_t r) {
    if (!r || !nw_objc.cls) return NULL;
    struct nw_result *o = NULL;
    pthread_mutex_lock(&nw_lock);
    for (int i = 0; i < NW_RESULTS && !o; i++) {
        if (nw_objs[i] == (void *)r) o = nw_objc.ivars(r);
    }
    pthread_mutex_unlock(&nw_lock);
    return o;
}

// nw_lookup_of is gai's slot, or with take, a fresh one; nw_lock is held.
static struct nw_lookup *nw_lookup_of(fws_dnssd_gai_t gai, int take) {
    struct nw_lookup *free_slot = NULL;
    for (int i = 0; i < NW_LOOKUPS; i++) {
        if (nw_lookups[i].gai == gai) return &nw_lookups[i];
        if (!nw_lookups[i].gai && !free_slot) free_slot = &nw_lookups[i];
    }
    if (take && free_slot) {
        memset(free_slot, 0, sizeof *free_slot);
        free_slot->gai = gai;
        free_slot->gen = ++nw_gen;
    }
    return take ? free_slot : NULL;
}

// nw_drop lets a slot's queue and handler go; nw_lock is held.
static void nw_drop(struct nw_lookup *l) {
    if (l->queue) dispatch_release(l->queue);
    if (l->handler) Block_release(l->handler);
    if (l->events) Block_release(l->events);
    memset(l, 0, sizeof *l);
}

static void fws_dnssd_set_hostname(fws_dnssd_gai_t gai, const char *name) {
    pthread_mutex_lock(&nw_lock);
    struct nw_lookup *l = nw_lookup_of(gai, 1);
    if (l) strlcpy(l->name, name ? name : "", sizeof l->name);
    pthread_mutex_unlock(&nw_lock);
    dnssd_getaddrinfo_set_hostname(gai, name);
}
INTERPOSE(fws_dnssd_set_hostname, dnssd_getaddrinfo_set_hostname);

static void fws_dnssd_set_queue(fws_dnssd_gai_t gai, dispatch_queue_t queue) {
    pthread_mutex_lock(&nw_lock);
    struct nw_lookup *l = nw_lookup_of(gai, 1);
    if (l) {
        if (queue) dispatch_retain(queue);
        if (l->queue) dispatch_release(l->queue);
        l->queue = queue;
    }
    pthread_mutex_unlock(&nw_lock);
    dnssd_getaddrinfo_set_queue(gai, queue);
}
INTERPOSE(fws_dnssd_set_queue, dnssd_getaddrinfo_set_queue);

static void fws_dnssd_set_result_handler(fws_dnssd_gai_t gai, fws_dnssd_result_handler_t handler) {
    pthread_mutex_lock(&nw_lock);
    struct nw_lookup *l = nw_lookup_of(gai, 1);
    if (l) {
        if (l->handler) Block_release(l->handler);
        l->handler = handler ? Block_copy(handler) : NULL;
    }
    pthread_mutex_unlock(&nw_lock);
    dnssd_getaddrinfo_set_result_handler(gai, handler);
}
INTERPOSE(fws_dnssd_set_result_handler, dnssd_getaddrinfo_set_result_handler);

static void fws_dnssd_set_event_handler(fws_dnssd_gai_t gai, fws_dnssd_event_handler_t handler) {
    pthread_mutex_lock(&nw_lock);
    struct nw_lookup *l = nw_lookup_of(gai, 1);
    if (l) {
        if (l->events) Block_release(l->events);
        l->events = handler ? Block_copy(handler) : NULL;
    }
    pthread_mutex_unlock(&nw_lock);
    dnssd_getaddrinfo_set_event_handler(gai, handler);
}
INTERPOSE(fws_dnssd_set_event_handler, dnssd_getaddrinfo_set_event_handler);

static void fws_dnssd_set_protocols(fws_dnssd_gai_t gai, uint32_t protocols) {
    pthread_mutex_lock(&nw_lock);
    struct nw_lookup *l = nw_lookup_of(gai, 1);
    if (l) l->protocols = protocols;
    pthread_mutex_unlock(&nw_lock);
    dnssd_getaddrinfo_set_protocols(gai, protocols);
}
INTERPOSE(fws_dnssd_set_protocols, dnssd_getaddrinfo_set_protocols);

// The threads resolving for the stand-in, whose own lookups, should the
// host's resolver ask the client, are the client's. A table, not a
// thread-local: an inserted library's thread-locals are not set up before
// its first calls.
#define NW_RESOLVERS 64
static _Atomic(pthread_t) nw_resolvers[NW_RESOLVERS];

// nw_resolver_add marks the calling thread as resolving and returns its
// slot, or -1 where every slot is taken.
static int nw_resolver_add(void) {
    pthread_t self = pthread_self();
    for (int i = 0; i < NW_RESOLVERS; i++) {
        pthread_t none = NULL;
        if (atomic_compare_exchange_strong(&nw_resolvers[i], &none, self)) return i;
    }
    return -1;
}

static int nw_resolving(void) {
    pthread_t self = pthread_self();
    for (int i = 0; i < NW_RESOLVERS; i++) {
        if (atomic_load(&nw_resolvers[i]) == self) return 1;
    }
    return 0;
}

// A lookup the library runs: what it asks, and what it found.
struct nw_job {
    fws_dnssd_gai_t gai;
    unsigned gen;
    uint32_t protocols;
    char name[256];
    dispatch_queue_t queue;
    int found, n;
    struct sockaddr_storage addrs[NW_ADDRS];
};

// nw_take makes a result and holds it in the next slot, or reports NULL;
// nw_lock is held.
static fws_dnssd_result_t nw_take(const char *name, int type, int negative, const struct sockaddr_storage *sa) {
    void *obj = nw_objc.create(nw_objc.cls, sizeof(struct nw_result));
    if (!obj) return NULL;
    struct nw_result *r = nw_objc.ivars(obj);
    strlcpy(r->name, name, sizeof r->name);
    r->type = type;
    r->negative = negative;
    r->sa = *sa;
    void **slot = &nw_objs[nw_next++ % NW_RESULTS];
    if (*slot) nw_objc.release(*slot);
    *slot = obj;
    return (fws_dnssd_result_t)obj;
}

// nw_deliver hands a job's results to its lookup's handler, on the
// lookup's queue, unless the lookup ended meanwhile, then says it is done.
static void nw_deliver(void *arg) {
    struct nw_job *j = arg;
    fws_dnssd_result_t results[NW_ADDRS + 3];
    size_t n = 0;
    pthread_mutex_lock(&nw_lock);
    struct nw_lookup *l = nw_lookup_of(j->gai, 0);
    fws_dnssd_result_handler_t handler = l && l->gen == j->gen && l->handler ? Block_copy(l->handler) : NULL;
    fws_dnssd_event_handler_t events = handler && l->events ? Block_copy(l->events) : NULL;
    if (handler) {
        struct sockaddr_storage none = {0};
        if ((results[n] = nw_take(j->name, NW_RESULT_SERVICE_BINDING, j->found ? NW_NEGATIVE_NO_DATA : NW_NEGATIVE_NO_NAME, &none))) n++;
        static const int families[] = {AF_INET6, AF_INET};
        for (int f = 0; f < 2; f++) {
            int fam = families[f];
            if (j->protocols && !(j->protocols & (fam == AF_INET ? NW_PROTOCOL_V4 : NW_PROTOCOL_V6))) continue;
            int any = 0;
            for (int i = 0; i < j->n; i++) {
                if (j->addrs[i].ss_family != fam) continue;
                if ((results[n] = nw_take(j->name, NW_RESULT_ADD, 0, &j->addrs[i]))) n++;
                any = 1;
            }
            if (!any) {
                struct sockaddr_storage empty = {0};
                empty.ss_family = (sa_family_t)fam;
                empty.ss_len = fam == AF_INET ? sizeof(struct sockaddr_in) : sizeof(struct sockaddr_in6);
                if ((results[n] = nw_take(j->name, NW_RESULT_NO_ADDRESS, j->found ? NW_NEGATIVE_NO_DATA : NW_NEGATIVE_NO_NAME, &empty))) n++;
            }
        }
    }
    pthread_mutex_unlock(&nw_lock);
    if (handler) {
        handler(results, n);
        Block_release(handler);
    }
    if (events) {
        events(NW_EVENT_DONE, 0);
        Block_release(events);
    }
    dispatch_release(j->queue);
    free(j);
}

// nw_resolve finds a job's name's addresses as getaddrinfo does in the
// container, up to NW_ADDRS, each once, and hands them on.
static void nw_resolve(void *arg) {
    struct nw_job *j = arg;
    struct addrinfo hints = {0}, *res = NULL;
    hints.ai_family = AF_UNSPEC;
    hints.ai_socktype = SOCK_STREAM;
    // Past NW_RESOLVERS lookups at once, one fails as a resolver that
    // does not answer, rather than ask the client it stands in for.
    int slot = nw_resolver_add();
    int err = slot < 0 ? EAI_AGAIN : fws_getaddrinfo(j->name, NULL, &hints, &res);
    if (slot >= 0) atomic_store(&nw_resolvers[slot], (pthread_t)NULL);
    j->found = err == 0;
    for (struct addrinfo *ai = err == 0 ? res : NULL; ai && j->n < NW_ADDRS; ai = ai->ai_next) {
        if ((ai->ai_family != AF_INET && ai->ai_family != AF_INET6) || ai->ai_addrlen > sizeof j->addrs[0]) continue;
        int seen = 0;
        for (int i = 0; i < j->n && !seen; i++) seen = j->addrs[i].ss_len == ai->ai_addrlen && memcmp(&j->addrs[i], ai->ai_addr, ai->ai_addrlen) == 0;
        if (seen) continue;
        memset(&j->addrs[j->n], 0, sizeof j->addrs[0]);
        memcpy(&j->addrs[j->n], ai->ai_addr, ai->ai_addrlen);
        j->addrs[j->n].ss_len = (uint8_t)ai->ai_addrlen;
        j->n++;
    }
    if (res) freeaddrinfo(res);
    dispatch_async_f(j->queue, j, nw_deliver);
}

// fws_dnssd_activate starts the library's lookup in the client's place,
// for a lookup whose name, queue and handler it has.
static void fws_dnssd_activate(fws_dnssd_gai_t gai) {
    struct nw_job *j = nw_resolving() || !nw_objc_ready() ? NULL : calloc(1, sizeof *j);
    pthread_mutex_lock(&nw_lock);
    struct nw_lookup *l = j ? nw_lookup_of(gai, 0) : NULL;
    if (!l || !l->name[0] || !l->queue || !l->handler) {
        pthread_mutex_unlock(&nw_lock);
        free(j);
        dnssd_getaddrinfo_activate(gai);
        return;
    }
    j->gai = gai;
    j->gen = l->gen;
    j->protocols = l->protocols;
    strlcpy(j->name, l->name, sizeof j->name);
    j->queue = l->queue;
    dispatch_retain(j->queue);
    pthread_mutex_unlock(&nw_lock);
    dispatch_async_f(dispatch_get_global_queue(QOS_CLASS_DEFAULT, 0), j, nw_resolve);
}
INTERPOSE(fws_dnssd_activate, dnssd_getaddrinfo_activate);

static void fws_dnssd_invalidate(fws_dnssd_gai_t gai) {
    pthread_mutex_lock(&nw_lock);
    struct nw_lookup *l = nw_lookup_of(gai, 0);
    if (l) nw_drop(l);
    pthread_mutex_unlock(&nw_lock);
    dnssd_getaddrinfo_invalidate(gai);
}
INTERPOSE(fws_dnssd_invalidate, dnssd_getaddrinfo_invalidate);

// The getters, for the library's results: what it found, as the name
// asked, from no interface in particular and no cache, as the host
// answers a name itself, with nothing a DNS server adds besides.
static const struct sockaddr *fws_dnssd_result_get_address(fws_dnssd_result_t r) {
    struct nw_result *o = nw_own(r);
    return o ? (const struct sockaddr *)&o->sa : dnssd_getaddrinfo_result_get_address(r);
}
INTERPOSE(fws_dnssd_result_get_address, dnssd_getaddrinfo_result_get_address);

static const char *fws_dnssd_result_get_hostname(fws_dnssd_result_t r) {
    struct nw_result *o = nw_own(r);
    return o ? o->name : dnssd_getaddrinfo_result_get_hostname(r);
}
INTERPOSE(fws_dnssd_result_get_hostname, dnssd_getaddrinfo_result_get_hostname);

static const char *fws_dnssd_result_get_actual_hostname(fws_dnssd_result_t r) {
    struct nw_result *o = nw_own(r);
    return o ? o->name : dnssd_getaddrinfo_result_get_actual_hostname(r);
}
INTERPOSE(fws_dnssd_result_get_actual_hostname, dnssd_getaddrinfo_result_get_actual_hostname);

static int fws_dnssd_result_get_type(fws_dnssd_result_t r) {
    struct nw_result *o = nw_own(r);
    return o ? o->type : dnssd_getaddrinfo_result_get_type(r);
}
INTERPOSE(fws_dnssd_result_get_type, dnssd_getaddrinfo_result_get_type);

static uint32_t fws_dnssd_result_get_interface_index(fws_dnssd_result_t r) {
    return nw_own(r) ? 0 : dnssd_getaddrinfo_result_get_interface_index(r);
}
INTERPOSE(fws_dnssd_result_get_interface_index, dnssd_getaddrinfo_result_get_interface_index);

static int fws_dnssd_result_get_protocol(fws_dnssd_result_t r) { return nw_own(r) ? 0 : dnssd_getaddrinfo_result_get_protocol(r); }
INTERPOSE(fws_dnssd_result_get_protocol, dnssd_getaddrinfo_result_get_protocol);

static const char *fws_dnssd_result_get_provider_name(fws_dnssd_result_t r) {
    return nw_own(r) ? NULL : dnssd_getaddrinfo_result_get_provider_name(r);
}
INTERPOSE(fws_dnssd_result_get_provider_name, dnssd_getaddrinfo_result_get_provider_name);

static const char *fws_dnssd_result_get_tracker_hostname(fws_dnssd_result_t r) {
    return nw_own(r) ? NULL : dnssd_getaddrinfo_result_get_tracker_hostname(r);
}
INTERPOSE(fws_dnssd_result_get_tracker_hostname, dnssd_getaddrinfo_result_get_tracker_hostname);

static const char *fws_dnssd_result_get_tracker_owner(fws_dnssd_result_t r) {
    return nw_own(r) ? NULL : dnssd_getaddrinfo_result_get_tracker_owner(r);
}
INTERPOSE(fws_dnssd_result_get_tracker_owner, dnssd_getaddrinfo_result_get_tracker_owner);

static bool fws_dnssd_result_get_tracker_is_approved(fws_dnssd_result_t r) {
    return nw_own(r) ? false : dnssd_getaddrinfo_result_get_tracker_is_approved(r);
}
INTERPOSE(fws_dnssd_result_get_tracker_is_approved, dnssd_getaddrinfo_result_get_tracker_is_approved);

static const void *fws_dnssd_result_get_validation_data(fws_dnssd_result_t r, size_t *len) {
    return nw_own(r) ? NULL : dnssd_getaddrinfo_result_get_validation_data(r, len);
}
INTERPOSE(fws_dnssd_result_get_validation_data, dnssd_getaddrinfo_result_get_validation_data);

static bool fws_dnssd_result_is_from_cache(fws_dnssd_result_t r) { return nw_own(r) ? false : dnssd_getaddrinfo_result_is_from_cache(r); }
INTERPOSE(fws_dnssd_result_is_from_cache, dnssd_getaddrinfo_result_is_from_cache);

static const void *fws_dnssd_result_get_ech_config(fws_dnssd_result_t r, size_t *len) {
    return nw_own(r) ? NULL : dnssd_getaddrinfo_result_get_ech_config(r, len);
}
INTERPOSE(fws_dnssd_result_get_ech_config, dnssd_getaddrinfo_result_get_ech_config);

static int fws_dnssd_result_get_negative_reason(fws_dnssd_result_t r) {
    struct nw_result *o = nw_own(r);
    return o ? o->negative : dnssd_getaddrinfo_result_get_negative_reason(r);
}
INTERPOSE(fws_dnssd_result_get_negative_reason, dnssd_getaddrinfo_result_get_negative_reason);

static bool fws_dnssd_result_enumerate_alpn_values(fws_dnssd_result_t r, void *block) {
    return nw_own(r) ? true : dnssd_getaddrinfo_result_enumerate_alpn_values(r, block);
}
INTERPOSE(fws_dnssd_result_enumerate_alpn_values, dnssd_getaddrinfo_result_enumerate_alpn_values);

static const char *fws_dnssd_result_get_doh_uri(fws_dnssd_result_t r) { return nw_own(r) ? NULL : dnssd_getaddrinfo_result_get_doh_uri(r); }
INTERPOSE(fws_dnssd_result_get_doh_uri, dnssd_getaddrinfo_result_get_doh_uri);
