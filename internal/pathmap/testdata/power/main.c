// power registers for system power notifications as Chrome's power monitor
// does, in a sandbox that denies IOKit's user clients as a container's
// does, and reports whether it got a registration and a run loop source.
#include <IOKit/pwr_mgt/IOPMLib.h>
#include <stdio.h>

int sandbox_init(const char *profile, uint64_t flags, char **err);

static void callback(void *refcon, io_service_t service, uint32_t type, void *arg) {
    (void)refcon, (void)service, (void)type, (void)arg;
}

int main(void) {
    char *err = NULL;
    if (sandbox_init("(version 1)(allow default)(deny iokit-open-user-client)", 0, &err) != 0) {
        fprintf(stderr, "sandbox_init: %s\n", err);
        return 1;
    }
    IONotificationPortRef port = NULL;
    io_object_t notifier = 0;
    io_connect_t conn = IORegisterForSystemPower(NULL, &port, callback, &notifier);
    printf("%d %d\n", conn != 0, port != NULL && IONotificationPortGetRunLoopSource(port) != NULL);
    return 0;
}
