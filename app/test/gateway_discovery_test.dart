import 'dart:async';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:corral/gateway_discovery.dart';

void main() {
  test('health gate beats lower RTT from an unhealthy peer', () async {
    const peers = [
      GatewayPeer(stableNodeId: 'node-a', ip: '192.0.2.1', online: true),
      GatewayPeer(stableNodeId: 'node-b', ip: '192.0.2.2', online: true),
      GatewayPeer(stableNodeId: 'offline', ip: '192.0.2.3', online: false),
      GatewayPeer(stableNodeId: 'unhealthy', ip: '192.0.2.4', online: true),
    ];
    final discovery = GatewayDiscovery(
      healthProbe: (peer) async => peer.stableNodeId == 'unhealthy'
          ? null
          : const Duration(milliseconds: 10),
      rttProbe: (peer) async => switch (peer.stableNodeId) {
        'node-a' => const Duration(milliseconds: 25),
        'node-b' => const Duration(milliseconds: 8),
        _ => const Duration(milliseconds: 1),
      },
    );

    final selected = await discovery.select(peers);

    expect(selected?.peer.stableNodeId, 'node-b');
    expect(selected?.score, const Duration(milliseconds: 8));
  });

  test('returns no selection when every peer fails health', () async {
    const peers = [
      GatewayPeer(stableNodeId: 'fast', ip: '192.0.2.1', online: true),
      GatewayPeer(stableNodeId: 'faster', ip: '192.0.2.2', online: true),
    ];
    var rttCalls = 0;
    final discovery = GatewayDiscovery(
      healthProbe: (_) async => null,
      rttProbe: (peer) async {
        rttCalls++;
        return peer.stableNodeId == 'fast'
            ? const Duration(milliseconds: 2)
            : const Duration(milliseconds: 1);
      },
    );

    final selected = await discovery.select(peers);
    expect(selected, isNull);
    expect(rttCalls, 0);
    expect(
      () => requireGatewaySelection(selected),
      throwsA(
        isA<StateError>().having(
          (error) => error.message,
          'message',
          noGatewayFoundMessage,
        ),
      ),
    );
  });

  test('falls back to health latency and breaks ties by stable id', () async {
    const peers = [
      GatewayPeer(stableNodeId: 'node-z', ip: '192.0.2.2', online: true),
      GatewayPeer(stableNodeId: 'node-a', ip: '192.0.2.1', online: true),
    ];
    final discovery = GatewayDiscovery(
      healthProbe: (_) async => const Duration(milliseconds: 12),
      rttProbe: (_) async => null,
    );

    final selected = await discovery.select(peers);

    expect(selected?.peer.stableNodeId, 'node-a');
    expect(selected?.score, const Duration(milliseconds: 12));
  });

  group('sampleMedianLatency', () {
    final cases = <(String, List<int>, Duration?)>[
      ('3 successes', [30, 10, 20], const Duration(milliseconds: 20)),
      ('2 successes', [10, 20], const Duration(milliseconds: 15)),
      ('1 success', [10], const Duration(milliseconds: 10)),
      ('0 successes', [], null),
    ];

    for (final (name, successes, expected) in cases) {
      test(name, () async {
        final result = await sampleMedianLatency(
          sample: (index) async {
            if (index >= successes.length) throw StateError('ping failed');
            return Duration(milliseconds: successes[index]);
          },
        );

        expect(result, expected);
      });
    }

    test('starts all ping samples concurrently', () async {
      final allStarted = Completer<void>();
      var active = 0;
      var maxActive = 0;
      final result = sampleMedianLatency(
        sample: (index) async {
          active++;
          if (active > maxActive) maxActive = active;
          if (active == 3) allStarted.complete();
          await allStarted.future;
          active--;
          return Duration(milliseconds: index + 1);
        },
      );

      await allStarted.future.timeout(const Duration(seconds: 1));
      expect(maxActive, 3);
      expect(await result, const Duration(milliseconds: 2));
    });
  });

  group('HttpHealthProbe', () {
    const peer = GatewayPeer(
      stableNodeId: 'gateway',
      ip: '127.0.0.1',
      online: true,
    );
    final cases = <(String, int, String, bool)>[
      ('accepts 2xx JSON status ok', 200, '{"status":"ok"}', true),
      ('rejects non-2xx', 503, '{"status":"ok"}', false),
      ('rejects non-JSON', 200, '<html>not health</html>', false),
      ('rejects status other than ok', 200, '{"status":"starting"}', false),
    ];

    for (final (name, statusCode, body, accepted) in cases) {
      test(name, () async {
        final client = MockClient((_) async => http.Response(body, statusCode));

        final result = await HttpHealthProbe(client: client).call(peer);

        expect(result != null, accepted);
        client.close();
      });
    }

    test('reports the health failure reason for diagnostics', () async {
      final client = MockClient((_) async => http.Response('busy', 503));

      final result = await HttpHealthProbe(client: client).diagnose(peer);

      expect(result.isOk, isFalse);
      expect(result.failureReason, 'HTTP 503');
      client.close();
    });
  });
}
