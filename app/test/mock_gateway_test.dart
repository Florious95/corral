import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:corral/gateway_discovery.dart';
import 'package:corral/loopback_gateway_proxy.dart';

void main() {
  test(
    'discovers a mock gateway and serves it through the WebView URI',
    () async {
      final gateway = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      unawaited(
        gateway.forEach((request) async {
          if (request.uri.path == '/api/health') {
            request.response.headers.contentType = ContentType.json;
            request.response.write('{"status":"ok"}');
          } else {
            request.response.headers.contentType = ContentType.html;
            request.response.write('<html><body>mock gateway</body></html>');
          }
          await request.response.close();
        }),
      );
      final client = http.Client();
      final discovery = GatewayDiscovery(
        healthProbe: HttpHealthProbe(client: client, port: gateway.port).call,
        rttProbe: (_) async => const Duration(milliseconds: 3),
      );
      const peer = GatewayPeer(
        stableNodeId: 'mock-gateway',
        ip: '127.0.0.1',
        online: true,
      );
      final selected = await discovery.select([peer]);
      expect(selected?.peer, same(peer));

      final proxy = LoopbackGatewayProxy(
        client: client,
        upstreamBase: Uri.parse('http://127.0.0.1:${gateway.port}/'),
      );
      final webViewUri = await proxy.start();
      final response = await client.get(webViewUri);

      expect(webViewUri.host, '127.0.0.1');
      expect(response.statusCode, 200);
      expect(response.body, contains('mock gateway'));

      await proxy.close();
      await gateway.close(force: true);
      client.close();
    },
  );

  test(
    'keeps a preferred origin and advances when its port is occupied',
    () async {
      final occupied = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      final gateway = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      unawaited(
        gateway.forEach((request) async {
          request.response.write('ok');
          await request.response.close();
        }),
      );
      final client = http.Client();
      final proxy = LoopbackGatewayProxy(
        client: client,
        upstreamBase: Uri.parse('http://127.0.0.1:${gateway.port}/'),
      );

      final uri = await proxy.start(preferredPort: occupied.port);

      expect(uri.port, greaterThan(occupied.port));
      expect(await client.read(uri), 'ok');
      await proxy.close();
      await gateway.close(force: true);
      await occupied.close(force: true);
      client.close();
    },
  );

  test(
    'streams SSE before EOF and preserves query and request headers',
    () async {
      final releaseStream = Completer<void>();
      final requestSeen = Completer<void>();
      final gateway = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      unawaited(
        gateway.forEach((request) async {
          expect(request.uri.path, '/api/v2/stream');
          expect(request.uri.queryParameters['afterRevision'], 'r0');
          expect(request.headers.value('accept'), 'text/event-stream');
          expect(request.headers.value('cache-control'), 'no-cache');
          requestSeen.complete();
          request.response.headers.contentType = ContentType(
            'text',
            'event-stream',
          );
          request.response.bufferOutput = false;
          request.response.write(
            'event: snapshot\ndata: {"revision":"r1"}\n\n',
          );
          await request.response.flush();
          await releaseStream.future;
          await request.response.close();
        }),
      );
      final client = http.Client();
      final proxy = LoopbackGatewayProxy(
        client: client,
        upstreamBase: Uri.parse('http://127.0.0.1:${gateway.port}/'),
      );
      final webViewUri = await proxy.start();
      final request =
          http.Request(
              'GET',
              webViewUri.resolve('/api/v2/stream?afterRevision=r0'),
            )
            ..headers['accept'] = 'text/event-stream'
            ..headers['cache-control'] = 'no-cache';
      final response = await client.send(request);
      final firstChunk = await response.stream
          .transform(const Utf8Decoder())
          .first
          .timeout(const Duration(seconds: 2));

      expect(response.statusCode, 200);
      expect(firstChunk, contains('event: snapshot'));
      expect(proxy.hasActiveStream, isTrue);
      await requestSeen.future;

      releaseStream.complete();
      await proxy.close();
      await gateway.close(force: true);
      client.close();
    },
  );

  test(
    'flushes an unknown-length delivery stream without waiting for EOF',
    () async {
      final releaseStream = Completer<void>();
      final gateway = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      unawaited(
        gateway.forEach((request) async {
          expect(request.uri.queryParameters['afterSeq'], '7');
          expect(request.headers.value('last-event-id'), 'delivery-6');
          request.response.headers.contentType = ContentType.json;
          request.response.bufferOutput = false;
          request.response.write('{"status":"echoed"}\n');
          await request.response.flush();
          await releaseStream.future;
          await request.response.close();
        }),
      );
      final client = http.Client();
      final proxy = LoopbackGatewayProxy(
        client: client,
        upstreamBase: Uri.parse('http://127.0.0.1:${gateway.port}/'),
      );
      final uri = await proxy.start();
      final request = http.Request(
        'GET',
        uri.resolve('/api/v2/entries/e1_test/stream?afterSeq=7'),
      )..headers['last-event-id'] = 'delivery-6';
      final response = await client.send(request);
      final firstChunk = await response.stream
          .transform(const Utf8Decoder())
          .first
          .timeout(const Duration(seconds: 2));

      expect(firstChunk, contains('echoed'));
      releaseStream.complete();
      await proxy.close();
      await gateway.close(force: true);
      client.close();
    },
  );
}
