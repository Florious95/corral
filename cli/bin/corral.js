#!/usr/bin/env node
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const packageJson = require('../../package.json');
const { targetFor } = require('../lib/platform');

const root = path.resolve(__dirname, '..', '..');

function usage() {
  console.log(`Corral ${packageJson.version}

Usage:
  corral up       Start the gateway
  corral status   Show gateway, listener, tsnet, and tmux status
  corral down     Stop the gateway gracefully
  corral version  Print the installed version`);
}

function runtimeEnvironment(command) {
  const environment = { ...process.env };
  if (command !== 'up') return environment;

  targetFor();
  const gateway =
    environment.CORRAL_GATEWAY_BIN || path.join(root, 'cli', 'vendor', 'corral-gateway');
  const web = environment.GATEWAY_STATIC || path.join(root, 'cli', 'vendor', 'web', 'dist');
  const versionFile = path.join(root, 'cli', 'vendor', 'VERSION');
  if (!fs.existsSync(gateway)) {
    throw new Error(
      `Gateway binary is missing at ${gateway}. Reinstall @florious95/corral, or build from source with ` +
        './scripts/corral-up.sh: https://github.com/Florious95/corral/blob/main/scripts/corral-up.sh',
    );
  }
  if (!environment.CORRAL_GATEWAY_BIN) {
    const installedVersion = fs.existsSync(versionFile)
      ? fs.readFileSync(versionFile, 'utf8').trim()
      : '';
    if (installedVersion !== packageJson.version) {
      throw new Error(
        `Installed runtime version ${installedVersion || 'unknown'} does not match package version ${packageJson.version}. Reinstall the package.`,
      );
    }
  }
  environment.CORRAL_GATEWAY_BIN = gateway;
  if (fs.existsSync(path.join(web, 'index.html'))) environment.GATEWAY_STATIC = web;
  return environment;
}

function run(command) {
  const script = path.join(root, 'scripts', `corral-${command}.sh`);
  const result = spawnSync('/bin/bash', [script], {
    env: runtimeEnvironment(command),
    stdio: 'inherit',
  });
  if (result.error) throw result.error;
  if (result.signal) {
    throw new Error(`${path.basename(script)} stopped by signal ${result.signal}`);
  }
  return result.status ?? 1;
}

function main(argv = process.argv.slice(2)) {
  const command = argv[0];
  if (!command || command === 'help' || command === '--help' || command === '-h') {
    usage();
    return 0;
  }
  if (command === 'version' || command === '--version' || command === '-v') {
    console.log(packageJson.version);
    return 0;
  }
  if (!['up', 'status', 'down'].includes(command) || argv.length !== 1) {
    usage();
    return 2;
  }
  return run(command);
}

if (require.main === module) {
  try {
    process.exitCode = main();
  } catch (error) {
    console.error(`corral: ${error.message}`);
    process.exitCode = 1;
  }
}

module.exports = { main, runtimeEnvironment };
