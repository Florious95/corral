# Corral source whitelist

This directory was assembled from a positive whitelist. It was not produced by deleting files from the private workspace.

## Included

- `gateway/*.go`, including tests.
- `gateway/go.mod`, `gateway/go.sum`, and the CGo FSEvents bridge: `v2_fsevents_darwin.c` and `v2_fsevents_darwin.h`.
- `web/src/**`, `web/mock/**`, and the Web build manifests (`package*.json`, TypeScript configs, Vite config, linter config, and `index.html`).
- The Flutter Android app under `app/`: Dart sources and tests, pubspec files,
  Android host sources and manifests, Gradle text files, and user-facing build script.
- User-facing scripts only: `scripts/corral-up.sh`, `scripts/corral-status.sh`, `scripts/corral-down.sh`, `scripts/publish-web-dist.sh`, and `scripts/README.md`.
- The `@florious95/corral` npm manifest, `cli/` JavaScript sources and tests,
  and the tag-triggered release workflow under `.github/workflows/`.
- Open-source preparation notes under `docs/`.

## Excluded

- Credentials and private runtime state: the private SSH credential file, `.team/`, `.claude/`, and `local.properties`.
- Internal acceptance assets: every `scripts/qa-*`, `qa/`, `reports/`, and `Docs/QA/evidence/` path.
- Generated Web output: `web/dist/` and `web/artifacts/`.
- Generated Flutter and Android output: `.dart_tool/`, `.flutter-plugins-dependencies`,
  every `build/` or `.gradle/` directory, and `local.properties`.
- Executables and archives: `gateway/gateway*`, `*.apk`, `*.aar`, `*.jar`, `*.class`, and build cache `*.bin` files.
- Downloaded or generated package assets: `cli/vendor/`, release tarballs and
  checksums, and npm package archives.
- OS metadata such as `.DS_Store`.

The Gradle wrapper JAR is intentionally absent because binary JAR files are outside the whitelist. Contributors can use Flutter tooling or regenerate the wrapper from a reviewed distribution.

## Sanitized test constants

The private source locations called out during the scan were copied, then replaced only in this clean directory:

- `gateway/sessions_test.go`: upload paths at the former lines 1293/1296 and the queued-message token fixture near the former line 1315.
- `gateway/binding_state_test.go`: the project path/session fixture near the former line 255 and the placeholder cwd/title near the former line 406.
- `gateway/v2_snapshot_test.go`: the host ID and tmux socket fixture near the former line 88.
- Additional real-shaped session IDs found in `sessions_test.go`, `binding_state_test.go`, and `v2_write_test.go` were replaced with visibly synthetic UUIDs.

## Script boundary

The copied scripts are end-user helpers. Private QA scripts were not generalized because they encode one-off production allowlists, write gates, evidence paths, and topology assumptions; publishing those as supported commands would be misleading.

Remote Web publishing has no default host or credential. Set `RC_SSH_HOST`, optional `RC_CREDENTIAL_FILE`, `RC_TARGET_URL`, and optional destination variables explicitly. Local publishing remains the no-configuration default.

The npm command is `corral`, the package is `@florious95/corral`, and release
assets are downloaded from the matching version tag in the `Florious95/corral`
repository.
