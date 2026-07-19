'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { expectedChecksum, installVerified, sha256, validateVendor } = require('../postinstall');

test('accepts only a checksum naming the same release asset', () => {
  const digest = 'a'.repeat(64);
  assert.equal(expectedChecksum(`${digest}  corral-v0.1.0-linux-amd64.tar.gz\n`, 'corral-v0.1.0-linux-amd64.tar.gz'), digest);
  assert.throws(
    () => expectedChecksum(`${digest}  another.tar.gz\n`, 'corral-v0.1.0-linux-amd64.tar.gz'),
    /expected corral-v0.1.0-linux-amd64.tar.gz/,
  );
});

test('validates versioned runtime contents and installs atomically', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'corral-postinstall-test-'));
  const staging = path.join(root, 'staging');
  const vendor = path.join(root, 'vendor');
  fs.mkdirSync(path.join(staging, 'web', 'dist'), { recursive: true });
  fs.writeFileSync(path.join(staging, 'VERSION'), '0.1.0\n');
  fs.writeFileSync(path.join(staging, 'corral-gateway'), 'gateway');
  fs.writeFileSync(path.join(staging, 'web', 'dist', 'index.html'), 'index');
  fs.mkdirSync(vendor);
  fs.writeFileSync(path.join(vendor, 'old'), 'old');

  assert.equal(validateVendor(staging, '0.1.0'), true);
  assert.equal(validateVendor(staging, '0.1.1'), false);
  installVerified(staging, vendor);
  assert.equal(validateVendor(vendor, '0.1.0'), true);
  assert.equal(fs.existsSync(path.join(vendor, 'old')), false);
  assert.equal(sha256(path.join(vendor, 'VERSION')).length, 64);
  fs.rmSync(root, { recursive: true, force: true });
});
