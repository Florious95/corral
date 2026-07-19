# Corral helper scripts

Start the gateway:

```bash
./scripts/corral-up.sh
```

Show its process, listeners, tsnet state, and local tmux pane count:

```bash
./scripts/corral-status.sh
```

Stop it gracefully:

```bash
./scripts/corral-down.sh
```

The scripts store the managed process ID in
`~/Library/Application Support/Corral/gateway.pid`. Override this with
`CORRAL_PIDFILE`; use `CORRAL_LOG` to select another log file.

For an isolated local instance without tsnet:

```bash
GATEWAY_ADDR=127.0.0.1:18799 \
TSNET_DISABLE=1 \
CORRAL_PIDFILE=/tmp/corral-18799.pid \
CORRAL_LOG=/tmp/corral-18799.log \
./scripts/corral-up.sh
```

Use the same `CORRAL_PIDFILE` and `CORRAL_LOG` values with the status and down
scripts.

Publish an already built Web bundle locally with
`./scripts/publish-web-dist.sh`. Remote publishing requires explicit
`RC_SSH_HOST`, `RC_CREDENTIAL_FILE`, and `RC_TARGET_URL` values; no remote host
or credential is built in.
