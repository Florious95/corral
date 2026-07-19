import 'dart:async';

import 'package:flutter_test/flutter_test.dart';
import 'package:corral/webview_resume_guard.dart';

void main() {
  test(
    'does not reload while file selection leaves and resumes the app',
    () async {
      final guard = WebViewResumeGuard();
      final selection = Completer<List<String>>();

      final result = guard.duringFileSelection(() => selection.future);

      expect(guard.shouldReload(hasActiveStream: false), isFalse);
      selection.complete(['content://selected/image.jpg']);
      await expectLater(result, completion(hasLength(1)));
      expect(guard.shouldReload(hasActiveStream: false), isTrue);
    },
  );

  test('reloads only when the page stream is disconnected', () {
    final guard = WebViewResumeGuard();

    expect(guard.shouldReload(hasActiveStream: true), isFalse);
    expect(guard.shouldReload(hasActiveStream: false), isTrue);
  });

  test('injects reload and calls it only for a disconnected stream', () async {
    final guard = WebViewResumeGuard();
    var reloads = 0;

    expect(
      await guard.reloadOnResumeIfNeeded(
        hasActiveStream: true,
        reload: () async => reloads++,
      ),
      isFalse,
    );
    expect(
      await guard.reloadOnResumeIfNeeded(
        hasActiveStream: false,
        reload: () async => reloads++,
      ),
      isTrue,
    );
    expect(reloads, 1);
  });
}
