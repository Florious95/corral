import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:corral/app_bridge.dart';
import 'package:corral/main.dart';

class _TestBridge extends AppBridge {
  const _TestBridge();

  @override
  Future<StoredAppSettings> loadSettings() async {
    return const StoredAppSettings(stateDir: '/tmp/corral-test');
  }
}

void main() {
  test('normalizes and validates authkeys without logging their value', () {
    expect(
      normalizeAuthKey('  tskey-auth-valid-secret  '),
      'tskey-auth-valid-secret',
    );
    expect(() => normalizeAuthKey('not-an-authkey'), throwsFormatException);
  });

  test('restores only routes from the stable loopback origin', () {
    expect(
      normalizeStoredWebRoute('/entries/e1_test?tab=chat#latest'),
      '/entries/e1_test?tab=chat#latest',
    );
    expect(normalizeStoredWebRoute('https://attacker.invalid/path'), '/');
    expect(
      webRouteFromLoopbackUrl(
        'http://127.0.0.1:17878/entries/e1_test?tab=chat#latest',
        17878,
      ),
      '/entries/e1_test?tab=chat#latest',
    );
    expect(
      webRouteFromLoopbackUrl('http://127.0.0.1:17879/entries/e1_test', 17878),
      isNull,
    );
  });

  test('removes only the duplicated WebView top safe-area padding', () {
    expect(webViewTopInsetFixScript, contains('.wa-header'));
    expect(webViewTopInsetFixScript, contains('padding-top: 10px !important'));
    expect(webViewTopInsetFixScript, isNot(contains('padding-bottom')));
  });

  testWidgets('renders the probe shell', (tester) async {
    await tester.pumpWidget(const ProbeApp(appBridge: _TestBridge()));
    await tester.pumpAndSettle();

    expect(find.text('Corral Console'), findsOneWidget);
    expect(find.byType(Banner), findsNothing);
    expect(tester.getSize(find.byKey(const Key('native-toolbar'))).height, 38);
    expect(
      tester.getBottomLeft(find.byKey(const Key('native-toolbar'))).dy,
      tester.getTopLeft(find.byKey(const Key('web-content-area'))).dy,
    );
    expect(find.text('Tailscale authkey'), findsOneWidget);
    expect(find.text('Join tailnet and open console'), findsOneWidget);
    expect(find.byKey(const Key('open-settings')), findsOneWidget);

    await tester.tap(find.byKey(const Key('open-settings')));
    await tester.pumpAndSettle();
    expect(find.text('Settings'), findsOneWidget);
    expect(find.text('No nodes available'), findsOneWidget);
  });
}
