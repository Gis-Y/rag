// Run with: node --test src/views/knowledge-base/index.test.mjs
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { setImmediate as nextTurn } from 'node:timers/promises';
import vm from 'node:vm';
import { ref, reactive, watch } from 'vue';
import { parse, compileScript, compileTemplate } from 'vue/compiler-sfc';
import ts from 'typescript';

const statuses = { Uploading: 0, Completed: 1, Pending: 2, Paused: 3, Break: 4 };
const md5 = 'a'.repeat(32);
const sourceURL = new URL('./index.vue', import.meta.url);
const source = readFileSync(sourceURL, 'utf8');
const { descriptor } = parse(source);

function actualFunctions(script, names, kind = ts.ScriptKind.TS) {
  const ast = ts.createSourceFile('actual.tsx', script, ts.ScriptTarget.ES2022, true, kind);
  const selected = ast.statements.filter(node => ts.isFunctionDeclaration(node) && names.includes(node.name?.text));
  assert.equal(selected.length, names.length);
  return ts.transpileModule(selected.map(node => node.getText(ast)).join('\n'), {
    compilerOptions: { target: ts.ScriptTarget.ES2022 }
  }).outputText;
}

const pageFunctions = actualFunctions(descriptor.scriptSetup.content, ['getList', 'canDelete', 'handleDelete'], ts.ScriptKind.TSX);

function testAuth() {
  return reactive({ token: 'account-a', identity: 1, userInfo: { id: 7, role: 'USER' }, getIdentityVersion() { return this.identity; } });
}

function setupPage(tasks, authStore = testAuth()) {
  const data = ref([]);
  const calls = [];
  const canceled = [];
  const request = async options => {
    calls.push(options);
    const id = Number(options.url.split('/').at(-1));
    data.value = data.value.filter(row => row.id !== id);
    return { error: null };
  };
  request.cancelRequest = id => canceled.push(id);
  const context = { tasks, data, request, listRequest: 0, loading: ref(false), apiFn: async () => ({ error: null, data: { data: data.value } }), authStore, UploadStatus: statuses,
    window: { $message: { success() {} } } };
  vm.createContext(context);
  vm.runInContext(pageFunctions, context);
  return Object.assign(context, { calls, canceled });
}

function setupUploads() {
  const pending = [];
  const auth = testAuth();
  const canceled = [];
  const module = { exports: {} };
  let sequence = 0;
  const modules = {
    '~/packages/axios/src': { REQUEST_ID_KEY: 'request-id' },
    '~/packages/utils/src': { nanoid: () => `local-${++sequence}` }
  };
  const code = ts.transpileModule(readFileSync(new URL('../../store/modules/knowledge-base/index.ts', import.meta.url), 'utf8'), {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS }
  }).outputText;
  const request = options => options.url === '/upload/supported-types'
    ? Promise.resolve({ error: null, data: { maxFileBytes: 100 } })
    : new Promise(resolve => pending.push({ options, resolve, userId: auth.userInfo.id, identity: auth.getIdentityVersion() }));
  request.cancelRequest = id => canceled.push(id);
  vm.runInNewContext(code, {
    module, exports: module.exports, require: name => modules[name], ref, watch,
    defineStore: (_id, setup) => setup, SetupStoreId: { KnowledgeBase: 'kb' }, UploadStatus: statuses,
    useAuthStore: () => auth, chunkSize: 4, calculateMD5: async () => md5,
    request,
    window: { $message: { error() {} } }, console: { error() {} }
  });
  const store = module.exports.useKnowledgeBaseStore();
  const form = { fileList: [{ file: { name: 'same.pdf', size: 8, slice: (start, end) => ({ start, end }) } }], isPublic: false, orgTag: null };
  return { store, auth, pending, form, canceled };
}

test('delete uses the clicked SQL ID, never another owner with the same MD5', async () => {
  const own = { id: 12, userId: 7, fileMd5: md5, status: statuses.Completed };
  const publicFile = { id: 37, userId: 8, fileMd5: md5, status: statuses.Completed };
  const page = setupPage(ref([own, publicFile]));
  page.data.value = [own, publicFile];
  assert.equal(page.canDelete(publicFile), false);
  await page.handleDelete(publicFile);
  assert.equal(page.calls.length, 0);
  assert.deepEqual(Array.from(page.tasks.value, row => row.id), [12, 37]);
  page.authStore.userInfo.role = 'ADMIN';
  await page.handleDelete(publicFile);
  assert.equal(page.calls[0].url, '/documents/37');
  assert.equal(page.calls[0].method, 'DELETE');
  assert.deepEqual(Array.from(page.tasks.value, row => row.id), [12]);
});

test('cancel a not-yet-persisted task by stable local identity without an HTTP delete', async () => {
  const first = { localId: 'first', userId: 7, fileMd5: md5, requestIds: ['request-1'] };
  const second = { localId: 'second', userId: 7, fileMd5: md5 };
  const page = setupPage(ref([first, second]));
  await page.handleDelete(first);
  assert.equal(page.tasks.value.length, 1);
  assert.equal(page.tasks.value[0].localId, 'second');
  assert.deepEqual(page.canceled, ['request-1']);
  assert.equal(page.calls.length, 0);
});

test('refresh replaces stale server rows while retaining queued and interrupted local tasks', async () => {
  const page = setupPage(ref([
    { id: 12, userId: 7, fileMd5: md5, status: statuses.Completed },
    { localId: 'waiting', userId: 7, fileMd5: 'b', status: statuses.Pending },
    { localId: 'interrupted', userId: 7, fileMd5: 'c', status: statuses.Break }
  ]));
  await page.getList();
  assert.deepEqual(Array.from(page.tasks.value, row => row.localId), ['waiting', 'interrupted']);
});

test('empty refresh preserves an in-flight upload and callbacks finish it without touching same-MD5 public files', async () => {
  const { store, auth, pending, form } = setupUploads();
  const publicFile = { id: 37, userId: 8, fileMd5: md5, status: statuses.Completed, progress: 23 };
  store.tasks.value.push(publicFile);
  await store.enqueueUpload(form);
  assert.equal(store.tasks.value.length, 2, 'another owner must not prevent this user uploading the same content');
  const local = store.tasks.value.find(row => row.localId);
  assert.ok(local.localId);
  const page = setupPage(store.tasks, auth);
  await page.getList();
  assert.equal(store.tasks.value.length, 1);
  assert.equal(store.tasks.value[0], local);
  page.data.value = [publicFile];
  await page.getList();
  assert.equal(store.tasks.value.find(row => row.localId), local);
  pending.shift().resolve({ error: null, data: { uploaded: [0], progress: 50 } });
  await nextTurn();
  assert.equal(local.progress, 50);
  assert.equal(store.tasks.value.find(row => row.id === 37).progress, 23);
  pending.shift().resolve({ error: null, data: { uploaded: [0, 1], progress: 100 } });
  await nextTurn();
  assert.equal(pending[0].options.url, '/upload/merge');
  pending.shift().resolve({ error: null });
  await nextTurn();
  assert.equal(local.status, statuses.Completed);
  assert.equal(store.activeUploads.value.size, 0);
  page.data.value = [publicFile, { id: 12, userId: 7, fileMd5: md5, status: statuses.Completed }];
  await page.getList();
  assert.equal(store.tasks.value.length, 2);
  assert.equal(local.id, 12);
  assert.equal(store.tasks.value.find(row => row.id === 12), local);
});

test('late callbacks from a canceled upload cannot update or merge its same-MD5 replacement', async () => {
  const { store, auth, pending, form } = setupUploads();
  await store.enqueueUpload(form);
  const old = store.tasks.value[0];
  const page = setupPage(store.tasks, auth);
  await page.handleDelete(old);
  await store.enqueueUpload(form);
  const replacement = store.tasks.value[0];
  assert.notEqual(replacement.localId, old.localId);
  pending.shift().resolve({ error: null, data: { uploaded: [0, 1], progress: 100 } });
  await nextTurn();
  assert.equal(replacement.progress, 0);
  assert.equal(replacement.status, statuses.Uploading);
  assert.equal(pending.length, 1);
  assert.equal(pending[0].options.url, '/upload/chunk');
  await page.handleDelete(replacement);
  pending.shift().resolve({ error: new Error('canceled') });
  await nextTurn();
  assert.equal(store.activeUploads.value.size, 0);
});

test('logout and account replacement cancel the old upload and never send its next chunk or merge', async () => {
  for (const nextUserId of [8, 7]) {
    const { store, auth, pending, form, canceled } = setupUploads();
    await store.enqueueUpload(form);
    const oldRequest = pending.shift();
    assert.equal(oldRequest.userId, 7);
    auth.identity += 1;
    auth.token = '';
    auth.userInfo.id = 0;
    assert.equal(store.tasks.value.length, 0);
    assert.ok(canceled.includes(oldRequest.options.headers['request-id']));
    auth.identity += 1;
    auth.userInfo.id = nextUserId;
    auth.token = 'new-session';
    oldRequest.resolve({ error: null, data: { uploaded: [0], progress: 50 } });
    await nextTurn();
    assert.equal(pending.length, 0, 'old file must not send chunk 1 in the new session');
    assert.equal(store.activeUploads.value.size, 0);
    assert.equal(store.tasks.value.length, 0);
  }
});

test('identity-version changes suppress old callbacks even before token or user ID changes', async () => {
  const { store, auth, pending, form } = setupUploads();
  await store.enqueueUpload(form);
  const oldTask = store.tasks.value[0];
  auth.identity += 1; // login() invalidates identity before its token request finishes.
  pending.shift().resolve({ error: null, data: { uploaded: [0, 1], progress: 100 } });
  await nextTurn();
  assert.equal(pending.length, 0, 'identity mismatch must block merge, not only cross-user chunks');
  assert.equal(oldTask.progress, 0);
  assert.notEqual(oldTask.status, statuses.Completed);
});

test('same-account token refresh keeps the current upload valid', async () => {
  const { store, auth, pending, form } = setupUploads();
  await store.enqueueUpload(form);
  const task = store.tasks.value[0];
  auth.token = 'refreshed-account-a';
  pending.shift().resolve({ error: null, data: { uploaded: [0], progress: 50 } });
  await nextTurn();
  assert.equal(store.tasks.value[0], task);
  assert.equal(pending[0].options.data.chunkIndex, 1);
  const page = setupPage(store.tasks, auth);
  await page.handleDelete(task);
  pending.shift().resolve({ error: new Error('canceled') });
  await nextTurn();
});

test('a pre-delete refresh cannot resurrect a row after the post-delete empty response wins', async () => {
  const row = { id: 12, userId: 7, fileMd5: md5, status: statuses.Completed };
  const page = setupPage(ref([row]));
  const fetches = [];
  page.apiFn = () => new Promise(resolve => fetches.push(resolve));
  page.getData = () => { throw new Error('shared table data must not be used for list refresh'); };
  const oldRefresh = page.getList();
  const deletion = page.handleDelete(row);
  await nextTurn();
  assert.equal(fetches.length, 2);
  fetches[1]({ error: null, data: { data: [] } });
  await deletion;
  assert.equal(page.tasks.value.length, 0);
  page.data.value = [row]; // Even a stale table cache cannot be used instead of this response's own payload.
  fetches[0]({ error: null, data: { data: [row] } });
  await oldRefresh;
  assert.equal(page.tasks.value.length, 0);
  assert.equal(page.loading.value, false);
});

test('failed latest refresh keeps local state and an older success cannot override it', async () => {
  const page = setupPage(ref([]));
  const fetches = [];
  page.apiFn = () => new Promise(resolve => fetches.push(resolve));
  const first = page.getList();
  const latest = page.getList();
  fetches[0]({ error: null, data: { data: [{ id: 12, userId: 7 }] } });
  await first;
  assert.equal(page.tasks.value.length, 0);
  assert.equal(page.loading.value, true);
  fetches[1]({ error: new Error('unavailable'), data: null });
  await latest;
  assert.equal(page.tasks.value.length, 0);
  assert.equal(page.loading.value, false);
});

test('search treats null or malformed success data as an empty result list', async () => {
  const searchSource = parse(readFileSync(new URL('./modules/search-dialog.vue', import.meta.url), 'utf8')).descriptor.scriptSetup.content;
  const code = actualFunctions(searchSource, ['search']);
  for (const data of [null, undefined, {}]) {
    const context = {
      searchRequest: 0,
      store: { token: 'token', userInfo: { id: 7 }, getIdentityVersion: () => 1 },
      loading: ref(false), list: ref([{ fileName: 'stale' }]), patterns: ref([]),
      model: ref({ userId: '7', query: 'q', topK: 10 }), request: async () => ({ error: null, data })
    };
    vm.createContext(context);
    vm.runInContext(code, context);
    await context.search();
    assert.equal(context.list.value.length, 0);
    assert.equal(context.loading.value, false);
  }
});

test('knowledge-base and search templates compile with ID-based delete and stable local row keys', () => {
  for (const file of ['./index.vue', './modules/search-dialog.vue']) {
    const { descriptor, errors } = parse(readFileSync(new URL(file, import.meta.url), 'utf8'));
    assert.equal(errors.length, 0);
    const script = compileScript(descriptor, { id: file });
    const template = compileTemplate({ source: descriptor.template.content, filename: file, id: file, compilerOptions: { bindingMetadata: script.bindings } });
    assert.deepEqual(template.errors, []);
  }
  assert.match(source, /handleDelete\(row\)/);
  assert.match(source, /disabled=\{!canDelete\(row\)\}/);
  assert.match(source, /row\.localId \|\| row\.id/);
});
