# Sanitization evidence

Run from the clean repository root on 2026-07-20.

## Release-blocking values

The release-gate regular expression supplied by the private scan report was run over every file with binary scanning enabled:

```text
LC_ALL=C rg -a -n -i "$BLOCKED_PATTERN" .
exit=1
matches=0
```

An additional scan for private project names, all space/hyphen/underscore forms of internal tool names, and absolute private workspace/home prefixes also returned `exit=1`, `matches=0`.

The replacement Flutter app was scanned separately for private names, tailnet
addresses, hostnames, real UUIDs, and high-entropy auth-key shapes. It returned
`exit=1`, `matches=0`. Scans for the former Android package/project names and
for Chinese source or script text also returned `exit=1`, `matches=0`.

## Excluded artifacts

```text
forbidden credential/runtime/generated file matches=0
forbidden runtime/generated directory matches=0
internal qa script matches=0
Flutter/Android binary or local-config matches=0
Flutter/Android generated directory matches=0
```

The scan includes generated executables/archives, Android build/cache outputs, Web dependency/build outputs, OS metadata, local SDK configuration, and the private runtime/evidence directories listed in `OPEN_SOURCE_WHITELIST.md`.

## Source verification

```text
gateway: GOMAXPROCS=2 go test -count=1 ./...  PASS
gateway: GOMAXPROCS=2 go vet ./...            PASS
web: npm ci                                   PASS
web: npm run build                            PASS
web: npm run lint                             PASS
app: flutter analyze                          PASS (No issues found)
app: flutter test                             PASS (42 tests)
mock JSON parse                               PASS
mock session-to-timeline links                PASS
shell syntax                                  PASS
gateway lifecycle on 127.0.0.1:18799          PASS
```

The lifecycle probe used `TSNET_DISABLE=1` and isolated pid, log, and state
paths. It verified `up -> health -> status -> down`, repeated up/down behavior,
SIGTERM exit code 0, state flushes, pidfile removal, and listener release.

`web/node_modules`, `web/dist`, Flutter `.dart_tool`, Android `.gradle`, all
build output, generated plugin files, and locally built gateway executables
were removed after verification. APK construction was intentionally left to
the release-build stage; no APK, AAR, shared library, or Gradle wrapper JAR was
copied into the source tree.
