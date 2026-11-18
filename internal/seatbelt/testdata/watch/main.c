// watch watches directory argv[1] through FSEvents, as libuv's fs.watch
// does for a directory, makes a file in it and waits for the event: it
// exits 0 on the event, 2 if the stream would not start (which libuv
// reports as EMFILE), and 3 if no event came.
#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>
#include <fcntl.h>
#include <stdio.h>
#include <unistd.h>

static dispatch_semaphore_t seen;

static void changed(ConstFSEventStreamRef s, void *info, size_t n, void *paths,
                    const FSEventStreamEventFlags flags[], const FSEventStreamEventId ids[]) {
    dispatch_semaphore_signal(seen);
}

int main(int argc, char **argv) {
    if (argc != 2) {
        fprintf(stderr, "usage: watch dir\n");
        return 1;
    }
    seen = dispatch_semaphore_create(0);
    CFStringRef dir = CFStringCreateWithCString(NULL, argv[1], kCFStringEncodingUTF8);
    CFArrayRef paths = CFArrayCreate(NULL, (const void **)&dir, 1, &kCFTypeArrayCallBacks);
    FSEventStreamRef s = FSEventStreamCreate(NULL, changed, NULL, paths, kFSEventStreamEventIdSinceNow, 0.05,
                                             kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagNoDefer);
    if (s == NULL) {
        fprintf(stderr, "FSEventStreamCreate failed\n");
        return 2;
    }
    FSEventStreamSetDispatchQueue(s, dispatch_queue_create("watch", NULL));
    if (!FSEventStreamStart(s)) {
        fprintf(stderr, "FSEventStreamStart failed\n");
        return 2;
    }
    char file[4096];
    snprintf(file, sizeof file, "%s/changed", argv[1]);
    for (int i = 0; i < 20; i++) {
        int fd = open(file, O_WRONLY | O_CREAT | O_TRUNC, 0644);
        if (fd < 0) {
            perror(file);
            return 1;
        }
        write(fd, "x", 1);
        close(fd);
        if (dispatch_semaphore_wait(seen, dispatch_time(DISPATCH_TIME_NOW, 500 * NSEC_PER_MSEC)) == 0) {
            return 0;
        }
    }
    fprintf(stderr, "no event for %s\n", argv[1]);
    return 3;
}
