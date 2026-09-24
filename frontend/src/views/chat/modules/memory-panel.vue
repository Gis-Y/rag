<script setup lang="ts">
import { computed, onUnmounted, reactive, ref, watch } from 'vue';
import {
  NAlert,
  NButton,
  NCheckbox,
  NDatePicker,
  NDrawer,
  NDrawerContent,
  NEmpty,
  NForm,
  NFormItem,
  NInput,
  NPopconfirm,
  NSelect,
  NSpin,
  NTabPane,
  NTabs,
  NTag,
  NText
} from 'naive-ui';
import { request } from '@/service/request';
import { useAuthStore } from '@/store/modules/auth';
import { useChatStore } from '@/store/modules/chat';

interface UserMemory {
  id: number;
  scope: string;
  kind: 'preference' | 'project' | 'decision';
  key: string;
  content: string;
  keywords: string[];
  version: number;
  expires_at: string | null;
}

interface ArchiveTurn {
  id: number;
  turn_no: number;
  question: string;
  answer: string;
  created_at: string;
}

const auth = useAuthStore();
const chat = useChatStore();
const show = ref(false);
const tab = ref('memories');
const memories = ref<UserMemory[]>([]);
const archive = ref<ArchiveTurn[]>([]);
const memoryLoading = ref(false);
const archiveLoading = ref(false);
const saving = ref(false);
const memoryError = ref('');
const archiveError = ref('');
const formError = ref('');
const editorOpen = ref(false);
const after = ref(0);
const hasMore = ref(true);
const form = reactive({
  id: 0,
  version: 0,
  scope: 'global',
  kind: 'preference' as UserMemory['kind'],
  key: '',
  content: '',
  keywords: '',
  expiresAt: null as number | null,
  confirmed: false
});
const kinds = [
  { label: '回答偏好', value: 'preference' },
  { label: '项目背景', value: 'project' },
  { label: '已确认决定', value: 'decision' }
];
const mutationDisabled = computed(() => saving.value || memoryLoading.value || chat.isSending || !auth.token);
const resetNotice = '保存、修改、删除或到期会重新开始自动对话上下文，原始聊天归档仍保留。';
let generation = 0;
let observedIdentity = auth.getIdentityVersion();

function currentRequest() {
  const identity = auth.getIdentityVersion();
  const requestGeneration = generation;
  return () => Boolean(auth.token) && auth.getIdentityVersion() === identity && generation === requestGeneration;
}

function resetPanel() {
  generation += 1;
  show.value = false;
  memories.value = [];
  archive.value = [];
  editorOpen.value = false;
  Object.assign(form, {
    id: 0,
    version: 0,
    key: '',
    content: '',
    keywords: '',
    confirmed: false,
    expiresAt: null,
    scope: 'global',
    kind: 'preference'
  });
  memoryLoading.value = false;
  archiveLoading.value = false;
  saving.value = false;
  memoryError.value = '';
  archiveError.value = '';
  formError.value = '';
  after.value = 0;
  hasMore.value = true;
}

watch(
  () => [auth.token, auth.userInfo.id],
  () => {
    const identity = auth.getIdentityVersion();
    if (!auth.token || identity !== observedIdentity) resetPanel();
    observedIdentity = identity;
  },
  { flush: 'sync' }
);
onUnmounted(resetPanel);

async function openPanel() {
  if (!auth.token) return;
  show.value = true;
  // Reopening also refreshes the archive cursor so newly completed turns are visible.
  if (!archiveLoading.value) {
    archive.value = [];
    after.value = 0;
    hasMore.value = true;
  }
  await Promise.all([loadMemories(), tab.value === 'archive' ? loadArchive() : Promise.resolve()]);
}

async function loadMemories() {
  if (memoryLoading.value || !auth.token) return;
  const isCurrent = currentRequest();
  memoryLoading.value = true;
  memoryError.value = '';
  try {
    const { data, error } = await request<UserMemory[]>({ url: '/users/memories' });
    if (!isCurrent()) return;
    if (error) memoryError.value = '记忆加载失败，请重试。';
    else memories.value = data || [];
  } catch {
    if (isCurrent()) memoryError.value = '记忆加载失败，请重试。';
  } finally {
    if (isCurrent()) memoryLoading.value = false;
  }
}

async function loadArchive() {
  if (archiveLoading.value || !hasMore.value || !auth.token) return;
  const isCurrent = currentRequest();
  archiveLoading.value = true;
  archiveError.value = '';
  try {
    const { data, error } = await request<{ turns: ArchiveTurn[]; next_after: number }>({
      url: '/users/conversation/archive',
      params: { after: after.value, limit: 20 }
    });
    if (!isCurrent()) return;
    if (error) archiveError.value = '原文加载失败，请重试。';
    else {
      const turns = data.turns || [];
      archive.value.push(...turns);
      hasMore.value = turns.length === 20 && data.next_after > after.value;
      after.value = data.next_after;
    }
  } catch {
    if (isCurrent()) archiveError.value = '原文加载失败，请重试。';
  } finally {
    if (isCurrent()) archiveLoading.value = false;
  }
}

function changeTab(value: string) {
  tab.value = value;
  if (value === 'archive' && !archive.value.length) void loadArchive();
}

function editMemory(memory?: UserMemory) {
  if (mutationDisabled.value) return;
  Object.assign(form, {
    id: memory?.id || 0,
    version: memory?.version || 0,
    scope: memory?.scope || 'global',
    kind: memory?.kind || 'preference',
    key: memory?.key || '',
    content: memory?.content || '',
    keywords: memory?.keywords?.join('，') || '',
    expiresAt: memory?.expires_at ? Date.parse(memory.expires_at) : null,
    confirmed: false
  });
  formError.value = '';
  editorOpen.value = true;
}

async function saveMemory() {
  if (mutationDisabled.value) return;
  if (
    !form.confirmed ||
    (form.expiresAt !== null && (!Number.isFinite(form.expiresAt) || form.expiresAt <= Date.now()))
  ) {
    formError.value = '请确认记忆内容；到期时间必须在将来，也可留空。';
    return;
  }
  const keywords = [
    ...new Set(
      form.keywords
        .split(/[,，\n]/)
        .map(value => value.trim())
        .filter(Boolean)
    )
  ];
  const data = {
    scope: form.scope.trim(),
    kind: form.kind,
    key: form.key.trim(),
    content: form.content.trim(),
    keywords,
    expires_at: form.expiresAt === null ? null : new Date(form.expiresAt).toISOString(),
    confirmed: form.confirmed,
    ...(form.id ? { version: form.version } : {})
  };
  const size = (text: string) => Array.from(text).length;
  if (
    !data.scope ||
    size(data.scope) > 64 ||
    !data.key ||
    size(data.key) > 64 ||
    !data.content ||
    size(data.content) > 500
  ) {
    formError.value = '请填写范围、标题和内容（范围及标题最多 64 字，内容最多 500 字）。';
    return;
  }
  if (
    keywords.length > 8 ||
    keywords.some(value => size(value) > 40) ||
    (!(data.scope === 'global' && data.kind === 'preference') && !keywords.length)
  ) {
    formError.value = '项目或特定范围的记忆需要关键词；最多 8 个，每个不超过 40 字。';
    return;
  }
  const isCurrent = currentRequest();
  saving.value = true;
  formError.value = '';
  try {
    const { error } = await request({
      url: `/users/memories${form.id ? `/${form.id}` : ''}`,
      method: form.id ? 'put' : 'post',
      data
    });
    if (!isCurrent()) return;
    if (error) formError.value = '保存失败。若记忆已被其他页面修改，请取消后刷新再编辑。';
    else {
      editorOpen.value = false;
      window.$message?.success('记忆已保存，后续对话将使用新的上下文。');
      await loadMemories();
    }
  } catch {
    if (isCurrent()) formError.value = '保存失败，请重试。';
  } finally {
    if (isCurrent()) saving.value = false;
  }
}

async function deleteMemory(memory: UserMemory) {
  if (mutationDisabled.value) return;
  const isCurrent = currentRequest();
  saving.value = true;
  memoryError.value = '';
  try {
    const { error } = await request({
      url: `/users/memories/${memory.id}`,
      method: 'delete',
      params: { version: memory.version }
    });
    if (!isCurrent()) return;
    if (error) memoryError.value = '删除失败，请刷新记忆列表后重试。';
    else {
      if (form.id === memory.id) editorOpen.value = false;
      window.$message?.success('记忆已删除；原始聊天归档仍保留。');
      await loadMemories();
    }
  } catch {
    if (isCurrent()) memoryError.value = '删除失败，请重试。';
  } finally {
    if (isCurrent()) saving.value = false;
  }
}

function displayTime(value: string) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('zh-CN');
}
</script>

<template>
  <div class="flex justify-end">
    <NButton size="small" quaternary :disabled="!auth.token" @click="openPanel">记忆与历史</NButton>
  </div>
  <NDrawer v-model:show="show" :width="560" style="max-width: 94vw">
    <NDrawerContent title="记忆与历史" closable>
      <NTabs :value="tab" type="line" @update:value="changeTab">
        <NTabPane name="memories" tab="长期记忆">
          <div class="flex-col gap-4">
            <NAlert type="info" :show-icon="false">仅保存你明确确认的内容，仅供当前账号使用。{{ resetNotice }}</NAlert>
            <NText v-if="chat.isSending" type="warning">回答生成中，请等待完成后再修改记忆。</NText>
            <div class="flex items-center justify-between gap-3">
              <NText depth="3">{{ memories.length }} 条已确认记忆</NText>
              <div class="flex gap-2">
                <NButton size="small" :loading="memoryLoading" :disabled="saving" @click="loadMemories">刷新</NButton>
                <NButton size="small" type="primary" :disabled="mutationDisabled" @click="editMemory()">
                  添加记忆
                </NButton>
              </div>
            </div>
            <NAlert v-if="memoryError" type="error">{{ memoryError }}</NAlert>
            <NForm
              v-if="editorOpen"
              label-placement="top"
              :show-feedback="false"
              class="memory-editor flex-col gap-3"
              @submit.prevent="saveMemory"
            >
              <NText strong>{{ form.id ? '编辑记忆' : '新记忆' }}</NText>
              <NFormItem label="类型">
                <NSelect v-model:value="form.kind" :options="kinds" :disabled="mutationDisabled" />
              </NFormItem>
              <NFormItem label="范围（global 表示通用，否则填写项目名）">
                <NInput v-model:value="form.scope" :maxlength="64" :disabled="mutationDisabled" />
              </NFormItem>
              <NFormItem label="标题">
                <NInput
                  v-model:value="form.key"
                  :maxlength="64"
                  placeholder="例如：回答语言"
                  :disabled="mutationDisabled"
                />
              </NFormItem>
              <NFormItem label="确认内容（请勿填写密码、密钥或令牌）">
                <NInput
                  v-model:value="form.content"
                  type="textarea"
                  :maxlength="500"
                  show-count
                  :autosize="{ minRows: 3, maxRows: 8 }"
                  placeholder="例如：优先用中文回答，保留关键英文术语。"
                  :disabled="mutationDisabled"
                />
              </NFormItem>
              <NFormItem label="触发关键词（逗号分隔；通用回答偏好可留空）">
                <NInput v-model:value="form.keywords" placeholder="例如：派聪明，部署" :disabled="mutationDisabled" />
              </NFormItem>
              <NFormItem label="到期时间（留空表示不过期）">
                <NDatePicker
                  v-model:value="form.expiresAt"
                  type="datetime"
                  clearable
                  :disabled="mutationDisabled"
                  class="w-full"
                />
              </NFormItem>
              <NCheckbox v-model:checked="form.confirmed" :disabled="mutationDisabled">
                我已核对，确认将以上内容作为长期记忆
              </NCheckbox>
              <NAlert v-if="formError" type="error">{{ formError }}</NAlert>
              <div class="flex justify-end gap-2">
                <NButton :disabled="saving" @click="editorOpen = false">取消</NButton>
                <NButton
                  type="primary"
                  attr-type="submit"
                  :loading="saving"
                  :disabled="mutationDisabled || !form.confirmed"
                >
                  确认保存
                </NButton>
              </div>
            </NForm>
            <NSpin :show="memoryLoading">
              <div class="flex-col gap-3">
                <NEmpty v-if="!memoryLoading && !memoryError && !memories.length" description="尚未保存长期记忆" />
                <article v-for="memory in memories" :key="memory.id" class="memory-entry">
                  <div class="flex flex-wrap items-center gap-2">
                    <NText strong>{{ memory.key }}</NText>
                    <NTag size="small" :bordered="false">
                      {{ kinds.find(item => item.value === memory.kind)?.label }}
                    </NTag>
                  </div>
                  <p class="memory-text my-3">{{ memory.content }}</p>
                  <NText depth="3" class="text-12px">
                    范围：{{ memory.scope }} ·
                    {{ memory.expires_at ? `${displayTime(memory.expires_at)} 到期` : '不过期' }}
                  </NText>
                  <p v-if="memory.keywords?.length" class="memory-text mb-0 mt-1 text-12px">
                    关键词：{{ memory.keywords.join('、') }}
                  </p>
                  <div class="mt-2 flex justify-end gap-2">
                    <NButton size="small" quaternary :disabled="mutationDisabled" @click="editMemory(memory)">
                      编辑
                    </NButton>
                    <NPopconfirm
                      :disabled="mutationDisabled"
                      positive-text="确认删除"
                      negative-text="保留"
                      @positive-click="deleteMemory(memory)"
                    >
                      <template #trigger>
                        <NButton size="small" quaternary type="error" :disabled="mutationDisabled">删除</NButton>
                      </template>
                      删除「{{ memory.key }}」？{{ resetNotice }}
                    </NPopconfirm>
                  </div>
                </article>
              </div>
            </NSpin>
          </div>
        </NTabPane>
        <NTabPane name="archive" tab="原文归档">
          <div class="flex-col gap-4">
            <NText depth="3">按问答顺序保存已完成的原文。查看归档不会将旧内容重新加入模型上下文。</NText>
            <article v-for="turn in archive" :key="turn.id" class="memory-entry">
              <NText depth="3" class="text-12px">第 {{ turn.turn_no }} 轮 · {{ displayTime(turn.created_at) }}</NText>
              <div class="archive-label">你</div>
              <p class="memory-text m-0">{{ turn.question }}</p>
              <div class="archive-label">派聪明</div>
              <p class="memory-text m-0">{{ turn.answer }}</p>
            </article>
            <NAlert v-if="archiveError" type="error">{{ archiveError }}</NAlert>
            <NEmpty v-if="!archiveLoading && !archiveError && !archive.length" description="暂无已完成的问答归档" />
            <NButton v-if="hasMore || archiveError" :loading="archiveLoading" @click="loadArchive">
              {{ archiveError ? '重试' : '加载后续原文' }}
            </NButton>
            <NText v-else-if="archive.length" depth="3" class="text-center text-12px">已显示全部归档</NText>
          </div>
        </NTabPane>
      </NTabs>
    </NDrawerContent>
  </NDrawer>
</template>

<style scoped>
.memory-entry,
.memory-editor {
  border: 1px solid rgb(128 128 128 / 18%);
  border-radius: 8px;
  padding: 16px;
}
.memory-editor {
  border-left: 3px solid rgb(var(--primary-color));
}
.memory-text {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  line-height: 1.75;
}
.archive-label {
  margin: 16px 0 6px;
  font-size: 12px;
  font-weight: 600;
  opacity: 0.6;
}
</style>
