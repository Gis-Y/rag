<script setup lang="ts">
import { VueMarkdownIt } from 'vue-markdown-shiki';
import { formatDate } from '@/utils/common';
import { renderSourceCitations, sourceCitations } from '@/utils/source-citations';
defineOptions({ name: 'ChatMessage' });

const props = defineProps<{ msg: Api.Chat.Message }>();

const authStore = useAuthStore();
let downloadRequest = 0;

onBeforeUnmount(() => {
  downloadRequest += 1;
});

function handleCopy(content: string) {
  navigator.clipboard.writeText(content);
  window.$message?.success('已复制');
}

const chatStore = useChatStore();

const sources = computed(() => sourceCitations(props.msg.sources));

const content = computed(() => {
  chatStore.scrollToBottom?.();
  const rawContent = props.msg.content ?? '';

  // 只对助手消息处理来源链接
  if (props.msg.role === 'assistant') {
    return renderSourceCitations(rawContent, sources.value);
  }

  return rawContent;
});

// 处理内容点击事件（事件委托）
function handleContentClick(event: MouseEvent) {
  const target = event.target instanceof Element ? event.target.closest('a') : null;
  const match = /^#source-([1-9]\d*)$/.exec(target?.getAttribute('href') ?? '');
  if (!match) return;
  event.preventDefault();
  const number = Number(match[1]);
  const source = sources.value.find(item => item.number === number);
  if (source) void handleSourceFileClick(source);
}

// 处理来源文件点击事件
async function handleSourceFileClick(source: Api.Chat.SourceCitation) {
  const current = ++downloadRequest;
  const identity = authStore.getIdentityVersion();
  const token = authStore.token;
  const userId = authStore.userInfo.id;
  const isCurrent = () =>
    current === downloadRequest &&
    identity === authStore.getIdentityVersion() &&
    token === authStore.token &&
    userId === authStore.userInfo.id;
  const loadingMessage = window.$message?.loading(`正在获取文件下载链接: ${source.fileName}`, {
    duration: 0,
    closable: false
  });
  try {
    // 调用文件下载接口
    const { error, data } = await request<Api.Document.DownloadResponse>({
      url: '/documents/download',
      params: {
        documentId: source.documentId,
        version: source.version
      }
    });

    if (!isCurrent()) return;

    if (error) {
      window.$message?.error(`文件下载失败: ${error.response?.data?.message || '未知错误'}`);
      return;
    }

    if (
      data?.documentId === source.documentId &&
      data.version === source.version &&
      isSafeDownloadURL(data.downloadUrl)
    ) {
      // 在新窗口打开下载链接
      window.open(data.downloadUrl, '_blank', 'noopener,noreferrer');
      window.$message?.success(`文件下载链接已打开: ${source.fileName}`);
    } else {
      window.$message?.error('未能获取到下载链接');
    }
  } catch (err) {
    if (!isCurrent()) return;
    console.error('文件下载失败:', err);
    window.$message?.error(`文件下载失败: ${source.fileName}`);
  } finally {
    loadingMessage?.destroy();
  }
}

function isSafeDownloadURL(value: unknown) {
  if (typeof value !== 'string') return false;
  try {
    const protocol = new URL(value).protocol;
    return protocol === 'http:' || protocol === 'https:';
  } catch {
    return false;
  }
}
</script>

<template>
  <div class="mb-8 flex-col gap-2">
    <div v-if="msg.role === 'user'" class="flex items-center gap-4">
      <NAvatar class="bg-success">
        <SvgIcon icon="ph:user-circle" class="text-icon-large color-white" />
      </NAvatar>
      <div class="flex-col gap-1">
        <NText class="text-4 font-bold">{{ msg.username || authStore.userInfo.username }}</NText>
        <NText class="text-3 color-gray-500">{{ formatDate(msg.timestamp) }}</NText>
      </div>
    </div>
    <div v-else class="flex items-center gap-4">
      <NAvatar class="bg-primary">
        <SystemLogo class="text-6 text-white" />
      </NAvatar>
      <div class="flex-col gap-1">
        <NText class="text-4 font-bold">派聪明</NText>
        <NText class="text-3 color-gray-500">{{ formatDate(msg.timestamp) }}</NText>
      </div>
    </div>
    <NText v-if="msg.status === 'pending'">
      <icon-eos-icons:three-dots-loading class="ml-12 mt-2 text-8" />
    </NText>
    <div v-else-if="msg.role === 'assistant'" class="mt-2 pl-12" @click="handleContentClick">
      <VueMarkdownIt :content="content" />
      <NText v-if="!sources.length && /来源#/.test(msg.content)" depth="3" class="text-3">
        此记录未保存可验证的来源身份，引用不可打开。
      </NText>
    </div>
    <NText v-else-if="msg.role === 'user'" class="ml-12 mt-2 text-4">{{ content }}</NText>
    <NText v-if="msg.status === 'error'" class="ml-12 mt-2 italic">
      回答已中断，内容可能不完整，请稍后重试
    </NText>
    <NDivider class="ml-12 w-[calc(100%-3rem)] mb-0! mt-2!" />
    <div class="ml-12 flex gap-4">
      <NButton quaternary @click="handleCopy(msg.content)">
        <template #icon>
          <icon-mynaui:copy />
        </template>
      </NButton>
    </div>
  </div>
</template>

<style scoped lang="scss">
:deep(a[href^='#source-']) {
  color: #1890ff;
  cursor: pointer;
  text-decoration: underline;
  transition: color 0.2s;

  &:hover {
    color: #40a9ff;
    text-decoration: none;
  }

  &:active {
    color: #096dd9;
  }
}
</style>
