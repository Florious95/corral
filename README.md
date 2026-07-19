<h1 align="center">Corral</h1>

<p align="center"><b>Round up every coding-agent CLI already running on your machines — and reach them all from your phone.</b></p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="#why-corral">Why</a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="#security-model">Security</a>
</p>

---

Corral is a self-hosted control plane for the coding agents you *already run* — Claude Code, Codex, and anything else living in a tmux pane. It does **not** spawn your agents or ask you to change how you start them. It attaches to the sessions already there, presents them as a clean chat UI (web + Android), and makes every one of them reachable from your phone over your own private Tailscale network — no relay server, no account, no telemetry.

> **adopt, don't own.** Other tools want to be the thing that launches your agents. Corral just connects to the ones you've got.

## Why Corral

I built this after using Anthropic's official Remote Control for a while and hitting three walls:

1. **Its traffic goes through a relay.** On a network that needs a proxy, the whole session rides that proxy. Corral is point-to-point over Tailscale — packets go straight from your phone to your machine, through nobody's server.
2. **It only sees Claude.** My Codex sessions were invisible. Corral lists every agent CLI in one place, whatever the vendor.
3. **You arm it per terminal.** Every session needs an explicit opt-in command. Corral picks up a live CLI automatically — if it's running, it's already in the list.

And one thing none of them do: **multi-host.** One list spans every machine running a Corral gateway. Add a laptop, it joins the herd.

## Quick start

On each machine you want to reach (needs [Tailscale](https://tailscale.com/download), tmux, Go, and at least one of Claude Code / Codex):

```bash
git clone https://github.com/Florious95/corral && cd corral
./scripts/corral-up.sh      # build + start the gateway, print its URL
./scripts/corral-status.sh  # pid, listener, tsnet state, attached panes
./scripts/corral-down.sh    # graceful stop (SIGTERM, state flushed)
```

`corral-up.sh` builds and starts the gateway, exposes it on your tailnet, and prints the URL. Open it in any browser on any device in your tailnet.

For your phone, build the Android app from [`app/`](app/) (Flutter; see `app/build.sh`) — a prebuilt APK will be attached to Releases once signing is settled. On first launch, enter a Tailscale auth key in the app's settings; the key is stored encrypted on-device, never compiled into the APK. The app embeds a userspace Tailscale node, so it doesn't take your phone's system VPN slot and works over cellular with no external client.

Dev mode without Tailscale: `TSNET_DISABLE=1 ./scripts/corral-up.sh` serves on `localhost:8787`.

## How it works

- **Pane-first, not process-owning.** The gateway watches your tmux sockets, verifies each pane's real agent process, and binds it to a session — reusing what's there instead of launching anything. Compact, switch models, close a window: your session survives and stays reachable.
- **Structured chat over a raw fallback.** Agents render as message timelines (favorites, search, per-host grouping, deep-scroll history from the server, local cache for instant reopens). When a CLI does something the structured view can't show — an interactive menu, a TUI prompt — a raw terminal panel with a key pad lets you drive it directly.
- **Delivery you can trust.** Every send is tracked from POST through injection to echo confirmation; the UI never shows a false "sent."
- **Event-driven identity.** Discovery, binding, and reconciliation run off filesystem and process events, not polling — a slow or broken pane is isolated, it never freezes the whole fleet.

Transcript-format knowledge (how Claude Code / Codex write their JSONL) is isolated behind versioned adapters — see [`docs/ADAPTER_DESIGN.md`](docs/ADAPTER_DESIGN.md). Everything about tmux identity, binding, pagination, and delivery stays in the gateway.

## Security model

Corral has **no built-in authentication** — its trust boundary is your tailnet. The gateway listens on `:8787` on all interfaces *on purpose* (that's how your phone reaches it over Tailscale and your LAN), and relies on Tailscale's "a device must be on the network to talk to it" model plus directed process-identity verification before any send.

That means:

- **Do not expose `:8787` to the public internet.** Keep it inside your tailnet.
- Auth keys and gateway addresses live on-device; the Android app stores its key in the Android Keystore.
- There is no account system and no cloud — you host all of it.

If your threat model needs per-user auth, put an authenticating reverse proxy in front of the gateway. Corral assumes tailnet membership *is* the authorization.

## Status & scope

This is a personal project, maintained as time allows. It runs in production for its author across multiple machines, but it is offered as-is under the [MIT license](LICENSE) — no SLA, no guarantees. Issues and PRs welcome; response times vary.

Compatibility is validated against specific CLI versions (Claude Code, Codex) noted in the adapter docs; other versions may need adapter updates.
