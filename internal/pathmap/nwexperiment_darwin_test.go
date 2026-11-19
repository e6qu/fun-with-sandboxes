package pathmap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// nwCandidates are the functions through which Network.framework might
// resolve a name in-process, where the container library could answer it:
// libinfo's, dns_sd's and the private dnssd client's, and the calls that
// reach mDNSResponder or a server.
var nwCandidates = []string{
	"getaddrinfo", "getaddrinfo_async_start", "getaddrinfo_async_handle_reply", "gethostbyname",
	"DNSServiceGetAddrInfo", "DNSServiceQueryRecord", "DNSServiceCreateConnection", "DNSServiceProcessResult",
	"DNSServiceSetDispatchQueue", "DNSServiceRefSockFD",
	"dnssd_getaddrinfo_create", "dnssd_getaddrinfo_new", "dnssd_getaddrinfo_set_hostname", "dnssd_getaddrinfo_set_queue",
	"dnssd_getaddrinfo_set_result_handler", "dnssd_getaddrinfo_set_event_handler", "dnssd_getaddrinfo_activate",
	"dnssd_getaddrinfo_invalidate", "dnssd_getaddrinfo_result_get_ip_address", "dnssd_getaddrinfo_result_get_hostname",
	"dnssd_getaddrinfo_result_get_actual_hostname", "dnssd_getaddrinfo_result_get_type", "dnssd_getaddrinfo_result_get_interface_index",
	"xpc_connection_create_mach_service", "bootstrap_look_up", "connect", "connectx",
	// The rest of the dnssd client mDNSResponder's sources declare
	// (dnssd_private.h), which the SDK's stub leaves out.
	"dnssd_retain", "dnssd_release", "dnssd_getaddrinfo_set_flags", "dnssd_getaddrinfo_set_interface_index",
	"dnssd_getaddrinfo_set_protocols", "dnssd_getaddrinfo_set_service_scheme", "dnssd_getaddrinfo_set_delegate_pid",
	"dnssd_getaddrinfo_set_delegate_uuid", "dnssd_getaddrinfo_set_delegate_audit_token", "dnssd_getaddrinfo_set_account_id",
	"dnssd_getaddrinfo_set_need_encrypted_query", "dnssd_getaddrinfo_set_fallback_config", "dnssd_getaddrinfo_set_resolver_uuids",
	"dnssd_getaddrinfo_set_use_failover", "dnssd_getaddrinfo_set_prohibit_encrypted_dns", "dnssd_getaddrinfo_set_log_privacy_level",
	"dnssd_getaddrinfo_set_need_authenticated_results", "dnssd_getaddrinfo_set_validation_data",
	"dnssd_getaddrinfo_result_get_address", "dnssd_getaddrinfo_result_get_protocol", "dnssd_getaddrinfo_result_get_service_data",
	"dnssd_getaddrinfo_result_get_tracker_hostname", "dnssd_getaddrinfo_result_get_tracker_owner",
	"dnssd_getaddrinfo_result_get_tracker_is_approved", "dnssd_getaddrinfo_result_get_authentication_tag",
	"dnssd_getaddrinfo_result_get_validation_data", "dnssd_getaddrinfo_result_is_from_cache",
	"dnssd_getaddrinfo_result_get_extended_dns_error", "dnssd_getaddrinfo_result_get_provider_name",
	"dnssd_getaddrinfo_result_get_ech_config", "dnssd_getaddrinfo_result_get_svcb", "dnssd_getaddrinfo_result_get_negative_reason",
	"dnssd_getaddrinfo_result_get_ttl", "dnssd_getaddrinfo_result_get_svcb_alpn_values",
	"dnssd_getaddrinfo_result_enumerate_alpn_values", "dnssd_getaddrinfo_result_get_doh_uri", "dnssd_getaddrinfo_result_get_port",
}

// An experiment rather than a check of the library: which of nwCandidates
// macOS exports, and which of them a connection Network.framework makes to
// a host by name calls, as a library interposing them logs. That says
// where the container library could answer a name for Network.framework,
// as it does for getaddrinfo. It logs the answers, and fails only when the
// experiment cannot run.
func TestNetworkFrameworkExperiment(t *testing.T) {
	dir := t.TempDir()
	probe := filepath.Join(dir, "nwprobe")
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-Wall", "-Werror", "-framework", "Network", "-o", probe, "testdata/nwprobe/main.c").CombinedOutput(); err != nil {
		t.Fatalf("build the probe: %v\n%s", err, out)
	}
	// Every dnssd_ function the dnssd client library exports, from the
	// SDK's stub and from the loader's own view of the library, joins the
	// candidates.
	candidates := append([]string(nil), nwCandidates...)
	seen := map[string]bool{}
	for _, c := range candidates {
		seen[c] = true
	}
	var listings []string
	if sdk, err := exec.Command("/usr/bin/xcrun", "--show-sdk-path").Output(); err == nil {
		if b, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(sdk)), "usr/lib/system/libsystem_dnssd.tbd")); err == nil {
			listings = append(listings, string(b))
		}
	}
	if b, err := exec.Command("/usr/bin/dyld_info", "-exports", "/usr/lib/system/libsystem_dnssd.dylib").Output(); err == nil {
		listings = append(listings, string(b))
	}
	// The library's own export trie, as the loader mapped it.
	b, err := exec.Command(probe, "exports", "libsystem_dnssd").CombinedOutput()
	listings = append(listings, string(b))
	t.Logf("Network.framework experiment, libsystem_dnssd's export trie (%v), %d bytes:\n%.2000s", err, len(b), b)
	for _, l := range listings {
		for _, m := range regexp.MustCompile(`_?(dnssd_[A-Za-z0-9_]+)`).FindAllStringSubmatch(l, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				candidates = append(candidates, m[1])
			}
		}
	}
	out, err := exec.Command(probe, append([]string{"exists"}, candidates...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("exists: %v\n%s", err, out)
	}
	var exported []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name, ok := strings.CutSuffix(line, " exported"); ok {
			exported = append(exported, name)
		}
	}
	t.Logf("Network.framework experiment, %d candidates, exported:\n%s", len(candidates), strings.Join(exported, "\n"))

	// A tracer: each stub writes its function's name to stderr, then jumps
	// to the function with the caller's registers and stack as they were.
	var asm strings.Builder
	asm.WriteString(".text\n")
	for i, s := range exported {
		fmt.Fprintf(&asm, ".weak_reference _%[2]s\n.p2align 2\nstub_%[1]d:\n"+
			"\tstp x29, x30, [sp, #-96]!\n\tmov x29, sp\n"+
			"\tstp x0, x1, [sp, #16]\n\tstp x2, x3, [sp, #32]\n\tstp x4, x5, [sp, #48]\n\tstp x6, x7, [sp, #64]\n\tstr x8, [sp, #80]\n"+
			"\tadrp x0, name_%[1]d@PAGE\n\tadd x0, x0, name_%[1]d@PAGEOFF\n\tbl _fws_trace_hit\n"+
			"\tldp x0, x1, [sp, #16]\n\tldp x2, x3, [sp, #32]\n\tldp x4, x5, [sp, #48]\n\tldp x6, x7, [sp, #64]\n\tldr x8, [sp, #80]\n"+
			"\tldp x29, x30, [sp], #96\n\tb _%[2]s\n", i, s)
	}
	asm.WriteString(".section __TEXT,__cstring\n")
	for i, s := range exported {
		fmt.Fprintf(&asm, "name_%d:\n\t.asciz \"trace %s\\n\"\n", i, s)
	}
	asm.WriteString(".section __DATA,__interpose\n.p2align 3\n")
	for i, s := range exported {
		fmt.Fprintf(&asm, ".quad stub_%d\n.quad _%s\n", i, s)
	}
	src, hit, lib := filepath.Join(dir, "trace.s"), filepath.Join(dir, "hit.c"), filepath.Join(dir, "trace.dylib")
	if err := os.WriteFile(src, []byte(asm.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	// write(2) alone, which touches no floating-point register a traced
	// function's caller may have passed an argument in.
	if err := os.WriteFile(hit, []byte("#include <string.h>\n#include <unistd.h>\nvoid fws_trace_hit(const char *s) { write(2, s, strlen(s)); }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-dynamiclib", "-O2", "-Wl,-undefined,dynamic_lookup", "-o", lib, src, hit).CombinedOutput(); err != nil {
		t.Fatalf("build the tracer: %v\n%s", err, out)
	}
	// What the client hands Network.framework: each result's type,
	// protocol, interface and negative reason, and each event, which the
	// container library's stand-in answers in its place.
	results := filepath.Join(dir, "results.dylib")
	if err := os.WriteFile(filepath.Join(dir, "results.c"), []byte(nwResultsSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/xcrun", "clang", "-dynamiclib", "-O2", "-Wl,-undefined,dynamic_lookup", "-o", results, filepath.Join(dir, "results.c")).CombinedOutput(); err != nil {
		t.Fatalf("build the result printer: %v\n%s", err, out)
	}
	for _, host := range []string{"one.one.one.one", "localhost", "nothing-answers.invalid"} {
		cmd := exec.Command(probe, host, "80")
		cmd.Env = append(os.Environ(), "DYLD_INSERT_LIBRARIES="+results)
		out, err := cmd.CombinedOutput()
		t.Logf("Network.framework experiment, %s, the client's results (%v):\n%s", host, err, out)
	}
	for _, host := range []string{"one.one.one.one", "localhost"} {
		cmd := exec.Command(probe, host, "80")
		cmd.Env = append(os.Environ(), "DYLD_INSERT_LIBRARIES="+lib)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("%s: %v", host, err)
		}
		counts := map[string]int{}
		var other []string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if name, ok := strings.CutPrefix(line, "trace "); ok {
				counts[name]++
			} else {
				other = append(other, line)
			}
		}
		var hits []string
		for n, c := range counts {
			hits = append(hits, fmt.Sprintf("%s x%d", n, c))
		}
		sort.Strings(hits)
		t.Logf("Network.framework experiment, %s: %s\ncalled:\n%s", host, strings.Join(other, " | "), strings.Join(hits, "\n"))
	}
}

// nwResultsSource prints what the dnssd client hands its caller's
// handlers: each result's fields, as numbers, and each event.
const nwResultsSource = `#include <Block.h>
#include <stdint.h>
#include <stdio.h>
#include <sys/socket.h>
typedef void *gai_t, *res_t;
typedef void (^result_handler_t)(res_t *results, size_t count);
typedef void (^event_handler_t)(int event, int error);
extern void dnssd_getaddrinfo_set_result_handler(gai_t, result_handler_t);
extern void dnssd_getaddrinfo_set_event_handler(gai_t, event_handler_t);
extern const struct sockaddr *dnssd_getaddrinfo_result_get_address(res_t);
extern int dnssd_getaddrinfo_result_get_type(res_t);
extern int dnssd_getaddrinfo_result_get_protocol(res_t);
extern uint32_t dnssd_getaddrinfo_result_get_interface_index(res_t);
extern int dnssd_getaddrinfo_result_get_negative_reason(res_t);
static void set_result_handler(gai_t g, result_handler_t h) {
    if (!h) {
        dnssd_getaddrinfo_set_result_handler(g, h);
        return;
    }
    dnssd_getaddrinfo_set_result_handler(g, ^(res_t *rs, size_t n) {
        for (size_t i = 0; i < n; i++) {
            const struct sockaddr *sa = dnssd_getaddrinfo_result_get_address(rs[i]);
            fprintf(stderr, "result type=%d protocol=%d interface=%u negative=%d family=%d len=%d\n",
                    dnssd_getaddrinfo_result_get_type(rs[i]), dnssd_getaddrinfo_result_get_protocol(rs[i]),
                    dnssd_getaddrinfo_result_get_interface_index(rs[i]), dnssd_getaddrinfo_result_get_negative_reason(rs[i]),
                    sa ? sa->sa_family : -1, sa ? sa->sa_len : -1);
        }
        h(rs, n);
    });
}
static void set_event_handler(gai_t g, event_handler_t h) {
    if (!h) {
        dnssd_getaddrinfo_set_event_handler(g, h);
        return;
    }
    dnssd_getaddrinfo_set_event_handler(g, ^(int event, int error) {
        fprintf(stderr, "event %d error %d\n", event, error);
        h(event, error);
    });
}
__attribute__((used, section("__DATA,__interpose"))) static const void *interpose[] = {
    (const void *)set_result_handler, (const void *)dnssd_getaddrinfo_set_result_handler,
    (const void *)set_event_handler, (const void *)dnssd_getaddrinfo_set_event_handler,
};
`
