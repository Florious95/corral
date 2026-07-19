import 'dart:async';
import 'dart:io';

import 'package:flutter/material.dart';
import 'package:tailscale/tailscale.dart';
import 'package:webview_flutter/webview_flutter.dart';
import 'package:webview_flutter_android/webview_flutter_android.dart';

import 'app_bridge.dart';
import 'app_lifecycle.dart';
import 'gateway_discovery.dart';
import 'loopback_gateway_proxy.dart';
import 'settings_page.dart';
import 'webview_resume_guard.dart';

const _authKey = String.fromEnvironment('TS_AUTHKEY');
const _gatewayIp = String.fromEnvironment('GATEWAY_IP');
const webViewTopInsetFixScript = '''
(() => {
  const id = 'corral-top-inset-fix';
  if (document.getElementById(id)) return;
  const style = document.createElement('style');
  style.id = id;
  style.textContent = '.wa-header { padding-top: 10px !important; }';
  document.head.appendChild(style);
})();
''';

String normalizeAuthKey(String value) {
  final normalized = value.trim();
  if (!normalized.startsWith('tskey-auth-') || normalized.length < 20) {
    throw const FormatException('invalid Tailscale authkey format');
  }
  return normalized;
}

String normalizeStoredWebRoute(String? value) {
  final parsed = Uri.tryParse(value ?? '');
  if (parsed == null || parsed.hasScheme || parsed.hasAuthority) return '/';
  final path = parsed.path.startsWith('/') ? parsed.path : '/${parsed.path}';
  return Uri(
    path: path.isEmpty ? '/' : path,
    query: parsed.hasQuery ? parsed.query : null,
    fragment: parsed.hasFragment ? parsed.fragment : null,
  ).toString();
}

String? webRouteFromLoopbackUrl(String? value, int proxyPort) {
  final uri = Uri.tryParse(value ?? '');
  if (uri == null || uri.host != '127.0.0.1' || uri.port != proxyPort) {
    return null;
  }
  return normalizeStoredWebRoute(
    Uri(
      path: uri.path,
      query: uri.hasQuery ? uri.query : null,
      fragment: uri.hasFragment ? uri.fragment : null,
    ).toString(),
  );
}

void main() {
  runApp(const ProbeApp());
}

class ProbeApp extends StatefulWidget {
  const ProbeApp({super.key, this.appBridge = const AppBridge()});

  final AppBridge appBridge;

  @override
  State<ProbeApp> createState() => _ProbeAppState();
}

class _ProbeAppState extends State<ProbeApp> with WidgetsBindingObserver {
  late final AppBridge _appBridge;
  final _lines = <String>[];
  final _authKeyController = TextEditingController();
  final _webViewResumeGuard = WebViewResumeGuard();
  LoopbackGatewayProxy? _proxy;
  WebViewController? _webViewController;
  String? _savedAuthKey;
  String? _manualGatewayAddress;
  String? _persistentStateDir;
  Uri? _currentGateway;
  List<TailnetNodeView> _tailnetNodes = const [];
  var _connecting = false;
  var _restoringSettings = true;
  var _manualGatewayChoiceMade = false;
  var _tailscaleInitialized = false;
  var _proxyPort = defaultLoopbackProxyPort;
  var _lastWebRoute = '/';

  @override
  void initState() {
    super.initState();
    _appBridge = widget.appBridge;
    WidgetsBinding.instance.addObserver(this);
    unawaited(_restoreSettings());
  }

  @override
  void dispose() {
    WidgetsBinding.instance.removeObserver(this);
    _authKeyController.dispose();
    unawaited(_proxy?.close());
    super.dispose();
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    if (state == AppLifecycleState.resumed) unawaited(_handleResume());
  }

  Future<void> _handleResume() async {
    final controller = _webViewController;
    if (controller == null) return;
    final reloaded = await _webViewResumeGuard.reloadOnResumeIfNeeded(
      hasActiveStream: _proxy?.hasActiveStream ?? false,
      reload: controller.reload,
    );
    _log(
      reloaded
          ? 'app resumed with no active stream; WebView reloaded'
          : 'app resumed; WebView kept because stream or file chooser is active',
    );
  }

  Future<void> _restoreSettings() async {
    final stored = await _appBridge.loadSettings();
    if (!mounted) return;
    setState(() {
      _savedAuthKey = stored.authKey;
      _manualGatewayAddress = stored.gatewayOverride;
      _manualGatewayChoiceMade = stored.gatewayChoiceMade;
      _persistentStateDir = stored.stateDir;
      _proxyPort = stored.proxyPort;
      _lastWebRoute = normalizeStoredWebRoute(stored.lastWebRoute);
      _restoringSettings = false;
    });
    final startup = planStartupConnection(
      storedAuthKey: stored.authKey,
      buildTimeAuthKey: _authKey,
    );
    if (startup != null) {
      await _run(
        startup.authKey,
        reusePersistedSession: startup.reusePersistedSession,
      );
    }
  }

  void _log(String message) {
    final line = '${DateTime.now().toIso8601String()} $message';
    debugPrint('[corral] $line');
    if (mounted) setState(() => _lines.add(line));
  }

  Future<void> _run(
    String authKey, {
    bool reusePersistedSession = false,
  }) async {
    if (authKey.trim().isEmpty || _connecting) return;
    setState(() => _connecting = true);
    try {
      final normalizedAuthKey = normalizeAuthKey(authKey);
      _savedAuthKey = normalizedAuthKey;
      await _appBridge.saveAuthKey(normalizedAuthKey);
      _log('authkey format accepted length=${normalizedAuthKey.length}');
      final stateDir = Directory(
        _persistentStateDir ?? '${Directory.systemTemp.path}/corral',
      );
      await stateDir.create(recursive: true);
      if (!_tailscaleInitialized) {
        Tailscale.init(
          stateDir: stateDir.path,
          logLevel: TailscaleLogLevel.info,
        );
        _tailscaleInitialized = true;
      }

      _log('joining tailnet as corral-android');
      String? authKeyForUp = normalizedAuthKey;
      if (reusePersistedSession) {
        final persistedStatus = await Tailscale.instance.status();
        if (persistedStatus.state != NodeState.noState) authKeyForUp = null;
      }
      var status = await Tailscale.instance.up(
        hostname: 'corral-android',
        authKey: authKeyForUp,
        timeout: const Duration(seconds: 60),
      );
      _log('state=${status.state.name} ips=${status.tailscaleIPs.join(',')}');
      for (var attempt = 0; !status.isRunning && attempt < 90; attempt++) {
        if (status.state == NodeState.needsMachineAuth) break;
        await Future<void>.delayed(const Duration(seconds: 1));
        status = await Tailscale.instance.status();
        if (status.isRunning || attempt % 10 == 9) {
          _log(
            'enrollment state=${status.state.name} '
            'health=${status.health.join('|')}',
          );
        }
      }
      if (status.state != NodeState.running) {
        throw StateError('node is not running: ${status.state.name}');
      }
      await _connectTailnet(status.tailscaleIPs.toSet());
      _authKeyController.clear();
      _log('SUCCESS gateway=${_currentGateway?.authority}');
    } catch (error, stackTrace) {
      _log('FAILED: $error');
      debugPrintStack(stackTrace: stackTrace);
    } finally {
      if (mounted) setState(() => _connecting = false);
    }
  }

  Future<List<TailscaleNode>> _waitForNodes(Set<String> selfIps) async {
    var latest = const <TailscaleNode>[];
    for (var attempt = 0; attempt < 30; attempt++) {
      final nodes = await Tailscale.instance.nodes();
      latest = nodes;
      final onlinePeers = nodes.where(
        (node) =>
            node.online && node.ipv4 != null && !selfIps.contains(node.ipv4),
      );
      for (final node in nodes) {
        _log(
          'peer name=${node.hostName} ip=${node.ipv4} online=${node.online}',
        );
      }
      if (onlinePeers.isNotEmpty) return nodes;
      await Future<void>.delayed(const Duration(seconds: 1));
    }
    return latest;
  }

  Future<Map<String, Duration?>> _measureHealthyPeers(
    List<GatewayPeer> peers,
    Map<String, GatewayHealthResult> healthResults,
  ) async {
    final measured = await Future.wait(
      peers.where((peer) => healthResults[peer.stableNodeId]?.isOk == true).map(
        (peer) async {
          return (peer: peer, latency: await _medianRtt(peer));
        },
      ),
    );
    return {for (final item in measured) item.peer.stableNodeId: item.latency};
  }

  void _updateNodeViews(
    List<TailscaleNode> nodes,
    Set<String> selfIps,
    Map<String, GatewayHealthResult> healthResults,
    Map<String, Duration?> latencies,
  ) {
    final views =
        nodes
            .where((node) => node.ipv4 != null)
            .map((node) {
              final name = node.hostName.isNotEmpty
                  ? node.hostName
                  : node.dnsName.replaceFirst(RegExp(r'\.$'), '');
              final health = healthResults[node.stableNodeId];
              final isSelf = selfIps.contains(node.ipv4);
              return TailnetNodeView(
                name: name.isEmpty ? node.stableNodeId : name,
                ip: node.ipv4!,
                online: node.online,
                latency: latencies[node.stableNodeId],
                gatewayHealthOk: health?.isOk,
                gatewayHealthDetail: isSelf
                    ? 'Local node is not probed'
                    : !node.online
                    ? 'Offline, not probed'
                    : health?.failureReason ?? 'Not checked',
              );
            })
            .toList(growable: false)
          ..sort((a, b) {
            if (a.online != b.online) return a.online ? -1 : 1;
            return a.name.compareTo(b.name);
          });
    if (mounted) setState(() => _tailnetNodes = views);
  }

  Future<void> _connectTailnet(Set<String> selfIps) async {
    final nodes = await _waitForNodes(selfIps);
    final peers = nodes
        .where(
          (node) =>
              node.online && node.ipv4 != null && !selfIps.contains(node.ipv4),
        )
        .map(
          (node) => GatewayPeer(
            stableNodeId: node.stableNodeId,
            ip: node.ipv4!,
            online: true,
          ),
        )
        .toList();

    if (!_manualGatewayChoiceMade && _gatewayIp.trim().isNotEmpty) {
      final fallback = normalizeGatewayAddress(_gatewayIp);
      if (!peers.any((peer) => peer.ip == fallback.host)) {
        peers.add(
          GatewayPeer(
            stableNodeId: 'build-time-gateway-hint',
            ip: fallback.host,
            online: true,
          ),
        );
      }
    }
    if (peers.isEmpty) {
      _updateNodeViews(nodes, selfIps, const {}, const {});
      await _clearGatewayConnection();
      throw StateError('no online IPv4 peers found');
    }

    final healthProbe = HttpHealthProbe(client: Tailscale.instance.http.client);
    final healthEntries = await Future.wait(
      peers.map(
        (peer) async =>
            MapEntry(peer.stableNodeId, await healthProbe.diagnose(peer)),
      ),
    );
    final healthResults = Map.fromEntries(healthEntries);
    for (final peer in peers) {
      final result = healthResults[peer.stableNodeId]!;
      _log(
        'health ip=${peer.ip} ${result.isOk ? 'ok' : 'failed=${result.failureReason}'}',
      );
    }
    final latencies = await _measureHealthyPeers(peers, healthResults);
    _updateNodeViews(nodes, selfIps, healthResults, latencies);

    final override = preferredGatewayOverride(
      manual: _manualGatewayAddress,
      manualChoiceMade: _manualGatewayChoiceMade,
    );
    if (override != null) {
      final endpoint = normalizeGatewayAddress(override);
      _log('manual gateway override=${endpoint.authority}');
      await _openGateway(endpoint);
      return;
    }

    final discovery = GatewayDiscovery(
      healthProbe: (peer) async => healthResults[peer.stableNodeId]?.latency,
      rttProbe: (peer) async => latencies[peer.stableNodeId],
    );
    final discovered = await discovery.select(peers);
    if (discovered == null) await _clearGatewayConnection();
    final selected = requireGatewaySelection(discovered);
    _log(
      'selected id=${selected.peer.stableNodeId} ip=${selected.peer.ip} '
      'rttMs=${selected.score.inMilliseconds}',
    );
    await _openGateway(normalizeGatewayAddress(selected.peer.ip));
  }

  Future<void> _clearGatewayConnection() async {
    await _proxy?.close();
    if (!mounted) return;
    setState(() {
      _proxy = null;
      _webViewController = null;
      _currentGateway = null;
    });
  }

  Future<Duration?> _medianRtt(GatewayPeer peer) async {
    final median = await sampleMedianLatency(
      sample: (index) async => (await Tailscale.instance.diag.ping(
        peer.ip,
        timeout: const Duration(seconds: 5),
      )).latency,
      onError: (index, error) =>
          _log('ping ip=${peer.ip} sample=${index + 1} failed=$error'),
    );
    if (median == null) {
      _log('ping ip=${peer.ip} unavailable; using HTTP latency');
    }
    return median;
  }

  Future<void> _openGateway(Uri upstreamBase) async {
    await _proxy?.close();
    final proxy = LoopbackGatewayProxy(
      client: Tailscale.instance.http.client,
      upstreamBase: upstreamBase,
    );
    final webViewUri = await proxy.start(preferredPort: _proxyPort);
    _proxyPort = webViewUri.port;
    await _appBridge.saveProxyPort(_proxyPort);
    late final WebViewController controller;
    controller = WebViewController()
      ..setJavaScriptMode(JavaScriptMode.unrestricted)
      ..setNavigationDelegate(
        NavigationDelegate(
          onNavigationRequest: (request) {
            final uri = Uri.parse(request.url);
            return uri.host == '127.0.0.1'
                ? NavigationDecision.navigate
                : NavigationDecision.prevent;
          },
          onWebResourceError: (error) =>
              _log('WebView error=${error.errorCode} ${error.description}'),
          onPageFinished: (_) =>
              unawaited(controller.runJavaScript(webViewTopInsetFixScript)),
          onUrlChange: (change) {
            final route = webRouteFromLoopbackUrl(change.url, _proxyPort);
            if (route == null || route == _lastWebRoute) return;
            _lastWebRoute = route;
            unawaited(_appBridge.saveLastWebRoute(route));
          },
        ),
      );
    final platformController = controller.platform;
    if (platformController is AndroidWebViewController) {
      await platformController.setOnShowFileSelector((params) {
        return _webViewResumeGuard.duringFileSelection(
          () => _appBridge.showFileChooser(
            acceptTypes: params.acceptTypes,
            multiple: params.mode == FileSelectorMode.openMultiple,
            capture: params.isCaptureEnabled,
          ),
        );
      });
    }
    // AndroidWebViewController enables DOM storage at construction time. A
    // stable loopback origin therefore preserves the web UI's localStorage.
    await controller.loadRequest(webViewUri.resolve(_lastWebRoute));
    _proxy = proxy;
    if (mounted) {
      setState(() {
        _currentGateway = upstreamBase;
        _webViewController = controller;
      });
    }
    _log('WebView load $webViewUri -> $upstreamBase');
  }

  Future<void> _replaceAuthKey(String authKey) async {
    await AuthKeyReconnectCoordinator(
      closeConnection: () async => _proxy?.close(),
      clearConnectionState: () async {
        if (!mounted) return;
        setState(() {
          _proxy = null;
          _webViewController = null;
          _currentGateway = null;
          _tailnetNodes = const [];
        });
      },
      connect: _run,
    ).run(authKey);
  }

  Future<void> _saveGatewayOverride(String? address) async {
    await _appBridge.saveGatewayOverride(address);
    if (mounted) {
      setState(() {
        _manualGatewayAddress = address;
        _manualGatewayChoiceMade = true;
      });
    }
    if (!_tailscaleInitialized || _connecting) return;
    setState(() => _connecting = true);
    try {
      final status = await Tailscale.instance.status();
      if (!status.isRunning) return;
      await _connectTailnet(status.tailscaleIPs.toSet());
    } finally {
      if (mounted) setState(() => _connecting = false);
    }
  }

  Future<void> _resetSettings() async {
    await ResetCoordinator(
      clearSettings: _appBridge.clearSettings,
      closeConnection: () async => _proxy?.close(),
      logout: Tailscale.instance.logout,
    ).run(tailscaleInitialized: _tailscaleInitialized);
    _authKeyController.clear();
    if (mounted) {
      setState(() {
        _savedAuthKey = null;
        _manualGatewayAddress = null;
        _manualGatewayChoiceMade = false;
        _currentGateway = null;
        _tailnetNodes = const [];
        _proxy = null;
        _webViewController = null;
        _proxyPort = defaultLoopbackProxyPort;
        _lastWebRoute = '/';
      });
    }
  }

  Future<void> _openSettings(BuildContext pageContext) async {
    await Navigator.of(pageContext).push<void>(
      MaterialPageRoute(
        builder: (_) => SettingsPage(
          authKey: _savedAuthKey ?? _authKeyController.text,
          nodes: _tailnetNodes,
          currentGateway: _currentGateway,
          gatewayOverride: _manualGatewayAddress,
          onReplaceAuthKey: _replaceAuthKey,
          onSaveGatewayOverride: _saveGatewayOverride,
          onReset: _resetSettings,
        ),
      ),
    );
  }

  @override
  Widget build(BuildContext context) {
    final controller = _webViewController;
    return MaterialApp(
      debugShowCheckedModeBanner: false,
      home: Builder(
        builder: (pageContext) => Scaffold(
          body: SafeArea(
            bottom: false,
            child: Column(
              children: [
                SizedBox(
                  key: const Key('native-toolbar'),
                  height: 38,
                  child: Material(
                    color: Theme.of(pageContext).colorScheme.surface,
                    child: Row(
                      children: [
                        const SizedBox(width: 12),
                        Expanded(
                          child: Text(
                            _currentGateway == null
                                ? 'Corral Console'
                                : 'Connected - ${_currentGateway!.host}',
                            style: const TextStyle(fontSize: 14),
                          ),
                        ),
                        IconButton(
                          key: const Key('open-settings'),
                          tooltip: 'Settings',
                          onPressed: () => _openSettings(pageContext),
                          padding: EdgeInsets.zero,
                          constraints: const BoxConstraints.tightFor(
                            width: 40,
                            height: 38,
                          ),
                          icon: const Icon(Icons.settings, size: 20),
                        ),
                      ],
                    ),
                  ),
                ),
                Expanded(
                  key: const Key('web-content-area'),
                  child: MediaQuery.removePadding(
                    context: pageContext,
                    removeTop: true,
                    child: _restoringSettings
                        ? const Center(child: CircularProgressIndicator())
                        : controller == null
                        ? _savedAuthKey != null || _connecting
                              ? const Center(
                                  child: Column(
                                    mainAxisSize: MainAxisSize.min,
                                    children: [
                                      CircularProgressIndicator(),
                                      SizedBox(height: 12),
                                      Text('Restoring the previous page...'),
                                    ],
                                  ),
                                )
                              : Column(
                                  children: [
                                    Padding(
                                      padding: const EdgeInsets.all(16),
                                      child: TextField(
                                        controller: _authKeyController,
                                        obscureText: true,
                                        enabled: !_connecting,
                                        decoration: const InputDecoration(
                                          border: OutlineInputBorder(),
                                          labelText: 'Tailscale authkey',
                                          hintText: 'tskey-auth-...',
                                        ),
                                        textInputAction: TextInputAction.done,
                                        onSubmitted: _run,
                                      ),
                                    ),
                                    FilledButton(
                                      onPressed: _connecting
                                          ? null
                                          : () => _run(_authKeyController.text),
                                      child: Text(
                                        _connecting
                                            ? 'Connecting automatically...'
                                            : 'Join tailnet and open console',
                                      ),
                                    ),
                                    const SizedBox(height: 8),
                                    Expanded(
                                      child: ListView.builder(
                                        padding: const EdgeInsets.all(12),
                                        itemCount: _lines.length,
                                        itemBuilder: (context, index) =>
                                            SelectableText(_lines[index]),
                                      ),
                                    ),
                                  ],
                                )
                        : WebViewWidget(controller: controller),
                  ),
                ),
              ],
            ),
          ),
        ),
      ),
    );
  }
}
