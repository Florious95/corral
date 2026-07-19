#!/usr/bin/env node
'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const https = require('node:https');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const packageJson = require('../package.json');
const { releaseAsset, releaseTag, targetFor } = require('./lib/platform');

const repositoryBase = 'https://github.com/Florious95/corral';
const manualInstall =
  'Build from source with ./scripts/corral-up.sh: ' +
  'https://github.com/Florious95/corral/blob/main/scripts/corral-up.sh';

function download(url, destination, redirects = 0) {
  return new Promise((resolve, reject) => {
    const request = https.get(url, { headers: { 'User-Agent': 'corral-installer' } }, (response) => {
      if (
        response.statusCode >= 300 &&
        response.statusCode < 400 &&
        response.headers.location
      ) {
        response.resume();
        if (redirects >= 5) return reject(new Error(`Too many redirects for ${url}`));
        const redirected = new URL(response.headers.location, url).toString();
        return download(redirected, destination, redirects + 1).then(resolve, reject);
      }
      if (response.statusCode !== 200) {
        response.resume();
        return reject(new Error(`Download failed with HTTP ${response.statusCode}: ${url}`));
      }
      const output = fs.createWriteStream(destination, { mode: 0o600 });
      response.pipe(output);
      output.on('finish', () => output.close(resolve));
      output.on('error', reject);
    });
    request.setTimeout(30_000, () => request.destroy(new Error(`Download timed out: ${url}`)));
    request.on('error', reject);
  });
}

function sha256(file) {
  const hash = crypto.createHash('sha256');
  hash.update(fs.readFileSync(file));
  return hash.digest('hex');
}

function expectedChecksum(text, asset) {
  const line = text.trim().split(/\r?\n/).find(Boolean);
  const match = line && line.match(/^([a-fA-F0-9]{64})(?:\s+\*?(.+))?$/);
  if (!match) throw new Error('Release checksum file has an invalid format');
  if (match[2] && path.basename(match[2]) !== asset) {
    throw new Error(`Checksum names ${match[2]}, expected ${asset}`);
  }
  return match[1].toLowerCase();
}

function validateVendor(directory, version) {
  const versionFile = path.join(directory, 'VERSION');
  const gateway = path.join(directory, 'corral-gateway');
  const index = path.join(directory, 'web', 'dist', 'index.html');
  return (
    fs.existsSync(versionFile) &&
    fs.readFileSync(versionFile, 'utf8').trim() === version &&
    fs.existsSync(gateway) &&
    fs.statSync(gateway).isFile() &&
    fs.existsSync(index) &&
    fs.statSync(index).isFile()
  );
}

function installVerified(staging, vendor) {
  const backup = `${vendor}.backup-${process.pid}`;
  fs.rmSync(backup, { recursive: true, force: true });
  if (fs.existsSync(vendor)) fs.renameSync(vendor, backup);
  try {
    fs.renameSync(staging, vendor);
    fs.rmSync(backup, { recursive: true, force: true });
  } catch (error) {
    fs.rmSync(vendor, { recursive: true, force: true });
    if (fs.existsSync(backup)) fs.renameSync(backup, vendor);
    throw error;
  }
}

async function main() {
  if (process.env.CORRAL_SKIP_DOWNLOAD === '1') {
    console.log('[corral] Skipping release download because CORRAL_SKIP_DOWNLOAD=1.');
    return;
  }

  const version = packageJson.version;
  const target = targetFor();
  const tag = releaseTag(version);
  const asset = releaseAsset(version);
  const base = (process.env.CORRAL_RELEASE_BASE_URL || repositoryBase).replace(/\/$/, '');
  const release = `${base}/releases/download/${tag}`;
  const cliDir = __dirname;
  const vendor = path.join(cliDir, 'vendor');
  if (validateVendor(vendor, version)) {
    console.log(`[corral] ${target} runtime v${version} is already installed.`);
    return;
  }

  const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'corral-install-'));
  const archive = path.join(temporary, asset);
  const checksumFile = `${archive}.sha256`;
  const staging = path.join(cliDir, `.vendor-staging-${process.pid}`);
  try {
    console.log(`[corral] Downloading ${asset} from ${tag}...`);
    await download(`${release}/${asset}`, archive);
    await download(`${release}/${asset}.sha256`, checksumFile);
    const expected = expectedChecksum(fs.readFileSync(checksumFile, 'utf8'), asset);
    const actual = sha256(archive);
    if (actual !== expected) throw new Error(`SHA256 mismatch: expected ${expected}, got ${actual}`);

    fs.rmSync(staging, { recursive: true, force: true });
    fs.mkdirSync(staging, { recursive: true, mode: 0o755 });
    const extracted = spawnSync('tar', ['-xzf', archive, '-C', staging], { encoding: 'utf8' });
    if (extracted.error) throw new Error(`Cannot run tar: ${extracted.error.message}`);
    if (extracted.status !== 0) {
      throw new Error(`Cannot extract release archive: ${extracted.stderr.trim()}`);
    }
    if (!validateVendor(staging, version)) {
      throw new Error(`Release archive is incomplete or is not version ${version}`);
    }
    fs.chmodSync(path.join(staging, 'corral-gateway'), 0o755);
    installVerified(staging, vendor);
    console.log(`[corral] Installed ${target} runtime v${version}.`);
  } finally {
    fs.rmSync(staging, { recursive: true, force: true });
    fs.rmSync(temporary, { recursive: true, force: true });
  }
}

if (require.main === module) {
  main().catch((error) => {
    console.error(`[corral] Installation failed: ${error.message}`);
    console.error(`[corral] No partial runtime was installed. ${manualInstall}`);
    process.exitCode = 1;
  });
}

module.exports = { expectedChecksum, installVerified, main, sha256, validateVendor };
