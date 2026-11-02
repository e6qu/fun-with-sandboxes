// mach registers a bootstrap service and looks it up as Chrome's port
// rendezvous does, in a sandbox that lets it register names under its IPC
// prefix alone, as a container's does, and reports both results.
#include <servers/bootstrap.h>
#include <stdio.h>
#include <unistd.h>

int sandbox_init(const char *profile, uint64_t flags, char **err);

int main(void) {
    char *err = NULL;
    if (sandbox_init("(version 1)(allow default)(deny mach-register)(allow mach-register (global-name-prefix \"fwstest.\"))", 0, &err) != 0) {
        fprintf(stderr, "sandbox_init: %s\n", err);
        return 1;
    }
    name_t name;
    snprintf(name, sizeof name, "org.example.Rendezvous.%d", getpid());
    mach_port_t server = MACH_PORT_NULL, client = MACH_PORT_NULL;
    kern_return_t in = bootstrap_check_in(bootstrap_port, name, &server);
    kern_return_t up = in == KERN_SUCCESS ? bootstrap_look_up(bootstrap_port, name, &client) : in;
    printf("%d %d\n", in == KERN_SUCCESS, up == KERN_SUCCESS && client != MACH_PORT_NULL);
    return 0;
}
