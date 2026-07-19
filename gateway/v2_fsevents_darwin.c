//go:build darwin && cgo

#include "v2_fsevents_darwin.h"

#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>
#include <stdlib.h>

struct RCV2FSEvents {
    FSEventStreamRef stream;
    dispatch_queue_t queue;
};

static void rc_v2_fsevents_callback(ConstFSEventStreamRef stream,
                                     void *info,
                                     size_t event_count,
                                     void *event_paths,
                                     const FSEventStreamEventFlags event_flags[],
                                     const FSEventStreamEventId event_ids[]) {
    (void)stream;
    (void)event_ids;
    char **paths = (char **)event_paths;
    uintptr_t token = (uintptr_t)info;
    for (size_t index = 0; index < event_count; index++) {
        goV2FSEventsEmit(token, paths[index], (uint32_t)event_flags[index]);
    }
}

RCV2FSEvents *rc_v2_fsevents_start(uintptr_t token, const char *path1, const char *path2) {
    CFStringRef strings[2];
    CFIndex count = 0;
    if (path1 != NULL) {
        strings[count++] = CFStringCreateWithCString(NULL, path1, kCFStringEncodingUTF8);
    }
    if (path2 != NULL) {
        strings[count++] = CFStringCreateWithCString(NULL, path2, kCFStringEncodingUTF8);
    }
    if (count == 0) {
        return NULL;
    }
    CFArrayRef paths = CFArrayCreate(NULL, (const void **)strings, count, &kCFTypeArrayCallBacks);
    for (CFIndex index = 0; index < count; index++) {
        CFRelease(strings[index]);
    }
    if (paths == NULL) {
        return NULL;
    }

    FSEventStreamContext context = {0, (void *)token, NULL, NULL, NULL};
    FSEventStreamCreateFlags flags = kFSEventStreamCreateFlagFileEvents |
                                     kFSEventStreamCreateFlagNoDefer |
                                     kFSEventStreamCreateFlagWatchRoot;
    FSEventStreamRef stream = FSEventStreamCreate(NULL, rc_v2_fsevents_callback, &context,
                                                   paths, kFSEventStreamEventIdSinceNow,
                                                   0.05, flags);
    CFRelease(paths);
    if (stream == NULL) {
        return NULL;
    }
    dispatch_queue_t queue = dispatch_queue_create("com.florious95.corral.gateway.v2-fsevents", DISPATCH_QUEUE_SERIAL);
    if (queue == NULL) {
        FSEventStreamRelease(stream);
        return NULL;
    }
    FSEventStreamSetDispatchQueue(stream, queue);
    if (!FSEventStreamStart(stream)) {
        FSEventStreamInvalidate(stream);
        FSEventStreamRelease(stream);
#if !OS_OBJECT_USE_OBJC
        dispatch_release(queue);
#endif
        return NULL;
    }
    FSEventStreamFlushSync(stream);
    RCV2FSEvents *watcher = calloc(1, sizeof(RCV2FSEvents));
    if (watcher == NULL) {
        FSEventStreamStop(stream);
        FSEventStreamInvalidate(stream);
        FSEventStreamRelease(stream);
#if !OS_OBJECT_USE_OBJC
        dispatch_release(queue);
#endif
        return NULL;
    }
    watcher->stream = stream;
    watcher->queue = queue;
    return watcher;
}

void rc_v2_fsevents_stop(RCV2FSEvents *watcher) {
    if (watcher == NULL) {
        return;
    }
    FSEventStreamStop(watcher->stream);
    FSEventStreamInvalidate(watcher->stream);
    FSEventStreamRelease(watcher->stream);
#if !OS_OBJECT_USE_OBJC
    dispatch_release(watcher->queue);
#endif
    free(watcher);
}

uint32_t rc_v2_fsevents_full_scan_flags(void) {
    return (uint32_t)(kFSEventStreamEventFlagMustScanSubDirs |
                      kFSEventStreamEventFlagUserDropped |
                      kFSEventStreamEventFlagKernelDropped |
                      kFSEventStreamEventFlagRootChanged);
}
