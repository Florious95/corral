import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

class TailnetNodeView {
  const TailnetNodeView({
    required this.name,
    required this.ip,
    required this.online,
    this.latency,
    this.gatewayHealthOk,
    this.gatewayHealthDetail = 'Not checked',
  });

  final String name;
  final String ip;
  final bool online;
  final Duration? latency;
  final bool? gatewayHealthOk;
  final String gatewayHealthDetail;
}

Uri normalizeGatewayAddress(String value) {
  final trimmed = value.trim();
  if (trimmed.isEmpty) {
    throw const FormatException('Gateway address cannot be empty');
  }
  final candidate = trimmed.contains('://') ? trimmed : 'http://$trimmed';
  final parsed = Uri.tryParse(candidate);
  if (parsed == null || parsed.scheme != 'http' || parsed.host.isEmpty) {
    throw const FormatException('Enter an IP, IP:port, or http:// address');
  }
  return Uri(
    scheme: 'http',
    host: parsed.host,
    port: parsed.hasPort ? parsed.port : 8787,
    path: '/',
  );
}

String? preferredGatewayOverride({
  required String? manual,
  required bool manualChoiceMade,
  String buildTimeFallback = '',
}) {
  final normalizedManual = manual?.trim() ?? '';
  if (manualChoiceMade) {
    return normalizedManual.isEmpty ? null : normalizedManual;
  }
  if (normalizedManual.isNotEmpty) return normalizedManual;
  final normalizedFallback = buildTimeFallback.trim();
  return normalizedFallback.isEmpty ? null : normalizedFallback;
}

class SettingsPage extends StatefulWidget {
  const SettingsPage({
    super.key,
    required this.authKey,
    required this.nodes,
    required this.currentGateway,
    required this.gatewayOverride,
    required this.onReplaceAuthKey,
    required this.onSaveGatewayOverride,
    required this.onReset,
  });

  final String authKey;
  final List<TailnetNodeView> nodes;
  final Uri? currentGateway;
  final String? gatewayOverride;
  final Future<void> Function(String authKey) onReplaceAuthKey;
  final Future<void> Function(String? gatewayAddress) onSaveGatewayOverride;
  final Future<void> Function() onReset;

  @override
  State<SettingsPage> createState() => _SettingsPageState();
}

class _SettingsPageState extends State<SettingsPage> {
  late final TextEditingController _authKeyController;
  late final TextEditingController _gatewayController;
  var _showAuthKey = false;
  var _saving = false;

  @override
  void initState() {
    super.initState();
    _authKeyController = TextEditingController(text: widget.authKey);
    _gatewayController = TextEditingController(text: widget.gatewayOverride);
  }

  @override
  void dispose() {
    _authKeyController.dispose();
    _gatewayController.dispose();
    super.dispose();
  }

  Future<void> _replaceAuthKey() async {
    final value = _authKeyController.text.trim();
    if (!value.startsWith('tskey-auth-') || value.length < 20) {
      _showMessage('Invalid auth key format');
      return;
    }
    setState(() => _saving = true);
    try {
      await widget.onReplaceAuthKey(value);
      if (mounted) Navigator.pop(context);
    } finally {
      if (mounted) setState(() => _saving = false);
    }
  }

  Future<void> _saveGatewayOverride() async {
    final value = _gatewayController.text.trim();
    if (value.isNotEmpty) {
      try {
        normalizeGatewayAddress(value);
      } on FormatException catch (error) {
        _showMessage(error.message);
        return;
      }
    }
    setState(() => _saving = true);
    try {
      await widget.onSaveGatewayOverride(value.isEmpty ? null : value);
      if (mounted) Navigator.pop(context);
    } finally {
      if (mounted) setState(() => _saving = false);
    }
  }

  void _showMessage(String message) {
    ScaffoldMessenger.of(
      context,
    ).showSnackBar(SnackBar(content: Text(message)));
  }

  Future<void> _copyIp(String ip) async {
    await Clipboard.setData(ClipboardData(text: ip));
    if (mounted) _showMessage('Copied $ip');
  }

  Future<void> _reset() async {
    final confirmed = await showDialog<bool>(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text('Clear key and reset?'),
        content: const Text(
          'This disconnects embedded Tailscale and clears the gateway override.',
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.pop(context, false),
            child: const Text('Cancel'),
          ),
          FilledButton(
            onPressed: () => Navigator.pop(context, true),
            child: const Text('Clear and disconnect'),
          ),
        ],
      ),
    );
    if (confirmed != true || !mounted) return;
    setState(() => _saving = true);
    try {
      await widget.onReset();
      if (mounted) Navigator.pop(context);
    } finally {
      if (mounted) setState(() => _saving = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final currentGateway = widget.currentGateway;
    return Scaffold(
      appBar: AppBar(title: const Text('Settings')),
      body: ListView(
        padding: const EdgeInsets.all(16),
        children: [
          Text('Identity', style: Theme.of(context).textTheme.titleSmall),
          const SizedBox(height: 8),
          Card(
            child: Padding(
              padding: const EdgeInsets.all(16),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.stretch,
                children: [
                  const ListTile(
                    contentPadding: EdgeInsets.zero,
                    leading: Icon(Icons.key),
                    title: Text('Tailscale auth key'),
                  ),
                  TextField(
                    key: const Key('settings-authkey'),
                    controller: _authKeyController,
                    obscureText: !_showAuthKey,
                    enabled: !_saving,
                    decoration: InputDecoration(
                      border: const OutlineInputBorder(),
                      suffixIcon: IconButton(
                        tooltip: _showAuthKey ? 'Hide' : 'Show',
                        onPressed: () =>
                            setState(() => _showAuthKey = !_showAuthKey),
                        icon: Icon(
                          _showAuthKey
                              ? Icons.visibility_off
                              : Icons.visibility,
                        ),
                      ),
                    ),
                  ),
                  const SizedBox(height: 12),
                  FilledButton(
                    key: const Key('replace-authkey'),
                    onPressed: _saving ? null : _replaceAuthKey,
                    child: const Text('Save and reconnect'),
                  ),
                ],
              ),
            ),
          ),
          const SizedBox(height: 20),
          Text('TAILNET NODES', style: Theme.of(context).textTheme.titleSmall),
          const SizedBox(height: 8),
          if (widget.nodes.isEmpty)
            const Card(
              child: ListTile(
                leading: Icon(Icons.cloud_off),
                title: Text('No nodes available'),
              ),
            )
          else
            ...widget.nodes.map((node) {
              final isGateway = currentGateway?.host == node.ip;
              final latency = node.latency;
              final status = isGateway
                  ? 'Current gateway'
                  : node.online
                  ? 'Online'
                  : 'Offline';
              final health = node.gatewayHealthOk == true
                  ? 'health: ok'
                  : node.gatewayHealthOk == false
                  ? 'health: failed (${node.gatewayHealthDetail})'
                  : 'health: ${node.gatewayHealthDetail}';
              return Card(
                child: ListTile(
                  leading: Icon(
                    Icons.circle,
                    size: 14,
                    color: node.online ? Colors.green : Colors.grey,
                  ),
                  title: SelectableText(node.name),
                  subtitle: Text(
                    latency == null
                        ? '$status · $health'
                        : '$status · $health · ${latency.inMilliseconds} ms',
                  ),
                  trailing: Row(
                    mainAxisSize: MainAxisSize.min,
                    children: [
                      SelectableText(node.ip),
                      IconButton(
                        tooltip: 'Copy ${node.ip}',
                        onPressed: () => _copyIp(node.ip),
                        icon: const Icon(Icons.copy),
                      ),
                    ],
                  ),
                ),
              );
            }),
          const SizedBox(height: 20),
          Text(
            'Advanced connection',
            style: Theme.of(context).textTheme.titleSmall,
          ),
          const SizedBox(height: 8),
          Card(
            child: Padding(
              padding: const EdgeInsets.all(16),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.stretch,
                children: [
                  const ListTile(
                    contentPadding: EdgeInsets.zero,
                    leading: Icon(Icons.hub),
                    title: Text('Manual gateway override'),
                  ),
                  TextField(
                    key: const Key('gateway-override'),
                    controller: _gatewayController,
                    enabled: !_saving,
                    onChanged: (_) => setState(() {}),
                    decoration: const InputDecoration(
                      border: OutlineInputBorder(),
                      hintText: 'For example, 192.0.2.17:8787',
                    ),
                  ),
                  const SizedBox(height: 8),
                  Text(
                    currentGateway == null
                        ? 'No gateway connected'
                        : 'Connected to ${currentGateway.host}:${currentGateway.port}',
                  ),
                  const SizedBox(height: 12),
                  FilledButton(
                    key: const Key('save-gateway-override'),
                    onPressed: _saving ? null : _saveGatewayOverride,
                    child: Text(
                      _gatewayController.text.trim().isEmpty
                          ? 'Restore automatic discovery'
                          : 'Save and connect',
                    ),
                  ),
                ],
              ),
            ),
          ),
          const SizedBox(height: 20),
          OutlinedButton.icon(
            key: const Key('reset-settings'),
            onPressed: _saving ? null : _reset,
            icon: const Icon(Icons.logout),
            label: const Text('Clear key and reset'),
          ),
        ],
      ),
    );
  }
}
