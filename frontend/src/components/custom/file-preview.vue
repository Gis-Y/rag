<template>
  <div class="file-preview-container">
    <!-- 预览头部 -->
    <div class="preview-header">
      <div class="flex items-center gap-2">
        <SvgIcon :local-icon="getFileIcon(fileName)" class="text-16" />
        <span class="font-medium">{{ fileName }}</span>
      </div>
      <div class="flex items-center gap-2">
        <NButton size="small" @click="downloadFile" :loading="downloading" :disabled="!loadedVersion || loading">
          <template #icon>
            <icon-mdi-download />
          </template>
          下载
        </NButton>
        <NButton size="small" @click="closePreview">
          <template #icon>
            <icon-mdi-close />
          </template>
        </NButton>
      </div>
    </div>
    
    <!-- 预览内容 -->
    <div class="preview-content">
      <template v-if="loading">
        <div class="flex items-center justify-center h-full">
          <NSpin size="large" />
        </div>
      </template>
      <template v-else-if="error">
        <div class="flex flex-col items-center justify-center h-full text-gray-500">
          <icon-mdi-alert-circle class="text-48 mb-4" />
          <p>{{ error }}</p>
        </div>
      </template>
      <template v-else>
        <div class="content-wrapper">
          <pre class="preview-text">{{ content }}</pre>
        </div>
      </template>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue';
import { NButton, NSpin } from 'naive-ui';
import SvgIcon from '@/components/custom/svg-icon.vue';
import { request } from '@/service/request';
import { getFileExt } from '@/utils/common';

interface Props {
  documentId: number;
  version?: string;
  fileName: string;
  visible: boolean;
}

interface Emits {
  (e: 'close'): void;
}

const props = defineProps<Props>();
const emit = defineEmits<Emits>();
const authStore = useAuthStore();

const loading = ref(false);
const downloading = ref(false);
const content = ref('');
const error = ref('');
const loadedVersion = ref('');
let previewRequest = 0;
let downloadRequest = 0;

// 获取文件图标
function getFileIcon(fileName: string) {
  const ext = getFileExt(fileName);
  if (ext) {
    const supportedIcons = ['pdf', 'doc', 'docx', 'txt', 'md', 'jpg', 'jpeg', 'png', 'gif'];
    return supportedIcons.includes(ext.toLowerCase()) ? ext : 'dflt';
  }
  return 'dflt';
}

// Identity, not the display name, determines which document to load.
watch(() => [props.documentId, props.version, props.visible, authStore.token, authStore.userInfo.id], () => {
  previewRequest += 1;
  downloadRequest += 1;
  downloading.value = false;
  loadedVersion.value = '';
  content.value = '';
  if (props.visible) void loadPreviewContent();
}, { immediate: true });
onBeforeUnmount(() => {
  previewRequest += 1;
  downloadRequest += 1;
});

// 加载预览内容
async function loadPreviewContent() {
  if (!Number.isSafeInteger(props.documentId) || props.documentId <= 0) {
    loading.value = false;
    error.value = '缺少文档身份，无法预览';
    return;
  }
  const current = previewRequest;
  const documentId = props.documentId;
  const version = props.version;
  loading.value = true;
  error.value = '';
  content.value = '';
  
  try {
    const { error: requestError, data } = await request<{
      documentId: number;
      version: string;
      fileName: string;
      content: string;
      fileSize: number;
    }>({
      url: '/documents/preview',
      params: {
        documentId,
        version
      }
    });
    if (current !== previewRequest) return;
    
    if (requestError) {
      error.value = '预览失败：' + (requestError.message || '未知错误');
    } else if (data?.documentId === documentId && data.version && (!version || data.version === version)) {
      content.value = data.content;
      loadedVersion.value = data.version;
    } else {
      error.value = '预览失败：文档身份或版本不匹配';
    }
  } catch (err: any) {
    if (current === previewRequest) error.value = '预览失败：' + (err.message || '网络错误');
  } finally {
    if (current === previewRequest) loading.value = false;
  }
}

// 下载文件
async function downloadFile() {
  if (!props.documentId || !loadedVersion.value) return;

  const current = ++downloadRequest;
  const identity = authStore.getIdentityVersion();
  const token = authStore.token;
  const userId = authStore.userInfo.id;
  const documentId = props.documentId;
  const version = loadedVersion.value;
  const isCurrent = () =>
    current === downloadRequest &&
    identity === authStore.getIdentityVersion() &&
    token === authStore.token &&
    userId === authStore.userInfo.id &&
    documentId === props.documentId &&
    version === loadedVersion.value;
  downloading.value = true;
  
  try {
    const { error: requestError, data } = await request<Api.Document.DownloadResponse>({
      url: '/documents/download',
      params: {
        documentId,
        version
      }
    });

    if (!isCurrent()) return;
    if (requestError) {
      window.$message?.error('下载失败：' + (requestError.message || '未知错误'));
    } else if (data?.documentId === documentId && data.version === version && isSafeDownloadURL(data.downloadUrl)) {
      // 使用预签名URL下载文件
      const link = document.createElement('a');
      link.href = data.downloadUrl;
      link.download = data.fileName;
      document.body.appendChild(link);
      link.click();
      document.body.removeChild(link);
      window.$message?.success('开始下载文件');
    }
  } catch (err: any) {
    if (isCurrent()) window.$message?.error('下载失败：' + (err.message || '网络错误'));
  } finally {
    if (current === downloadRequest) downloading.value = false;
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

// 关闭预览
function closePreview() {
  emit('close');
}
</script>

<style scoped lang="scss">
.file-preview-container {
  @apply h-full flex flex-col bg-white border-l border-gray-200;
  
  .preview-header {
    @apply flex items-center justify-between p-4 border-b border-gray-200 bg-gray-50;
  }
  
  .preview-content {
    @apply flex-1 overflow-hidden;
    
    .content-wrapper {
      @apply h-full overflow-auto p-4;
    }
    
    .preview-text {
      @apply text-sm font-mono whitespace-pre-wrap break-words;
      font-family: 'Monaco', 'Menlo', 'Ubuntu Mono', monospace;
      line-height: 1.5;
      margin: 0;
    }
  }
}
</style>
