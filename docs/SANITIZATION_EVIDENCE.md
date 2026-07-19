# Sanitization evidence

Run from `/Volumes/nvme/tmp/corral-clean` on 2026-07-20.

## Release-blocking values

The release-gate regular expression supplied by the private scan report was run over every file with binary scanning enabled:

```text
LC_ALL=C rg -a -n -i "$BLOCKED_PATTERN" .
exit=1
matches=0
```

An additional scan for private project names, all space/hyphen/underscore forms of internal tool names, and absolute private workspace/home prefixes also returned `exit=1`, `matches=0`.

## Excluded artifacts

```text
forbidden credential/runtime/generated file matches=0
forbidden runtime/generated directory matches=0
internal qa script matches=0
.git directory matches=0
```

The scan includes generated executables/archives, Android build/cache outputs, Web dependency/build outputs, OS metadata, local SDK configuration, and the private runtime/evidence directories listed in `OPEN_SOURCE_WHITELIST.md`.

## Source verification

```text
gateway: GOMAXPROCS=2 go test -count=1 ./...  PASS
gateway: GOMAXPROCS=2 go vet ./...            PASS
web: npm ci                                   PASS
web: npm run build                            PASS
web: npm run lint                             PASS
mock JSON parse                               PASS
mock session-to-timeline links                PASS
shell syntax                                  PASS
```

`web/node_modules`, `web/dist`, Android `.gradle`, and Android build output were removed after verification.

Android compilation remains an environment boundary rather than a pass: the clean tree intentionally excludes the Gradle wrapper JAR, and the local offline Gradle cache lacks the Android/Kotlin plugin artifacts. The tsnet wrapper also requires Go 1.26.3 while the local toolchain is 1.26.1; automatic toolchain download was denied by the current dependency proxy. Neither condition was bypassed by copying binaries or lowering declared versions.
