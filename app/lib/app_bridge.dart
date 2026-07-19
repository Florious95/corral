import 'dart:io';

import 'package:flutter/services.dart';

class StoredAppSettings {
  const StoredAppSettings({
    this.authKey,
    this.gatewayOverride,
    this.gatewayChoiceMade = false,
    this.proxyPort = 17878,
    this.lastWebRoute = '/',
    required this.stateDir,
  });

  final String? authKey;
  final String? gatewayOverride;
  final bool gatewayChoiceMade;
  final int proxyPort;
  final String lastWebRoute;
  final String stateDir;
}

class AppBridge {
  const AppBridge();

  static const _channel = MethodChannel('com.florious95.corral.console/app');

  Future<StoredAppSettings> loadSettings() async {
    try {
      final raw = await _channel.invokeMapMethod<String, Object?>(
        'loadSettings',
      );
      return StoredAppSettings(
        authKey: raw?['authKey'] as String?,
        gatewayOverride: raw?['gatewayOverride'] as String?,
        gatewayChoiceMade: raw?['gatewayChoiceMade'] as bool? ?? false,
        proxyPort: raw?['proxyPort'] as int? ?? 17878,
        lastWebRoute: raw?['lastWebRoute'] as String? ?? '/',
        stateDir:
            raw?['stateDir'] as String? ??
            '${Directory.systemTemp.path}/corral',
      );
    } on MissingPluginException {
      return StoredAppSettings(stateDir: '${Directory.systemTemp.path}/corral');
    }
  }

  Future<void> saveAuthKey(String authKey) async {
    try {
      await _channel.invokeMethod<void>('saveAuthKey', {'authKey': authKey});
    } on MissingPluginException {
      // Widget and desktop tests do not install the Android bridge.
    }
  }

  Future<void> saveGatewayOverride(String? address) async {
    try {
      await _channel.invokeMethod<void>('saveGatewayOverride', {
        'address': address,
      });
    } on MissingPluginException {
      // Widget and desktop tests do not install the Android bridge.
    }
  }

  Future<void> clearSettings() async {
    try {
      await _channel.invokeMethod<void>('clearSettings');
    } on MissingPluginException {
      // Widget and desktop tests do not install the Android bridge.
    }
  }

  Future<void> saveProxyPort(int port) async {
    try {
      await _channel.invokeMethod<void>('saveProxyPort', {'port': port});
    } on MissingPluginException {
      // Widget and desktop tests do not install the Android bridge.
    }
  }

  Future<void> saveLastWebRoute(String route) async {
    try {
      await _channel.invokeMethod<void>('saveLastWebRoute', {'route': route});
    } on MissingPluginException {
      // Widget and desktop tests do not install the Android bridge.
    }
  }

  Future<List<String>> showFileChooser({
    required List<String> acceptTypes,
    required bool multiple,
    required bool capture,
  }) async {
    try {
      return await _channel.invokeListMethod<String>('showFileChooser', {
            'acceptTypes': acceptTypes,
            'multiple': multiple,
            'capture': capture,
          }) ??
          const [];
    } on MissingPluginException {
      return const [];
    }
  }
}
