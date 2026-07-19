import 'package:flutter_test/flutter_test.dart';
import 'package:corral/app_lifecycle.dart';

void main() {
  test('cold start restores the stored key and reuses the session', () {
    final plan = planStartupConnection(
      storedAuthKey: ' tskey-auth-stored-secret ',
      buildTimeAuthKey: 'tskey-auth-build-time',
    );

    expect(plan?.authKey, 'tskey-auth-stored-secret');
    expect(plan?.reusePersistedSession, isTrue);
  });

  test('cold start uses build-time key only without a stored key', () {
    final plan = planStartupConnection(
      storedAuthKey: null,
      buildTimeAuthKey: ' tskey-auth-build-time ',
    );

    expect(plan?.authKey, 'tskey-auth-build-time');
    expect(plan?.reusePersistedSession, isFalse);
    expect(
      planStartupConnection(storedAuthKey: null, buildTimeAuthKey: ''),
      isNull,
    );
  });

  test('reset clears settings, closes the connection, then logs out', () async {
    final calls = <String>[];
    final coordinator = ResetCoordinator(
      clearSettings: () async => calls.add('clear'),
      closeConnection: () async => calls.add('close'),
      logout: () async => calls.add('logout'),
    );

    await coordinator.run(tailscaleInitialized: true);

    expect(calls, ['clear', 'close', 'logout']);
  });

  test('replacing authkey closes, clears, then reconnects with it', () async {
    final calls = <String>[];
    final coordinator = AuthKeyReconnectCoordinator(
      closeConnection: () async => calls.add('close'),
      clearConnectionState: () async => calls.add('clear-view'),
      connect: (authKey) async => calls.add('connect:$authKey'),
    );

    await coordinator.run('tskey-auth-replacement-secret');

    expect(calls, [
      'close',
      'clear-view',
      'connect:tskey-auth-replacement-secret',
    ]);
  });

  test('reset skips logout before Tailscale initialization', () async {
    final calls = <String>[];
    final coordinator = ResetCoordinator(
      clearSettings: () async => calls.add('clear'),
      closeConnection: () async => calls.add('close'),
      logout: () async => calls.add('logout'),
    );

    await coordinator.run(tailscaleInitialized: false);

    expect(calls, ['clear', 'close']);
  });
}
