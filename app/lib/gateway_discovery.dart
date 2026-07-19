import 'dart:async';
import 'dart:convert';

import 'package:http/http.dart' as http;

class GatewayPeer {
  const GatewayPeer({
    required this.stableNodeId,
    required this.ip,
    required this.online,
  });

  final String stableNodeId;
  final String ip;
  final bool online;
}

class GatewaySelection {
  const GatewaySelection({
    required this.peer,
    required this.healthLatency,
    required this.rtt,
  });

  final GatewayPeer peer;
  final Duration healthLatency;
  final Duration? rtt;

  Duration get score => rtt ?? healthLatency;
}

typedef HealthProbe = Future<Duration?> Function(GatewayPeer peer);
typedef RttProbe = Future<Duration?> Function(GatewayPeer peer);
typedef LatencySample = Future<Duration> Function(int sampleIndex);
typedef SampleError = void Function(int sampleIndex, Object error);

class GatewayHealthResult {
  const GatewayHealthResult._({this.latency, this.failureReason});

  const GatewayHealthResult.ok(Duration latency) : this._(latency: latency);

  const GatewayHealthResult.failed(String reason)
    : this._(failureReason: reason);

  final Duration? latency;
  final String? failureReason;

  bool get isOk => latency != null;
}

Future<Duration?> sampleMedianLatency({
  required LatencySample sample,
  int sampleCount = 3,
  SampleError? onError,
}) async {
  final results = await Future.wait(
    List.generate(sampleCount, (index) async {
      try {
        return await sample(index);
      } catch (error) {
        onError?.call(index, error);
        return null;
      }
    }),
  );
  return medianDuration(results.whereType<Duration>());
}

Duration? medianDuration(Iterable<Duration> values) {
  final sorted = values.toList()..sort();
  if (sorted.isEmpty) return null;
  final middle = sorted.length ~/ 2;
  if (sorted.length.isOdd) return sorted[middle];

  // Conventional even-sample median: arithmetic mean of the two middle
  // values, rounded down to the nearest microsecond.
  final lower = sorted[middle - 1].inMicroseconds;
  final upper = sorted[middle].inMicroseconds;
  return Duration(microseconds: lower + ((upper - lower) ~/ 2));
}

class GatewayDiscovery {
  const GatewayDiscovery({required this.healthProbe, required this.rttProbe});

  final HealthProbe healthProbe;
  final RttProbe rttProbe;

  Future<GatewaySelection?> select(Iterable<GatewayPeer> peers) async {
    final measured = await Future.wait(
      peers.where((peer) => peer.online && peer.ip.isNotEmpty).map(_measure),
    );
    final candidates = measured.whereType<GatewaySelection>().toList()
      ..sort((a, b) {
        final byRtt = a.score.compareTo(b.score);
        return byRtt != 0
            ? byRtt
            : a.peer.stableNodeId.compareTo(b.peer.stableNodeId);
      });
    return candidates.firstOrNull;
  }

  Future<GatewaySelection?> _measure(GatewayPeer peer) async {
    final healthLatency = await healthProbe(peer);
    if (healthLatency == null) return null;
    return GatewaySelection(
      peer: peer,
      healthLatency: healthLatency,
      rtt: await rttProbe(peer),
    );
  }
}

const noGatewayFoundMessage =
    'No gateway found: no node returned 2xx JSON status=ok from :8787/api/health. You can enter a gateway manually in Settings.';

GatewaySelection requireGatewaySelection(GatewaySelection? selection) {
  if (selection == null) throw StateError(noGatewayFoundMessage);
  return selection;
}

class HttpHealthProbe {
  const HttpHealthProbe({required this.client, this.port = 8787});

  final http.Client client;
  final int port;

  Future<Duration?> call(GatewayPeer peer) async {
    return (await diagnose(peer)).latency;
  }

  Future<GatewayHealthResult> diagnose(GatewayPeer peer) async {
    final stopwatch = Stopwatch()..start();
    try {
      final response = await client
          .get(Uri.parse('http://${peer.ip}:$port/api/health'))
          .timeout(const Duration(seconds: 8));
      if (response.statusCode < 200 || response.statusCode >= 300) {
        return GatewayHealthResult.failed('HTTP ${response.statusCode}');
      }
      Object? body;
      try {
        body = jsonDecode(response.body);
      } on FormatException {
        return const GatewayHealthResult.failed('Response is not JSON');
      }
      if (body is! Map<String, dynamic> || body['status'] != 'ok') {
        return GatewayHealthResult.failed(
          body is Map<String, dynamic> && body.containsKey('status')
              ? 'status=${body['status']}'
              : 'Missing status=ok',
        );
      }
      return GatewayHealthResult.ok(stopwatch.elapsed);
    } on TimeoutException {
      return const GatewayHealthResult.failed('Request timed out');
    } catch (error) {
      return GatewayHealthResult.failed(
        'Request failed (${error.runtimeType})',
      );
    }
  }
}
