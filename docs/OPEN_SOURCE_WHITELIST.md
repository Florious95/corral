# Corral source whitelist

This directory was assembled from a positive whitelist. It was not produced by deleting files from the private workspace.

## Included

- `gateway/*.go`, including tests.
- `gateway/go.mod`, `gateway/go.sum`, and the CGo FSEvents bridge: `v2_fsevents_darwin.c` and `v2_fsevents_darwin.h`.
- `web/src/**`, `web/mock/**`, and the Web build manifests (`package*.json`, TypeScript configs, Vite config, linter config, and `index.html`).
- Android source and build manifests, including the Go source for `android/tsnet-wrapper`.
- User-facing scripts only: `scripts/corral-up.sh`, `scripts/publish-web-dist.sh`, and `scripts/README.md`.
- Open-source preparation notes under `docs/`.

## Excluded

- Credentials and private runtime state: the private SSH credential file, `.team/`, `.claude/`, and `local.properties`.
- Internal acceptance assets: every `scripts/qa-*`, `qa/`, `reports/`, and `Docs/QA/evidence/` path.
- Generated Web output: `web/dist/` and `web/artifacts/`.
- Generated Android output: every `build/` directory.
- Executables and archives: `gateway/gateway*`, `*.apk`, `*.aar`, `*.jar`, `*.class`, and build cache `*.bin` files.
- OS metadata such as `.DS_Store`.

The Gradle wrapper JAR is intentionally absent because binary JAR files are outside the whitelist. Contributors can use an installed Gradle or regenerate the wrapper from a reviewed distribution.

## Sanitized test constants

The private source locations called out during the scan were copied, then replaced only in this clean directory:

- `gateway/sessions_test.go`: upload paths at the former lines 1293/1296 and the queued-message token fixture near the former line 1315.
- `gateway/binding_state_test.go`: the project path/session fixture near the former line 255 and the placeholder cwd/title near the former line 406.
- `gateway/v2_snapshot_test.go`: the host ID and tmux socket fixture near the former line 88.
- Additional real-shaped session IDs found in `sessions_test.go`, `binding_state_test.go`, and `v2_write_test.go` were replaced with visibly synthetic UUIDs.

## Script boundary

The copied scripts are end-user helpers. Private QA scripts were not generalized because they encode one-off production allowlists, write gates, evidence paths, and topology assumptions; publishing those as supported commands would be misleading.

Remote Web publishing has no default host or credential. Set `RC_SSH_HOST`, optional `RC_CREDENTIAL_FILE`, `RC_TARGET_URL`, and optional destination variables explicitly. Local publishing remains the no-configuration default.

The names `corral`, `@florious95/cli`, and the `Florious95` repository owner are reserved for the CLI/repository packaging pass. This extraction includes the gateway, Web, and Android source; it does not invent a CLI implementation that is not present in the source workspace.
