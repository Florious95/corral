#ifndef RC_V2_FSEVENTS_DARWIN_H
#define RC_V2_FSEVENTS_DARWIN_H

#include <stdint.h>

typedef struct RCV2FSEvents RCV2FSEvents;

extern void goV2FSEventsEmit(uintptr_t token, char *path, uint32_t flags);

RCV2FSEvents *rc_v2_fsevents_start(uintptr_t token, const char *path1, const char *path2);
void rc_v2_fsevents_stop(RCV2FSEvents *watcher);
uint32_t rc_v2_fsevents_full_scan_flags(void);

#endif
