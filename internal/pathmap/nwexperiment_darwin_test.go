package pathmap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	out, err := exec.Command(probe, append([]string{"exists"}, nwCandidates...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("exists: %v\n%s", err, out)
	}
	var exported []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name, ok := strings.CutSuffix(line, " exported"); ok {
			exported = append(exported, name)
		}
	}
	t.Logf("Network.framework experiment, exported:\n%s", out)

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
		t.Logf("Network.framework experiment, %s: %s\ncalled: %s", host, strings.Join(other, " | "), strings.Join(hits, ", "))
	}
}
