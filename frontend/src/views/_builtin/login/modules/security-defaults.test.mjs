import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

test('production sources do not ship demo users or passwords', () => {
  const login = readFileSync(new URL('./pwd-login.vue', import.meta.url), 'utf8');
  const ddl = readFileSync(new URL('../../../../../../docs/ddl.sql', import.meta.url), 'utf8');
  assert.doesNotMatch(login, /admin123|test123|handleAccountLogin/);
  assert.doesNotMatch(ddl, /INSERT\s+INTO\s+users/i);
});

test('local deployment requires external credentials and binds published ports to loopback', () => {
  const compose = readFileSync(new URL('../../../../../../deployments/docker-compose.yaml', import.meta.url), 'utf8').replaceAll('\r\n', '\n');
  const ignored = readFileSync(new URL('../../../../../../.gitignore', import.meta.url), 'utf8').replaceAll('\r\n', '\n');
  for (const variable of ['MYSQL_ROOT_PASSWORD', 'DATABASE_REDIS_PASSWORD', 'MINIO_ACCESS_KEY_ID', 'MINIO_SECRET_ACCESS_KEY', 'DOCUMENT_WORKER_TOKEN']) {
    assert.ok(compose.includes(`\${${variable}:?`), `${variable} must be explicitly supplied`);
  }
  const ports = [...compose.matchAll(/^\s*- "([^"]+:\d+:\d+)"$/gm)];
  assert.ok(ports.length > 0);
  for (const [, port] of ports) {
    assert.ok(port.startsWith('127.0.0.1:'), `unexpected public port: ${port}`);
  }
  assert.ok(ignored.includes('/.env\n'));
  assert.ok(ignored.includes('/deployments/.env\n'));
});
