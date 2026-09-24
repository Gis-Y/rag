// Run with: node --test src/service/request/index.test.mjs
import assert from 'node:assert/strict';
import { webcrypto } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { test } from 'node:test';
import { setImmediate as nextTurn } from 'node:timers/promises';
import vm from 'node:vm';
import ts from 'typescript';

const packageRequire = createRequire(new URL('../../../packages/axios/package.json', import.meta.url));

function loadTypeScript(url, imports = {}, cache = new Map()) {
  if (cache.has(url.href)) return cache.get(url.href);
  const module = { exports: {} };
  cache.set(url.href, module.exports);
  const source = readFileSync(url, 'utf8').replaceAll('import.meta.env', '({ DEV: false, VITE_SERVICE_SUCCESS_CODE: "200", VITE_SERVICE_LOGOUT_CODES: "401" })');
  const code = ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, esModuleInterop: true }
  }).outputText;
  vm.runInNewContext(code, {
    module,
    exports: module.exports,
    AbortController,
    crypto: webcrypto,
    setTimeout,
    clearTimeout,
    require: name => {
      if (name in imports) return imports[name];
      if (name.startsWith('.')) return loadTypeScript(new URL(`${name}.ts`, url), imports, cache);
      return packageRequire(name);
    }
  });
  return module.exports;
}

function setup({ refreshSucceeds = true } = {}) {
  const axiosPackage = loadTypeScript(new URL('../../../packages/axios/src/index.ts', import.meta.url));
  const pending = [];
  const auth = {
    token: 'account-a-token',
    identity: 1,
    getIdentityVersion() { return this.identity; },
    setToken(token) { this.token = token; },
    resetStore() { this.token = ''; this.identity += 1; }
  };
  const refresh = { calls: 0 };
  const storage = new Map([['refreshToken', 'old-refresh']]);
  const service = loadTypeScript(new URL('./index.ts', import.meta.url), {
    '@sa/axios': {
      ...axiosPackage,
      createFlatRequest: (config, options) => axiosPackage.createFlatRequest({
        ...config,
        adapter: request => new Promise((resolve, reject) => pending.push({ request, resolve, reject }))
      }, options)
    },
    '@/store/modules/auth': { useAuthStore: () => auth },
    '@/utils/service': { getServiceBaseURL: () => ({ baseURL: 'http://unused.test' }) },
    '@/utils/storage': { localStg: { get: key => storage.get(key) || '' } },
    '@/locales': { $t: text => text },
    './shared': {
      getAuthorization: () => auth.token ? `Bearer ${auth.token}` : null,
      handleExpiredRequest: state => {
        if (!state.refreshTokenFn) {
          refresh.calls += 1;
          state.refreshTokenFn = Promise.resolve().then(() => {
            if (refreshSucceeds) {
              auth.setToken('refreshed-token');
              storage.set('refreshToken', 'rotated-refresh');
              return true;
            }
            auth.resetStore();
            return false;
          });
        }
        return state.refreshTokenFn;
      },
      showErrorMsg() {}
    }
  });
  return {
    auth,
    refresh,
    request: service.request,
    pending,
    respond(newToken, index = 0, code = 200) {
      const { request, resolve } = pending.splice(index, 1)[0];
      resolve({
        status: 200, statusText: 'OK', headers: { 'new-token': newToken }, config: request,
        data: { code, data: 'ok' }
      });
    },
    fail(status, index = 0) {
      const { request, reject } = pending.splice(index, 1)[0];
      const { AxiosError } = packageRequire('axios');
      reject(new AxiosError('forbidden', 'ERR_BAD_REQUEST', request, undefined, { status, config: request, data: {} }));
    }
  };
}

test('HTTP 401 refreshes once and replays the original request', async () => {
  const { auth, refresh, request, pending, fail, respond } = setup();
  const result = request({ url: '/private' });
  await nextTurn();
  fail(401);
  await nextTurn();
  assert.equal(refresh.calls, 1);
  assert.equal(pending.length, 1);
  assert.equal(pending[0].request.url, '/private');
  assert.equal(pending[0].request.headers.get('Authorization'), 'Bearer refreshed-token');
  respond(undefined);
  const resolved = await result;
  assert.equal(resolved.error, null);
  assert.equal(resolved.data, 'ok');
  assert.equal(auth.token, 'refreshed-token');
});

test('logout replay revokes the refresh token created by automatic rotation', async () => {
  const { request, pending, fail, respond } = setup();
  const result = request({ url: '/users/logout', method: 'post', headers: { 'X-Refresh-Token': 'old-refresh' } });
  await nextTurn();
  fail(401);
  await nextTurn();
  assert.equal(pending[0].request.headers.get('X-Refresh-Token'), 'rotated-refresh');
  respond(undefined);
  assert.equal((await result).error, null);
});

test('a replayed request receives a fresh managed abort controller', async () => {
  const { request, pending, fail } = setup();
  const result = request({ url: '/private' });
  await nextTurn();
  const originalSignal = pending[0].request.signal;
  fail(401);
  await nextTurn();
  assert.notEqual(pending[0].request.signal, originalSignal);
  request.cancelAllRequest();
  assert.equal(pending[0].request.signal.aborted, true);
  pending[0].reject?.(new Error('canceled'));
  void result;
});

test('concurrent HTTP 401 responses share one refresh and replay independently', async () => {
  const { refresh, request, pending, fail, respond } = setup();
  const first = request({ url: '/private/one' });
  const second = request({ url: '/private/two' });
  await nextTurn();
  fail(401, 0);
  await nextTurn();
  assert.equal(refresh.calls, 1);
  // The second response arrives after the shared refresh has already changed
  // the access token; it must reuse that token instead of failing or refreshing again.
  fail(401, 0);
  await nextTurn();
  assert.equal(refresh.calls, 1);
  assert.equal(pending.length, 2);
  respond(undefined, 1);
  respond(undefined, 0);
  assert.equal((await first).error, null);
  assert.equal((await second).error, null);
});

test('failed refresh returns the original 401 without replaying', async () => {
  const { auth, refresh, request, pending, fail } = setup({ refreshSucceeds: false });
  const result = request({ url: '/private' });
  await nextTurn();
  fail(401);
  assert.ok((await result).error);
  assert.equal(refresh.calls, 1);
  assert.equal(pending.length, 0);
  assert.equal(auth.token, '');
});

test('a replayed 401 logs out without a second refresh or loop', async () => {
  const { auth, refresh, request, pending, fail } = setup();
  const result = request({ url: '/private' });
  await nextTurn();
  fail(401);
  await nextTurn();
  assert.equal(pending.length, 1);
  fail(401);
  assert.ok((await result).error);
  assert.equal(refresh.calls, 1);
  assert.equal(pending.length, 0);
  assert.equal(auth.token, '');
});

test('the refresh endpoint HTTP 401 never recursively refreshes itself', async () => {
  const { refresh, request, fail } = setup();
  const result = request({ url: '/auth/refreshToken' });
  await nextTurn();
  fail(401);
  assert.ok((await result).error);
  assert.equal(refresh.calls, 0);
});

test('a stale-account HTTP 401 cannot refresh or log out the current account', async () => {
  const { auth, refresh, request, fail } = setup();
  const result = request({ url: '/private' });
  await nextTurn();
  auth.identity += 1;
  auth.token = 'account-b-token';
  fail(401);
  assert.ok((await result).error);
  assert.equal(refresh.calls, 0);
  assert.equal(auth.token, 'account-b-token');
});

test('refresh completion cannot update a re-login with the same stored tokens', async () => {
  let resolveRefresh;
  const storage = new Map([['token', 'same-token'], ['refreshToken', 'same-refresh']]);
  const auth = {
    token: 'same-token', identity: 1,
    getIdentityVersion() { return this.identity; },
    setToken(token) { this.token = token; storage.set('token', token); },
    resetStore() { this.token = ''; this.identity += 1; storage.clear(); }
  };
  const shared = loadTypeScript(new URL('./shared.ts', import.meta.url), {
    '@/store/modules/auth': { useAuthStore: () => auth },
    '@/utils/storage': { localStg: { get: key => storage.get(key), set: (key, value) => storage.set(key, value) } },
    '../api': { fetchRefreshToken: () => new Promise(resolve => { resolveRefresh = resolve; }) }
  });
  const refresh = shared.handleExpiredRequest({ refreshTokenFn: null });
  auth.identity += 2;
  resolveRefresh({ error: null, data: { token: 'old-session-token', refreshToken: 'old-session-refresh' } });
  assert.equal(await refresh, false);
  assert.equal(auth.token, 'same-token');
  assert.equal(storage.get('refreshToken'), 'same-refresh');
});

test('generated request IDs cancel the matching in-flight controller', async () => {
  const { request, pending, respond } = setup();
  const result = request({ url: '/private' });
  await nextTurn();
  const signal = pending[0].request.signal;
  const requestId = pending[0].request.headers.get('X-Request-Id');
  assert.match(requestId, /^[a-f0-9]{32}$/);
  request.cancelRequest(requestId);
  assert.equal(signal.aborted, true);
  respond(undefined);
  await result;
});

test('completed requests release their abort controllers', async () => {
  const { request, pending, respond } = setup();
  const result = request({ url: '/private', headers: { 'X-Request-Id': 'known-id' } });
  await nextTurn();
  const signal = pending[0].request.signal;
  respond(undefined);
  await result;
  request.cancelRequest('known-id');
  assert.equal(signal.aborted, false);
});

test('the production request client has no hard-coded third-party token', () => {
  const source = readFileSync(new URL('./index.ts', import.meta.url), 'utf8');
  assert.doesNotMatch(source, /apifoxToken|FY65Vng88xra_BveQ5E_4/);
});

test('the real axios interceptor passes request identity to the token-refresh hook', async () => {
  const { auth, request, respond } = setup();
  const result = request({ url: '/private' });
  await nextTurn();
  respond('account-a-refreshed');
  assert.equal((await result).error, null);
  assert.equal(auth.token, 'account-a-refreshed');
});

test('a late New-Token response cannot revive a logged-out session', async () => {
  const { auth, request, respond } = setup();
  const result = request({ url: '/private' });
  await nextTurn();
  auth.token = '';
  auth.identity += 1;
  respond('old-account-refreshed');
  await result;
  assert.equal(auth.token, '');
});

test('a late New-Token response cannot overwrite a different account', async () => {
  const { auth, request, respond } = setup();
  const result = request({ url: '/private' });
  await nextTurn();
  auth.token = 'account-b-token';
  auth.identity += 1;
  respond('account-a-refreshed');
  await result;
  assert.equal(auth.token, 'account-b-token');
});

test('re-login with the same token still rejects a prior-session refresh', async () => {
  const { auth, request, respond } = setup();
  const result = request({ url: '/private' });
  await nextTurn();
  auth.identity += 2;
  respond('old-session-refreshed');
  await result;
  assert.equal(auth.token, 'account-a-token');
});

test('concurrent older-token responses cannot roll back the current token', async () => {
  const { auth, request, respond } = setup();
  const older = request({ url: '/private/older' });
  const newer = request({ url: '/private/newer' });
  await nextTurn();
  respond('newer-token', 1);
  await newer;
  respond('older-token');
  await older;
  assert.equal(auth.token, 'newer-token');
});

test('malformed New-Token header values are ignored', async () => {
  const { auth, request, respond } = setup();
  const result = request({ url: '/private' });
  await nextTurn();
  respond(['not', 'a', 'token']);
  await result;
  assert.equal(auth.token, 'account-a-token');
});

test('late business-auth failures do not log out a newer account', async () => {
  const { auth, request, respond } = setup();
  const result = request({ url: '/private' });
  await nextTurn();
  auth.token = 'account-b-token';
  auth.identity += 1;
  respond('old-refresh', 0, 401);
  assert.ok((await result).error);
  assert.equal(auth.token, 'account-b-token');
});

test('late HTTP 403 responses do not log out a newer account', async () => {
  const { auth, request, fail } = setup();
  const result = request({ url: '/private' });
  await nextTurn();
  auth.token = 'account-b-token';
  auth.identity += 1;
  fail(403);
  assert.ok((await result).error);
  assert.equal(auth.token, 'account-b-token');
});
