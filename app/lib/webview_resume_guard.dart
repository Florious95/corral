class WebViewResumeGuard {
  bool _fileSelectionActive = false;

  Future<T> duringFileSelection<T>(Future<T> Function() select) async {
    _fileSelectionActive = true;
    try {
      return await select();
    } finally {
      _fileSelectionActive = false;
    }
  }

  bool shouldReload({required bool hasActiveStream}) {
    return !_fileSelectionActive && !hasActiveStream;
  }

  Future<bool> reloadOnResumeIfNeeded({
    required bool hasActiveStream,
    required Future<void> Function() reload,
  }) async {
    if (!shouldReload(hasActiveStream: hasActiveStream)) return false;
    await reload();
    return true;
  }
}
