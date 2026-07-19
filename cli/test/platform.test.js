'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const { releaseAsset, releaseTag, targetFor } = require('../lib/platform');

test('maps only supported release targets', () => {
  assert.equal(targetFor('darwin', 'arm64'), 'darwin-arm64');
  assert.equal(targetFor('darwin', 'x64'), 'darwin-amd64');
  assert.equal(targetFor('linux', 'x64'), 'linux-amd64');
  assert.throws(() => targetFor('win32', 'x64'), /Unsupported platform win32\/x64/);
});

test('pins release tag and asset to the exact package version', () => {
  assert.equal(releaseTag('0.1.0'), 'v0.1.0');
  assert.equal(
    releaseAsset('0.1.0', 'darwin', 'arm64'),
    'corral-v0.1.0-darwin-arm64.tar.gz',
  );
  assert.throws(() => releaseTag('latest'), /Invalid package version/);
});
