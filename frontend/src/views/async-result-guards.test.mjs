import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import vm from 'node:vm';
import { compileScript, compileTemplate, parse } from 'vue/compiler-sfc';
import ts from 'typescript';

function functionsFromVue(path, names) {
  const { descriptor } = parse(readFileSync(new URL(path, import.meta.url), 'utf8'));
  const source = descriptor.scriptSetup.content;
  const ast = ts.createSourceFile('actual.ts', source, ts.ScriptTarget.ES2022, true, ts.ScriptKind.TS);
  const selected = ast.statements.filter(node => ts.isFunctionDeclaration(node) && names.includes(node.name?.text));
  assert.equal(selected.length, names.length);
  return ts.transpileModule(selected.map(node => node.getText(ast)).join('\n'), {
    compilerOptions: { target: ts.ScriptTarget.ES2022 }
  }).outputText;
}

function deferredRequests() {
  const pending = [];
  return {
    pending,
    request: options => new Promise(resolve => pending.push({ options, resolve }))
  };
}

test('modified async-guard components compile as Vue SFCs', () => {
  for (const path of [
    './knowledge-base/modules/search-dialog.vue',
    './chat-history/index.vue',
    './chat/modules/chat-message.vue',
    '../components/custom/file-preview.vue'
  ]) {
    const source = readFileSync(new URL(path, import.meta.url), 'utf8');
    const { descriptor, errors } = parse(source, { filename: path });
    assert.deepEqual(errors, []);
    compileScript(descriptor, { id: path });
    const compiled = compileTemplate({ id: path, filename: path, source: descriptor.template.content });
    assert.deepEqual(compiled.errors, []);
  }
});

test('knowledge search and admin history are latest-wins and normalize null payloads', async () => {
  {
    const { pending, request } = deferredRequests();
    const context = {
      searchRequest: 0,
      store: { identity: 1, token: 'token', userInfo: { id: 7 }, getIdentityVersion() { return this.identity; } },
      model: { value: { userId: '7', query: 'old', topK: 10 } }, loading: { value: false },
      list: { value: [] }, patterns: { value: [] }, request
    };
    vm.createContext(context);
    vm.runInContext(functionsFromVue('./knowledge-base/modules/search-dialog.vue', ['search']), context);
    const oldRequest = context.search();
    context.model.value.query = 'new';
    const newRequest = context.search();
    pending[1].resolve({ error: null, data: [{ textContent: 'new' }] });
    await newRequest;
    pending[0].resolve({ error: null, data: [{ textContent: 'old' }] });
    await oldRequest;
    assert.equal(context.list.value[0].textContent, 'new');
    assert.deepEqual(Array.from(context.patterns.value), ['new']);
    const emptyRequest = context.search();
    pending[2].resolve({ error: null, data: null });
    await emptyRequest;
    assert.equal(context.list.value.length, 0);
  }

  {
    const { pending, request } = deferredRequests();
    const context = {
      historyRequest: 0,
      store: { identity: 1, token: 'token', userInfo: { id: 7 }, getIdentityVersion() { return this.identity; } },
      params: { value: { userid: 7, page: 1, size: 20 } }, loading: { value: false },
      list: { value: [] }, total: { value: 0 }, request, scrollToBottom() {}
    };
    vm.createContext(context);
    vm.runInContext(functionsFromVue('./chat-history/index.vue', ['getList']), context);
    const oldRequest = context.getList();
    context.params.value = { userid: 8, page: 1, size: 20 };
    const newRequest = context.getList();
    pending[1].resolve({ error: null, data: { content: [{ content: 'new' }], totalElements: 1 } });
    await newRequest;
    pending[0].resolve({ error: null, data: { content: [{ content: 'old' }], totalElements: 1 } });
    await oldRequest;
    assert.equal(context.list.value[0].content, 'new');
    const emptyRequest = context.getList();
    pending[2].resolve({ error: null, data: null });
    await emptyRequest;
    assert.equal(context.list.value.length, 0);
    assert.equal(context.total.value, 0);
  }
});

test('a download response from an old login cannot open a URL or report success', async () => {
  const { pending, request } = deferredRequests();
  const sideEffects = [];
  const context = {
    downloadRequest: 0,
    authStore: { identity: 1, token: 'account-a', userInfo: { id: 7 }, getIdentityVersion() { return this.identity; } },
    request,
    window: {
      open: (...args) => sideEffects.push(['open', ...args]),
      $message: {
        loading: () => ({ destroy() { sideEffects.push(['destroy']); } }),
        error: text => sideEffects.push(['error', text]),
        success: text => sideEffects.push(['success', text])
      }
    },
    URL,
    console
  };
  vm.createContext(context);
  vm.runInContext(functionsFromVue('./chat/modules/chat-message.vue', ['handleSourceFileClick', 'isSafeDownloadURL']), context);
  const source = { number: 1, documentId: 11, version: 'v1', fileName: 'same.pdf' };
  const result = context.handleSourceFileClick(source);
  context.authStore.identity += 1;
  context.authStore.token = 'account-b';
  context.authStore.userInfo.id = 8;
  pending[0].resolve({ error: null, data: { documentId: 11, version: 'v1', downloadUrl: 'https://storage.test/file' } });
  await result;
  assert.deepEqual(sideEffects, [['destroy']]);
});

test('file preview cannot click an old-account download or a non-HTTP URL', async () => {
  for (const switchedAccount of [true, false]) {
    const { pending, request } = deferredRequests();
    const sideEffects = [];
    const context = {
      downloadRequest: 0,
      props: { documentId: 11 }, loadedVersion: { value: 'v1' }, downloading: { value: false },
      authStore: { identity: 1, token: 'account-a', userInfo: { id: 7 }, getIdentityVersion() { return this.identity; } },
      request,
      window: { $message: { error: text => sideEffects.push(['error', text]), success: text => sideEffects.push(['success', text]) } },
      document: {
        createElement: () => ({ set href(value) { sideEffects.push(['href', value]); }, set download(value) { sideEffects.push(['name', value]); }, click: () => sideEffects.push(['click']) }),
        body: { appendChild: () => sideEffects.push(['append']), removeChild: () => sideEffects.push(['remove']) }
      },
      URL
    };
    vm.createContext(context);
    vm.runInContext(functionsFromVue('../components/custom/file-preview.vue', ['downloadFile', 'isSafeDownloadURL']), context);
    const result = context.downloadFile();
    if (switchedAccount) {
      context.authStore.identity += 1;
      context.authStore.token = 'account-b';
      context.authStore.userInfo.id = 8;
    }
    pending[0].resolve({
      error: null,
      data: { documentId: 11, version: 'v1', fileName: 'report.pdf', downloadUrl: switchedAccount ? 'https://storage.test/file' : 'javascript:alert(1)' }
    });
    await result;
    assert.equal(sideEffects.some(effect => ['href', 'click', 'append', 'success'].includes(effect[0])), false);
  }
});
