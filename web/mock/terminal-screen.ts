import type { V2EntryScreen } from '../src/v2/types';

export const terminalScreenFixture: Omit<V2EntryScreen, 'entryId'> = {
  cols: 96,
  rows: 18,
  cursorX: 2,
  cursorY: 3,
  content: [
    '\u001b[1;36mClaude Code\u001b[0m  /Users/demo/Projects/sample-app',
    '',
    '\u001b[1;33mAllow this action?\u001b[0m',
    '  \u001b[32m1. Yes\u001b[0m',
    '\u001b[7m  2. Yes, and remember for this session\u001b[0m',
    '  \u001b[31m3. No\u001b[0m',
    '',
    '\u001b[47m Automation CLI message (ANSI white) \u001b[0m',
    '\u001b[48;5;252m Automation CLI message (256 gray) \u001b[0m',
    '\u001b[48;2;245;242;235m Automation CLI message (truecolor) \u001b[0m',
    '\u001b[44m Colored background stays blue \u001b[0m',
    '',
    '\u001b[2mUse ↑/↓ to select · Enter to confirm · Esc to cancel\u001b[0m',
  ].join('\n'),
  hash: '4f75bd3fa4d46061491649048fb65568a9b9ce631e48bdad6dfb6295d7f1b6d7',
};
