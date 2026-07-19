import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:corral/app_lifecycle.dart';
import 'package:corral/settings_page.dart';

void main() {
  Future<void> pumpSettings(
    WidgetTester tester, {
    List<TailnetNodeView> nodes = const [],
    String? gatewayOverride,
    Future<void> Function(String)? onReplaceAuthKey,
    Future<void> Function(String?)? onSaveGatewayOverride,
  }) {
    return tester.pumpWidget(
      MaterialApp(
        home: SettingsPage(
          authKey: 'tskey-auth-valid-secret',
          nodes: nodes,
          currentGateway: null,
          gatewayOverride: gatewayOverride,
          onReplaceAuthKey: onReplaceAuthKey ?? (_) async {},
          onSaveGatewayOverride: onSaveGatewayOverride ?? (_) async {},
          onReset: () async {},
        ),
      ),
    );
  }

  test('normalizes gateway addresses and gives manual override priority', () {
    expect(
      normalizeGatewayAddress('192.0.2.8').toString(),
      'http://192.0.2.8:8787/',
    );
    expect(
      normalizeGatewayAddress('http://192.0.2.8:9000/path').toString(),
      'http://192.0.2.8:9000/',
    );
    expect(
      preferredGatewayOverride(
        manual: '192.0.2.9:8787',
        manualChoiceMade: true,
        buildTimeFallback: '192.0.2.8',
      ),
      '192.0.2.9:8787',
    );
    expect(
      preferredGatewayOverride(
        manual: ' ',
        manualChoiceMade: false,
        buildTimeFallback: '192.0.2.8',
      ),
      '192.0.2.8',
    );
    expect(
      preferredGatewayOverride(
        manual: null,
        manualChoiceMade: true,
        buildTimeFallback: '192.0.2.8',
      ),
      isNull,
    );
    expect(
      preferredGatewayOverride(manual: null, manualChoiceMade: false),
      isNull,
    );
  });

  testWidgets('shows protected authkey, nodes, gateway, and saves override', (
    tester,
  ) async {
    String? savedOverride;
    await tester.pumpWidget(
      MaterialApp(
        home: SettingsPage(
          authKey: 'tskey-auth-valid-secret',
          nodes: const [
            TailnetNodeView(
              name: 'test-node-1',
              ip: '192.0.2.78',
              online: true,
              latency: Duration(milliseconds: 18),
              gatewayHealthOk: true,
            ),
            TailnetNodeView(
              name: 'fast-without-gateway',
              ip: '192.0.2.79',
              online: true,
              gatewayHealthOk: false,
              gatewayHealthDetail: 'Request timed out',
            ),
          ],
          currentGateway: Uri.parse('http://192.0.2.78:8787/'),
          gatewayOverride: null,
          onReplaceAuthKey: (_) async {},
          onSaveGatewayOverride: (value) async => savedOverride = value,
          onReset: () async {},
        ),
      ),
    );

    final authField = tester.widget<TextField>(
      find.byKey(const Key('settings-authkey')),
    );
    expect(authField.obscureText, isTrue);
    expect(find.text('test-node-1'), findsOneWidget);
    expect(find.text('192.0.2.78'), findsOneWidget);
    expect(find.text('Current gateway · health: ok · 18 ms'), findsOneWidget);
    expect(
      find.text('Online · health: failed (Request timed out)'),
      findsOneWidget,
    );

    await tester.enterText(
      find.byKey(const Key('gateway-override')),
      '192.0.2.78:9000',
    );
    final saveButton = find.byKey(const Key('save-gateway-override'));
    await tester.drag(find.byType(ListView), const Offset(0, -400));
    await tester.pumpAndSettle();
    await tester.tap(saveButton);
    await tester.pumpAndSettle();
    expect(savedOverride, '192.0.2.78:9000');
  });

  testWidgets('toggles authkey visibility without changing its value', (
    tester,
  ) async {
    await pumpSettings(tester);

    TextField field() =>
        tester.widget(find.byKey(const Key('settings-authkey')));
    expect(field().obscureText, isTrue);
    expect(field().controller?.text, 'tskey-auth-valid-secret');

    await tester.tap(find.byTooltip('Show'));
    await tester.pump();
    expect(field().obscureText, isFalse);

    await tester.tap(find.byTooltip('Hide'));
    await tester.pump();
    expect(field().obscureText, isTrue);
    expect(field().controller?.text, 'tskey-auth-valid-secret');
  });

  testWidgets('replacing authkey invokes the reconnect callback', (
    tester,
  ) async {
    final calls = <String>[];
    final reconnect = AuthKeyReconnectCoordinator(
      closeConnection: () async => calls.add('close'),
      clearConnectionState: () async => calls.add('clear-view'),
      connect: (key) async => calls.add('up:$key'),
    );
    await pumpSettings(tester, onReplaceAuthKey: reconnect.run);

    await tester.enterText(
      find.byKey(const Key('settings-authkey')),
      'tskey-auth-replacement-secret',
    );
    await tester.tap(find.byKey(const Key('replace-authkey')));
    await tester.pumpAndSettle();

    expect(calls, ['close', 'clear-view', 'up:tskey-auth-replacement-secret']);
  });

  testWidgets('copies a node IP through Clipboard', (tester) async {
    MethodCall? clipboardCall;
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(SystemChannels.platform, (call) async {
          if (call.method == 'Clipboard.setData') clipboardCall = call;
          return null;
        });
    addTearDown(
      () => TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(SystemChannels.platform, null),
    );
    await pumpSettings(
      tester,
      nodes: const [
        TailnetNodeView(
          name: 'gateway',
          ip: '192.0.2.78',
          online: true,
          gatewayHealthOk: true,
        ),
      ],
    );

    await tester.tap(find.byTooltip('Copy 192.0.2.78'));
    await tester.pump();

    expect(clipboardCall?.method, 'Clipboard.setData');
    expect(clipboardCall?.arguments, {'text': '192.0.2.78'});
    expect(find.text('Copied 192.0.2.78'), findsOneWidget);
  });

  testWidgets('shows an offline node and its unprobed health state', (
    tester,
  ) async {
    await pumpSettings(
      tester,
      nodes: const [
        TailnetNodeView(
          name: 'offline-mac',
          ip: '192.0.2.90',
          online: false,
          gatewayHealthDetail: 'Offline, not probed',
        ),
      ],
    );

    expect(find.text('offline-mac'), findsOneWidget);
    expect(find.text('Offline · health: Offline, not probed'), findsOneWidget);
  });

  testWidgets('clearing a manual gateway submits explicit automatic mode', (
    tester,
  ) async {
    String? saved = 'not-called';
    await pumpSettings(
      tester,
      gatewayOverride: '192.0.2.78:8787',
      onSaveGatewayOverride: (value) async => saved = value,
    );

    await tester.enterText(find.byKey(const Key('gateway-override')), '');
    await tester.drag(find.byType(ListView), const Offset(0, -500));
    await tester.pumpAndSettle();
    expect(find.text('Restore automatic discovery'), findsOneWidget);
    await tester.tap(find.byKey(const Key('save-gateway-override')));
    await tester.pumpAndSettle();

    expect(saved, isNull);
  });

  test('explicit automatic mode ignores build-time GATEWAY_IP', () {
    expect(
      preferredGatewayOverride(
        manual: null,
        manualChoiceMade: true,
        buildTimeFallback: '192.0.2.78',
      ),
      isNull,
    );
  });

  test('first-run mode uses build-time GATEWAY_IP when present', () {
    expect(
      preferredGatewayOverride(
        manual: null,
        manualChoiceMade: false,
        buildTimeFallback: '192.0.2.78',
      ),
      '192.0.2.78',
    );
    expect(
      preferredGatewayOverride(
        manual: null,
        manualChoiceMade: false,
        buildTimeFallback: '',
      ),
      isNull,
    );
  });
}
