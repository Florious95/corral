'use strict';

const supportedTargets = new Map([
  ['darwin:arm64', 'darwin-arm64'],
  ['darwin:x64', 'darwin-amd64'],
  ['linux:x64', 'linux-amd64'],
]);

function targetFor(platform = process.platform, arch = process.arch) {
  const target = supportedTargets.get(`${platform}:${arch}`);
  if (!target) {
    throw new Error(
      `Unsupported platform ${platform}/${arch}. ` +
        'Build from source with ./scripts/corral-up.sh: ' +
        'https://github.com/Florious95/corral/blob/main/scripts/corral-up.sh',
    );
  }
  return target;
}

function releaseTag(version) {
  if (!/^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(version)) {
    throw new Error(`Invalid package version ${JSON.stringify(version)}`);
  }
  return `v${version}`;
}

function releaseAsset(version, platform, arch) {
  const tag = releaseTag(version);
  return `corral-${tag}-${targetFor(platform, arch)}.tar.gz`;
}

module.exports = { releaseAsset, releaseTag, targetFor };
