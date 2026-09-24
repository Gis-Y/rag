import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import vm from 'node:vm';
import MarkdownIt from 'markdown-it';
import ts from 'typescript';

const module = { exports: {} };
vm.runInNewContext(ts.transpileModule(readFileSync(new URL('./source-citations.ts', import.meta.url), 'utf8'), {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 }
}).outputText, { module, exports: module.exports });
const { sourceCitations, renderSourceCitations, chatMarkdownOptions } = module.exports;
const markdown = new MarkdownIt(chatMarkdownOptions);

test('same names bind different source numbers and display names remain literal after Markdown rendering', () => {
  const sources = [
    { number: 1, documentId: 5, version: 'v1', fileName: '报告(最终).pdf' },
    { number: 2, documentId: 6, version: 'v2', fileName: '报告(最终).pdf' },
    { number: 3, documentId: 7, version: 'v1', fileName: '<img src=x onerror="bad()">.pdf' }
  ];
  const rendered = markdown.render(renderSourceCitations('内容 [来源#1] [来源#2] [来源#3] [来源#99]', sources));
  assert.match(rendered, /href="#source-1"/);
  assert.match(rendered, /href="#source-2"/);
  assert.match(rendered, /报告\(最终\)\.pdf/);
  assert.match(rendered, /&lt;img src=x onerror=&quot;bad\(\)&quot;&gt;\.pdf/);
  assert.doesNotMatch(rendered, /href="#source-99"|<img/);
  assert.equal(renderSourceCitations('内容 [来源#1]', []), '内容 [来源#1]');
  assert.equal(renderSourceCitations('(来源#1: 报告.pdf)', sources), '(来源#1: 报告.pdf)');
});

test('Markdown images, links and attributes in filenames cannot inject elements or event handlers', () => {
  const names = ['![x](x){onerror=alert(1)}.pdf', '[x](https://example.test){onclick=alert(1)}.pdf',
    '文档[终稿]_(一) & &#60; > " \\ ` ! {x} 😀.pdf'];
  const sources = names.map((fileName, i) => ({ number: i + 1, documentId: i + 1, version: 'v1', fileName }));
  const rendered = markdown.render(renderSourceCitations('[来源#1] [来源#2] [来源#3]', sources));
  assert.equal((rendered.match(/<a /g) || []).length, names.length);
  assert.doesNotMatch(rendered, /<(?:img|script|iframe|button)\b|<[^>]+\son\w+\s*=/i);
  for (let i = 0; i < names.length; i++) {
    assert.match(rendered, new RegExp(`href="#source-${i + 1}"`));
  }
  const texts = markdown.parse(renderSourceCitations('[来源#1] [来源#2] [来源#3]', sources), {})[1].children
    .filter(token => token.type === 'text').map(token => token.content).join('');
  for (const name of names) assert.ok(texts.includes(name));
});

test('chat provider disables raw HTML and attribute syntax for assistant output', () => {
  assert.equal(chatMarkdownOptions.html, false);
  assert.equal(chatMarkdownOptions.attrs.disable, true);
  const provider = readFileSync(new URL('../views/chat/modules/chat-list.vue', import.meta.url), 'utf8');
  assert.match(provider, /<VueMarkdownItProvider :options="chatMarkdownOptions">/);
  const historyProvider = readFileSync(new URL('../views/chat-history/index.vue', import.meta.url), 'utf8');
  assert.match(historyProvider, /<VueMarkdownItProvider :options="chatMarkdownOptions">/);
  const raw = '<img src=x onerror=alert(1)>\n\n![x](x){onerror=alert(1)}\n\n[x](#source-1){onclick=alert(1)}';
  const rendered = markdown.render(renderSourceCitations(raw, []));
  assert.match(rendered, /&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.doesNotMatch(rendered, /<[^>]+\son\w+\s*=/i);
  assert.match(rendered, /\{onerror=alert\(1\)\}/);
});

test('history and frames with missing, ambiguous or invalid identities cannot make clickable sources', () => {
  const valid = { number: 1, documentId: 5, version: 'v1', fileName: '报告.pdf' };
  for (const value of [undefined, null, {}, [{ fileName: '报告.pdf' }], [{ ...valid, documentId: 0 }],
    [{ ...valid, version: '' }], [{ ...valid, number: 25 }], [valid, valid]]) {
    assert.equal(sourceCitations(value).length, 0);
  }
  const restored = JSON.parse(JSON.stringify([valid]));
  assert.equal(sourceCitations(restored)[0].documentId, 5);
});
