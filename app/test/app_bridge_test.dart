import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:corral/app_bridge.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  const channel = MethodChannel('com.florious95.corral.console/app');

  tearDown(() async {
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(channel, null);
  });

  test('loads encrypted key metadata and persisted gateway choice', () async {
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(channel, (call) async {
          expect(call.method, 'loadSettings');
          return <String, Object?>{
            'authKey': 'tskey-auth-valid-secret',
            'gatewayOverride': null,
            'gatewayChoiceMade': true,
            'proxyPort': 17881,
            'lastWebRoute': '/entries/e1_test?tab=chat',
            'stateDir': '/persistent/corral',
          };
        });

    final stored = await const AppBridge().loadSettings();
    expect(stored.authKey, 'tskey-auth-valid-secret');
    expect(stored.gatewayOverride, isNull);
    expect(stored.gatewayChoiceMade, isTrue);
    expect(stored.proxyPort, 17881);
    expect(stored.lastWebRoute, '/entries/e1_test?tab=chat');
    expect(stored.stateDir, '/persistent/corral');
  });

  test('persists the stable proxy port and current web route', () async {
    final calls = <MethodCall>[];
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(channel, (call) async {
          calls.add(call);
          return null;
        });

    await const AppBridge().saveProxyPort(17879);
    await const AppBridge().saveLastWebRoute('/entries/e1_test#messages');

    expect(calls.map((call) => call.method), [
      'saveProxyPort',
      'saveLastWebRoute',
    ]);
    expect(calls.first.arguments, {'port': 17879});
    expect(calls.last.arguments, {'route': '/entries/e1_test#messages'});
  });

  test('persists explicit automatic gateway choice', () async {
    MethodCall? received;
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(channel, (call) async {
          received = call;
          return null;
        });

    await const AppBridge().saveGatewayOverride(null);
    expect(received?.method, 'saveGatewayOverride');
    expect((received?.arguments as Map<Object?, Object?>)['address'], isNull);
  });

  test(
    'passes web file chooser capabilities to Android and returns URIs',
    () async {
      MethodCall? received;
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(channel, (call) async {
            received = call;
            return <String>['content://app.fileprovider/camera/capture.jpg'];
          });

      final uris = await const AppBridge().showFileChooser(
        acceptTypes: const ['image/*'],
        multiple: true,
        capture: true,
      );
      expect(received?.method, 'showFileChooser');
      expect(received?.arguments, {
        'acceptTypes': ['image/*'],
        'multiple': true,
        'capture': true,
      });
      expect(uris, ['content://app.fileprovider/camera/capture.jpg']);
    },
  );
}
