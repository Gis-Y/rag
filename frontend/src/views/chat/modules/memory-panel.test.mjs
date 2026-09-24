// Run with: node --test src/views/chat/modules/memory-panel.test.mjs
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { setImmediate as nextTurn } from 'node:timers/promises';
import vm from 'node:vm';
import * as vue from 'vue';
import { compileScript, compileTemplate, parse } from 'vue/compiler-sfc';
import ts from 'typescript';

const source = readFileSync(new URL('./memory-panel.vue', import.meta.url), 'utf8');
const { descriptor } = parse(source);
const code = ts.transpileModule(
  `${descriptor.scriptSetup.content}
export { openPanel, loadMemories, loadArchive, editMemory, saveMemory, deleteMemory,
  show, memories, archive, after, hasMore, form, formError, memoryError, archiveError,
  editorOpen, saving, memoryLoading, archiveLoading };`,
  {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 }
  }
).outputText;

function setup(t) {
  const scope = vue.effectScope();
  const unmount = [];
  const pending = [];
  const messages = [];
  const auth = vue.reactive({
    token: 'account-a',
    identity: 1,
    userInfo: { id: 7 },
    getIdentityVersion() {
      return this.identity;
    }
  });
  const chat = vue.reactive({ isSending: false, list: [{ content: 'current chat' }] });
  const modules = {
    vue: { ...vue, onUnmounted: callback => unmount.push(callback) },
    'naive-ui': {},
    '@/service/request': { request: options => new Promise(resolve => pending.push({ options, resolve })) },
    '@/store/modules/auth': { useAuthStore: () => auth },
    '@/store/modules/chat': { useChatStore: () => chat }
  };
  const module = { exports: {} };
  scope.run(() =>
    vm.runInNewContext(code, {
      module,
      exports: module.exports,
      require: name => modules[name],
      window: { $message: { success: message => messages.push(message) } }
    })
  );
  t.after(() => {
    unmount.forEach(callback => callback());
    scope.stop();
  });
  return {
    panel: module.exports,
    auth,
    chat,
    pending,
    messages,
    unmount,
    respond(data, error = null) {
      pending.shift().resolve({ data, error });
    }
  };
}

test('panel template compiles and archive text is never rendered as HTML', () => {
  const script = compileScript(descriptor, { id: 'memory-panel' });
  const template = compileTemplate({
    source: descriptor.template.content,
    filename: 'memory-panel.vue',
    id: 'memory-panel',
    compilerOptions: { bindingMetadata: script.bindings }
  });
  assert.deepEqual(template.errors, []);
  assert.equal(source.includes('v-html'), false);
  assert.match(source, /\{\{ turn\.question \}\}/);
  assert.match(source, /\{\{ turn\.answer \}\}/);
});

test('raw archive uses a bounded cursor and does not replace displayed chat', async t => {
  const { panel, pending, respond, chat } = setup(t);
  const turns = Array.from({ length: 20 }, (_, index) => ({
    id: index + 1,
    turn_no: index + 1,
    question: 'Q',
    answer: '<script>alert(1)</script>'
  }));
  const first = panel.loadArchive();
  assert.equal(pending[0].options.url, '/users/conversation/archive');
  assert.equal(pending[0].options.params.after, 0);
  assert.equal(pending[0].options.params.limit, 20);
  respond({ turns, next_after: 20 });
  await first;
  assert.equal(panel.archive.value.length, 20);
  assert.equal(panel.hasMore.value, true);
  const second = panel.loadArchive();
  assert.equal(pending[0].options.params.after, 20);
  respond({ turns: [{ id: 21, turn_no: 21, question: 'next', answer: 'answer' }], next_after: 21 });
  await second;
  assert.equal(panel.archive.value.length, 21);
  assert.equal(panel.hasMore.value, false);
  assert.equal(chat.list[0].content, 'current chat');
});

test('logout drops cached text and late memory/archive responses cannot enter a new account', async t => {
  const { panel, auth, respond } = setup(t);
  panel.form.content = 'private draft';
  const memory = panel.openPanel();
  const archive = panel.loadArchive();
  auth.identity += 1;
  auth.token = '';
  auth.token = 'account-b';
  auth.userInfo.id = 8;
  respond([{ id: 1, content: 'private old memory' }]);
  respond({ turns: [{ id: 1, question: 'private old question' }], next_after: 1 });
  await Promise.all([memory, archive]);
  assert.equal(panel.memories.value.length, 0);
  assert.equal(panel.archive.value.length, 0);
  assert.equal(panel.form.content, '');
  assert.equal(panel.show.value, false);
  assert.equal(panel.memoryLoading.value, false);
});

test('same-account token refresh keeps a valid pending read', async t => {
  const { panel, auth, respond } = setup(t);
  const read = panel.loadMemories();
  auth.token = 'account-a-refreshed';
  respond([{ id: 1, key: 'language', content: '中文' }]);
  await read;
  assert.equal(panel.memories.value[0].content, '中文');
});

test('create requires confirmation, future expiry and scope keywords; completed save leaves chat intact', async t => {
  const { panel, chat, pending, respond } = setup(t);
  panel.editMemory();
  Object.assign(panel.form, { key: '部署', content: '仅本地部署', kind: 'project', scope: 'paismart' });
  await panel.saveMemory();
  assert.equal(pending.length, 0);
  assert.match(panel.formError.value, /确认/);
  panel.form.confirmed = true;
  await panel.saveMemory();
  assert.match(panel.formError.value, /关键词/);
  panel.form.keywords = '派聪明，部署,部署';
  panel.form.expiresAt = NaN;
  await panel.saveMemory();
  assert.equal(pending.length, 0);
  panel.form.expiresAt = null;
  const save = panel.saveMemory();
  assert.equal(pending[0].options.method, 'post');
  assert.equal(pending[0].options.data.confirmed, true);
  assert.equal(pending[0].options.data.keywords.join(','), '派聪明,部署');
  respond({ id: 5 });
  await nextTurn();
  respond([{ id: 5, key: '部署', content: '仅本地部署' }]);
  await save;
  assert.equal(panel.editorOpen.value, false);
  assert.equal(panel.memories.value[0].id, 5);
  assert.equal(chat.list[0].content, 'current chat');
});

test('edit and delete use versions; mutations are blocked during generation', async t => {
  const { panel, chat, pending, respond } = setup(t);
  const memory = {
    id: 5,
    version: 3,
    scope: 'global',
    kind: 'preference',
    key: '语言',
    content: '中文',
    keywords: [],
    expires_at: null
  };
  panel.editMemory(memory);
  assert.equal(panel.form.confirmed, false);
  panel.form.confirmed = true;
  chat.isSending = true;
  await panel.saveMemory();
  await panel.deleteMemory(memory);
  assert.equal(pending.length, 0);
  chat.isSending = false;
  const save = panel.saveMemory();
  assert.equal(pending[0].options.method, 'put');
  assert.equal(pending[0].options.url, '/users/memories/5');
  assert.equal(pending[0].options.data.version, 3);
  respond(null, new Error('conflict'));
  await save;
  assert.match(panel.formError.value, /刷新/);
  const remove = panel.deleteMemory(memory);
  assert.equal(pending[0].options.method, 'delete');
  assert.equal(pending[0].options.params.version, 3);
  respond(null);
  await nextTurn();
  respond([]);
  await remove;
  assert.equal(panel.memories.value.length, 0);
  assert.equal(chat.list[0].content, 'current chat');
});

test('read errors preserve cursor and allow retries; unmount rejects late results', async t => {
  const { panel, respond, unmount } = setup(t);
  const failed = panel.loadArchive();
  respond(null, new Error('offline'));
  await failed;
  assert.match(panel.archiveError.value, /重试/);
  assert.equal(panel.after.value, 0);
  const retry = panel.loadArchive();
  unmount.forEach(callback => callback());
  respond({ turns: [{ id: 1, question: 'late' }], next_after: 1 });
  await retry;
  assert.equal(panel.archive.value.length, 0);
  assert.equal(panel.archiveLoading.value, false);
});
