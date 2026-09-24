// Run with: node --test src/store/modules/chat/index.test.mjs
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { setImmediate as nextTurn } from 'node:timers/promises';
import vm from 'node:vm';
import { createPinia, defineStore, setActivePinia } from 'pinia';
import { computed, markRaw, reactive, ref, shallowRef, watch } from 'vue';
import ts from 'typescript';

const sockets = [];
class FakeWebSocket {
  constructor(url) {
    // Native WebSockets are non-reactive host objects; make the test double behave the same way.
    markRaw(this);
    this.url = url;
    this.sent = [];
    sockets.push(this);
  }

  open() {
    this.onopen?.();
  }

  message(payload) {
    this.onmessage?.({ data: JSON.stringify(payload) });
  }

  send(payload) {
    this.sent.push(payload);
  }

  close() {
    this.closed = true;
  }
}

globalThis.window = {
  addEventListener() {},
  removeEventListener() {},
  setTimeout: globalThis.setTimeout,
  clearTimeout: globalThis.clearTimeout,
  location: { href: 'https://ui.example/app' }
};
globalThis.document = {};
globalThis.WebSocket = FakeWebSocket;
const vueUse = await import('@vueuse/core');
const source = ts.transpileModule(readFileSync(new URL('./index.ts', import.meta.url), 'utf8').replaceAll('import.meta.env', 'serviceEnv'), {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 }
}).outputText;
const citations = { exports: {} };
vm.runInNewContext(ts.transpileModule(readFileSync(new URL('../../../utils/source-citations.ts', import.meta.url), 'utf8'), {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 }
}).outputText, { exports: citations.exports, module: citations });

async function setup(t, token = 'account-a', useProxy = true) {
  setActivePinia(createPinia());
  const auth = reactive({ token, getIdentityVersion: () => 0 });
  let ticketNumber = 0;
  const module = { exports: {} };
  vm.runInNewContext(source, {
    module,
    exports: module.exports,
    require: name => {
      if (name === '@/utils/source-citations') return citations.exports;
      if (name === '@/utils/service') {
        return { getServiceBaseURL: (_env, isProxy) => ({ otherBaseURL: { ws: isProxy ? '/proxy-ws' : '/' } }) };
      }
      if (name === '@/service/api') {
        return { fetchWebsocketTicket: async () => ({ data: { ticket: `ticket-${++ticketNumber}` }, error: null }) };
      }
      assert.equal(name, '@vueuse/core');
      return vueUse;
    },
    ref,
    shallowRef,
    computed,
    watch,
    defineStore,
    SetupStoreId: { Chat: 'chat-store' },
    useAuthStore: () => auth,
    window: globalThis.window,
    URL,
    serviceEnv: { DEV: useProxy, VITE_HTTP_PROXY: useProxy ? 'Y' : 'N' }
  });
  const chat = module.exports.useChatStore();
  await nextTurn();
  const socket = token ? sockets.at(-1) : undefined;
  socket?.open();
  t.after(() => {
    chat.wsClose();
    chat.$dispose();
  });
  return { chat, auth, socket };
}

test('production websocket uses the configured same-origin base instead of the development proxy', async t => {
  const { socket } = await setup(t, 'account-a', false);
  assert.match(socket.url, /^wss:\/\/ui\.example\/chat\/ticket-/);
  assert.equal(socket.url.includes('/proxy-ws/'), false);
});

test('raw queries, identical chunks and completion work without a mounted input component', async t => {
  const { chat, socket } = await setup(t);
  chat.sendMessage();
  assert.equal(socket.sent.length, 0);
  chat.input.message = 'A 支持私有化吗？';
  chat.sendMessage();
  assert.equal(socket.sent[0], 'A 支持私有化吗？');
  assert.equal(chat.isSending, true);
  socket.message({ chunk: '哈' });
  socket.message({ chunk: '哈' });
  assert.equal(chat.list[1].content, '哈哈');
  socket.message({ type: 'completion', status: 'finished' });
  assert.equal(chat.isSending, false);
  assert.equal(chat.list[1].status, 'finished');
  socket.message({ chunk: 'late' });
  assert.equal(chat.list[1].content, '哈哈');
});

test('trusted sources bind to the active round without reconnecting and survive completion', async t => {
  const { chat, socket } = await setup(t);
  chat.input.message = '相同文件名的两份文档';
  chat.sendMessage();
  const sources = [
    { number: 1, documentId: 12, version: 'v1', fileName: '报告(终稿).pdf' },
    { number: 2, documentId: 37, version: 'v2', fileName: '报告(终稿).pdf' }
  ];
  socket.message({ type: 'sources', sources });
  assert.equal(socket.closed, undefined);
  assert.equal(chat.list[1].sources[1].documentId, 37);
  socket.message({ chunk: '结果 [来源#2]' });
  socket.message({ type: 'completion', status: 'finished' });
  assert.equal(chat.list[1].sources[0].documentId, 12);
  chat.input.message = '下一问';
  chat.sendMessage();
  assert.equal(chat.list[3].sources.length, 0);
  socket.message({ type: 'sources', sources: [{ number: 1, fileName: '报告(终稿).pdf' }] });
  assert.equal(chat.list[3].sources.length, 0);
  assert.equal(chat.isSending, true);
});

test('stop waits for completion and trailing chunks cannot become a new answer', async t => {
  const { chat, socket } = await setup(t);
  chat.input.message = '第一问';
  chat.sendMessage();
  chat.stopAnswer();
  chat.stopAnswer();
  assert.equal(socket.sent[1], '{"type":"stop"}');
  assert.equal(chat.isStopping, true);
  chat.input.message = '第二问';
  chat.sendMessage();
  assert.equal(socket.sent.length, 2);
  assert.equal(chat.list.length, 2);
  socket.message({ chunk: '第一问的末尾' });
  socket.message({ type: 'completion', status: 'finished' });
  assert.equal(chat.isStopping, false);
  chat.sendMessage();
  assert.equal(socket.sent[2], '第二问');
  assert.equal(chat.list[3].content, '');
});

test('server errors stay locked until completion and do not become successful answers', async t => {
  const { chat, socket } = await setup(t);
  chat.input.message = '复杂问题';
  chat.sendMessage();
  socket.message({ error: '查询理解超时' });
  assert.equal(chat.isSending, true);
  assert.equal(chat.list[1].status, 'error');
  socket.message({ type: 'completion', status: 'finished' });
  assert.equal(chat.isSending, false);
  assert.equal(chat.list[1].status, 'error');
});

test('logout, login and token refresh close old sockets and clear the conversation synchronously', async t => {
  const { chat, auth, socket } = await setup(t);
  chat.input.message = '私有内容';
  chat.sendMessage();
  const staleCallback = socket.onmessage;
  auth.token = '';
  assert.equal(socket.closed, true);
  assert.equal(chat.wsStatus, 'CLOSED');
  assert.equal(chat.list.length, 0);
  assert.equal(chat.isSending, false);
  auth.token = 'account-b';
  await nextTurn();
  const current = sockets.at(-1);
  assert.match(current.url, /^wss:\/\/ui\.example\/proxy-ws\/chat\/ticket-/);
  assert.equal(current.url.includes('account-b'), false);
  current.open();
  chat.input.message = '新账号问题';
  chat.sendMessage();
  staleCallback({ data: '{"chunk":"旧账号回答"}' });
  assert.equal(chat.list[1].content, '');
  current.message({ chunk: '新账号回答' });
  assert.equal(chat.list[1].content, '新账号回答');
  auth.token = 'account-b-refreshed';
  await nextTurn();
  assert.equal(chat.list.length, 0);
  assert.equal(current.closed, true);
  assert.equal(sockets.at(-1).url.includes('account-b-refreshed'), false);
});

test('signed-out stores do not connect and unexpected frames do not dereference an empty list', async t => {
  const before = sockets.length;
  const { chat, auth } = await setup(t, '');
  assert.equal(sockets.length, before);
  auth.token = 'account-c';
  await nextTurn();
  const socket = sockets.at(-1);
  socket.open();
  socket.message({ type: 'completion', status: 'finished' });
  socket.message(null);
  assert.equal(chat.list.length, 0);
});

test('disconnect ends the active round and reconnection does not replay its query', async t => {
  const { chat, socket } = await setup(t);
  chat.input.message = '不能重复执行的问题';
  chat.sendMessage();
  chat.stopAnswer();
  socket.onclose({});
  assert.equal(chat.isSending, false);
  assert.equal(chat.isStopping, false);
  assert.equal(chat.list[1].status, 'error');
  await chat.wsOpen();
  const reconnected = sockets.at(-1);
  reconnected.open();
  assert.equal(reconnected.sent.length, 0);
});

test('stop send failure reconnects without replaying the interrupted question', async t => {
  const { chat, socket } = await setup(t);
  chat.input.message = '第一问';
  chat.sendMessage();
  socket.send = () => { throw new Error('connection closed'); };
  chat.stopAnswer();
  assert.equal(chat.isSending, false);
  assert.equal(chat.isStopping, false);
  assert.equal(socket.closed, true);
  assert.equal(chat.list[1].status, 'error');
  await nextTurn();
  const current = sockets.at(-1);
  assert.notEqual(current, socket);
  current.open();
  assert.equal(current.sent.length, 0);
  chat.input.message = '第二问';
  chat.sendMessage();
  assert.equal(current.sent[0], '第二问');
});

test('logging in again with the same token still creates an isolated connection', async t => {
  const { chat, auth, socket } = await setup(t);
  chat.input.message = '上次会话';
  chat.sendMessage();
  auth.token = '';
  auth.token = 'account-a';
  await nextTurn();
  assert.equal(chat.list.length, 0);
  assert.equal(socket.closed, true);
  assert.notEqual(sockets.at(-1), socket);
  assert.equal(sockets.at(-1).url.includes('account-a'), false);
});

for (const payload of ['not json', 'null', '[]', '{"chunk":5}']) {
  test(`invalid protocol closes the stream instead of leaving an active round: ${payload}`, async t => {
    const { chat, socket } = await setup(t);
    chat.input.message = '问题';
    chat.sendMessage();
    socket.onmessage({ data: payload });
    await nextTurn();
    assert.equal(chat.isSending, false);
    assert.equal(chat.wsStatus, 'CONNECTING');
    assert.equal(socket.closed, true);
    assert.equal(chat.list[1].status, 'error');
  });
}

function setupAuth(t) {
  setActivePinia(createPinia());
  const storage = new Map([['token', 'startup-token']]);
  const users = [];
  const logins = [];
  const logouts = [];
  const noop = () => {};
  const modules = {
    vue: { computed, reactive, ref },
    pinia: { defineStore },
    'vue-router': { useRoute: () => ({ meta: { constant: true } }) },
    '@sa/hooks': { useLoading: () => ({ loading: ref(false), startLoading: noop, endLoading: noop }) },
    '@/service/api': {
      fetchGetUserInfo: () => new Promise(resolve => users.push(resolve)),
      fetchLogin: () => new Promise(resolve => logins.push(resolve)),
      fetchLogout: () => new Promise(resolve => logouts.push(resolve))
    },
    '@/hooks/common/router': { useRouterPush: () => ({ toLogin: noop, redirectFromLogin: noop }) },
    '@/utils/storage': { localStg: { get: key => storage.get(key), set: (key, value) => storage.set(key, value), remove: key => storage.delete(key) } },
    '@/enum': { SetupStoreId: { Auth: 'auth-store' } },
    '@/locales': { $t: text => text },
    '../route': { useRouteStore: () => ({ resetStore: noop }) },
    '../tab': { useTabStore: () => ({ cacheTabs: noop, clearTabs: noop }) },
    './shared': { getToken: () => storage.get('token') || '', clearAuthStorage: () => storage.clear() }
  };
  const module = { exports: {} };
  const authSource = readFileSync(new URL('../auth/index.ts', import.meta.url), 'utf8').replaceAll('import.meta.env', '({})');
  vm.runInNewContext(ts.transpileModule(authSource, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 }
  }).outputText, {
    exports: module.exports, module, require: name => modules[name], window: globalThis.window,
    useKnowledgeBaseStore: () => ({ $reset: noop })
  });
  const auth = module.exports.useAuthStore();
  // Model the repository's setup-store plugin restoring the initial token on $reset.
  auth.$reset = () => { auth.token = 'startup-token'; };
  t.after(() => auth.$dispose());
  return {
    auth,
    resolveUser: (value, index = 0) => users.splice(index, 1)[0](value),
    resolveLogin: (value, index = 0) => logins.splice(index, 1)[0](value),
    resolveLogout: value => logouts.shift()(value)
  };
}

test('auth reset cannot restore the startup token or accept a late previous-user response', async t => {
  const { auth, resolveUser } = setupAuth(t);
  const pending = auth.initUserInfo();
  await auth.resetStore();
  assert.equal(auth.token, '');
  resolveUser({ data: { id: 42, username: 'old-user', role: 'ADMIN' }, error: null });
  await pending;
  assert.equal(auth.token, '');
  assert.equal(auth.userInfo.id, 0);
});

test('same-account token refresh does not invalidate an in-flight user-info response', async t => {
  const { auth, resolveUser } = setupAuth(t);
  const pending = auth.initUserInfo();
  auth.setToken('refreshed-token');
  resolveUser({ data: { id: 42, username: 'current-user', role: 'USER' }, error: null });
  await pending;
  assert.equal(auth.token, 'refreshed-token');
  assert.equal(auth.userInfo.id, 42);
});

test('a stale startup user-info response cannot log out a newer login', async t => {
  const { auth, resolveUser, resolveLogin } = setupAuth(t);
  const startup = auth.initUserInfo();
  const login = auth.login('new-account', 'password');
  resolveLogin({ data: { token: 'new-token', refreshToken: 'new-refresh' }, error: null });
  await nextTurn();
  resolveUser({ data: { id: 43, username: 'new-account', role: 'USER' }, error: null }, 1);
  await login;
  resolveUser({ data: { id: 42, username: 'old-account', role: 'USER' }, error: null });
  await startup;
  assert.equal(auth.token, 'new-token');
  assert.equal(auth.userInfo.id, 43);
});

test('a delayed logout cannot reset a newer login', async t => {
  const { auth, resolveUser, resolveLogin, resolveLogout } = setupAuth(t);
  const logout = auth.logout();
  const login = auth.login('new-account', 'password');
  resolveLogin({ data: { token: 'new-token', refreshToken: 'new-refresh' }, error: null });
  await nextTurn();
  resolveUser({ data: { id: 43, username: 'new-account', role: 'USER' }, error: null });
  await login;
  resolveLogout({ error: null });
  await logout;
  assert.equal(auth.token, 'new-token');
});

test('logout clears local credentials even when server revocation fails', async t => {
  const { auth, resolveLogout } = setupAuth(t);
  const logout = auth.logout();
  resolveLogout({ error: new Error('offline') });
  await logout;
  assert.equal(auth.token, '');
});

test('an older login response cannot replace a newer successful login', async t => {
  const { auth, resolveUser, resolveLogin } = setupAuth(t);
  const older = auth.login('old-account', 'password');
  const newer = auth.login('new-account', 'password');
  resolveLogin({ data: { token: 'new-token', refreshToken: 'new-refresh' }, error: null }, 1);
  await nextTurn();
  resolveUser({ data: { id: 43, username: 'new-account', role: 'USER' }, error: null });
  await newer;
  resolveLogin({ data: { token: 'old-token', refreshToken: 'old-refresh' }, error: null });
  await older;
  assert.equal(auth.token, 'new-token');
  assert.equal(auth.userInfo.id, 43);
});
