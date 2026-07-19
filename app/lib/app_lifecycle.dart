class StartupConnectionPlan {
  const StartupConnectionPlan({
    required this.authKey,
    required this.reusePersistedSession,
  });

  final String authKey;
  final bool reusePersistedSession;
}

StartupConnectionPlan? planStartupConnection({
  required String? storedAuthKey,
  required String buildTimeAuthKey,
}) {
  final stored = storedAuthKey?.trim() ?? '';
  if (stored.isNotEmpty) {
    return StartupConnectionPlan(authKey: stored, reusePersistedSession: true);
  }
  final buildTime = buildTimeAuthKey.trim();
  return buildTime.isEmpty
      ? null
      : StartupConnectionPlan(authKey: buildTime, reusePersistedSession: false);
}

typedef AsyncAction = Future<void> Function();
typedef ConnectWithAuthKey = Future<void> Function(String authKey);

class AuthKeyReconnectCoordinator {
  const AuthKeyReconnectCoordinator({
    required this.closeConnection,
    required this.clearConnectionState,
    required this.connect,
  });

  final AsyncAction closeConnection;
  final AsyncAction clearConnectionState;
  final ConnectWithAuthKey connect;

  Future<void> run(String authKey) async {
    await closeConnection();
    await clearConnectionState();
    await connect(authKey);
  }
}

class ResetCoordinator {
  const ResetCoordinator({
    required this.clearSettings,
    required this.closeConnection,
    required this.logout,
  });

  final AsyncAction clearSettings;
  final AsyncAction closeConnection;
  final AsyncAction logout;

  Future<void> run({required bool tailscaleInitialized}) async {
    await clearSettings();
    await closeConnection();
    if (tailscaleInitialized) await logout();
  }
}
