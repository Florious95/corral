import 'dart:async';
import 'dart:io';

import 'package:http/http.dart' as http;

const defaultLoopbackProxyPort = 17878;

class LoopbackGatewayProxy {
  LoopbackGatewayProxy({required this.client, required this.upstreamBase});

  final http.Client client;
  final Uri upstreamBase;
  HttpServer? _server;
  var _activeStreamCount = 0;

  bool get hasActiveStream => _activeStreamCount > 0;

  Future<Uri> start({int preferredPort = defaultLoopbackProxyPort}) async {
    final firstPort = preferredPort >= 1024 && preferredPort <= 65515
        ? preferredPort
        : defaultLoopbackProxyPort;
    for (var port = firstPort; port < firstPort + 20; port++) {
      try {
        final server = await HttpServer.bind(
          InternetAddress.loopbackIPv4,
          port,
        );
        _server = server;
        unawaited(server.forEach(_forward));
        return Uri.parse('http://127.0.0.1:${server.port}/');
      } on SocketException {
        if (port == firstPort + 19) rethrow;
      }
    }
    throw StateError('no loopback proxy port available');
  }

  Future<void> close() async {
    await _server?.close(force: true);
  }

  Future<void> _forward(HttpRequest incoming) async {
    var tracksStream = false;
    try {
      final target = upstreamBase.resolveUri(incoming.uri);
      final outgoing = http.StreamedRequest(incoming.method, target);
      incoming.headers.forEach((name, values) {
        if (!_hopByHopHeaders.contains(name.toLowerCase())) {
          outgoing.headers[name] = values.join(',');
        }
      });
      final upstreamFuture = client.send(outgoing);
      await outgoing.sink.addStream(incoming);
      await outgoing.sink.close();
      final upstream = await upstreamFuture;
      incoming.response.statusCode = upstream.statusCode;
      upstream.headers.forEach((name, value) {
        if (!_hopByHopHeaders.contains(name.toLowerCase())) {
          incoming.response.headers.set(name, value);
        }
      });
      final isEventStream =
          upstream.headers['content-type']?.toLowerCase().startsWith(
            'text/event-stream',
          ) ??
          false;
      final shouldFlushChunks = isEventStream || upstream.contentLength == null;
      tracksStream = isEventStream || incoming.uri.path.endsWith('/stream');
      if (tracksStream) _activeStreamCount++;
      if (shouldFlushChunks) {
        incoming.response.bufferOutput = false;
        await for (final chunk in upstream.stream) {
          incoming.response.add(chunk);
          await incoming.response.flush();
        }
        await incoming.response.close();
      } else {
        await upstream.stream.pipe(incoming.response);
      }
    } catch (error) {
      incoming.response.statusCode = HttpStatus.badGateway;
      incoming.response.write('gateway proxy failed: $error');
      await incoming.response.close();
    } finally {
      if (tracksStream) _activeStreamCount--;
    }
  }
}

const _hopByHopHeaders = {
  'connection',
  'content-length',
  'host',
  'keep-alive',
  'proxy-authenticate',
  'proxy-authorization',
  'te',
  'trailer',
  'transfer-encoding',
  'upgrade',
};
