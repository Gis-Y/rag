// Run with: node --test build/config/proxy.test.mjs
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import vm from 'node:vm';
import ts from 'typescript';

test('websocket proxy preserves Host while HTTP proxies still change origin', () => {
  const source = ts.transpileModule(readFileSync(new URL('./proxy.ts', import.meta.url), 'utf8'), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 }
  }).outputText;
  const module = { exports: {} };
  vm.runInNewContext(source, {
    module,
    exports: module.exports,
    require: name => {
      if (name === '../../src/utils/service') {
        return {
          createServiceConfig: () => ({
            baseURL: 'http://backend.test/api/v1',
            proxyPattern: '/proxy-default',
            other: [{ key: 'ws', baseURL: 'ws://backend.test', proxyPattern: '/proxy-ws' }]
          })
        };
      }
      if (name === 'consola') return { consola: { log() {} } };
      if (name === 'kolorist') return new Proxy({}, { get: () => value => value });
      throw new Error(`unexpected import ${name}`);
    }
  });

  const proxy = module.exports.createViteProxy({ VITE_HTTP_PROXY: 'Y', VITE_PROXY_LOG: 'N' }, true);
  assert.equal(proxy['/proxy-default'].changeOrigin, true);
  assert.equal(proxy['/proxy-default'].ws, false);
  assert.equal(proxy['/proxy-ws'].changeOrigin, false);
  assert.equal(proxy['/proxy-ws'].ws, true);
});
