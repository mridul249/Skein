/**
 * Minimal declarations for the Node built-ins the tests use.
 *
 * @types/node is not installed and is not added here: Rules.md §3 requires
 * asking before any new dependency, and this file is a dozen lines against a
 * package with a large surface the tests do not need. If the frontend ever
 * grows a real test framework, delete this and use the real types.
 */

declare module 'node:test' {
  function test(name: string, fn: () => void | Promise<void>): void;
  export default test;
}

declare module 'node:assert/strict' {
  interface Assert {
    (value: unknown, message?: string): void;
    ok(value: unknown, message?: string): void;
    equal(actual: unknown, expected: unknown, message?: string): void;
    notEqual(actual: unknown, expected: unknown, message?: string): void;
    deepEqual(actual: unknown, expected: unknown, message?: string): void;
  }
  const assert: Assert;
  export default assert;
}

declare function setImmediate(fn: () => void): void;
