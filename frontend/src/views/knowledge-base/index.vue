<script setup lang="tsx">
import type { UploadFileInfo } from 'naive-ui';
import { NButton, NEllipsis, NModal, NPopconfirm, NProgress, NTag, NUpload } from 'naive-ui';
import { uploadAccept } from '@/constants/common';
import { fakePaginationRequest } from '@/service/request';
import { UploadStatus } from '@/enum';
import SvgIcon from '@/components/custom/svg-icon.vue';
import FilePreview from '@/components/custom/file-preview.vue';
import UploadDialog from './modules/upload-dialog.vue';
import SearchDialog from './modules/search-dialog.vue';

const appStore = useAppStore();

// 文件预览相关状态
const previewVisible = ref(false);
const previewFileName = ref('');
const previewDocumentId = ref(0);
const authStore = useAuthStore();
const loading = ref(false);
let listRequest = 0;
let listIdentity = authStore.getIdentityVersion();
watch(() => [authStore.token, authStore.userInfo.id], () => {
  const identity = authStore.getIdentityVersion();
  if (!authStore.token || identity !== listIdentity) {
    listRequest += 1;
    loading.value = false;
  }
  listIdentity = identity;
}, { flush: 'sync' });
onUnmounted(() => { listRequest += 1; });

function apiFn() {
  return fakePaginationRequest<Api.KnowledgeBase.List>({ url: '/documents/accessible' });
}

function renderIcon(fileName: string) {
  const ext = getFileExt(fileName);
  if (ext) {
    if (uploadAccept.split(',').includes(`.${ext}`)) return <SvgIcon localIcon={ext} class="mx-4 text-12" />;
    return <SvgIcon localIcon="dflt" class="mx-4 text-12" />;
  }
  return null;
}

// 处理文件预览
function handleFilePreview(file: Api.KnowledgeBase.UploadTask) {
  if (!file.id) {
    window.$message?.warning('文档尚无可用身份，请完成上传并刷新列表');
    return;
  }
  previewDocumentId.value = file.id;
  previewFileName.value = file.fileName;
  previewVisible.value = true;
}

// 关闭文件预览
function closeFilePreview() {
  previewVisible.value = false;
  previewFileName.value = '';
  previewDocumentId.value = 0;
}

const { columns, columnChecks } = useTable({
  apiFn,
  immediate: false,
  columns: () => [
    {
      key: 'fileName',
      title: '文件名',
      minWidth: 400,
      render: row => (
        <div class="flex items-center">
          {renderIcon(row.fileName)}
          <NEllipsis lineClamp={2} tooltip>
            <span
              class="cursor-pointer hover:text-primary transition-colors"
              onClick={() => handleFilePreview(row)}
            >
              {row.fileName}
            </span>
          </NEllipsis>
        </div>
      )
    },
    {
      key: 'totalSize',
      title: '文件大小',
      width: 100,
      render: row => fileSize(row.totalSize)
    },
    {
      key: 'status',
      title: '上传状态',
      width: 100,
      render: row => renderStatus(row.status, row.progress)
    },
    {
      key: 'orgTagName',
      title: '组织标签',
      width: 150,
      ellipsis: { tooltip: true, lineClamp: 2 }
    },
    {
      key: 'isPublic',
      title: '是否公开',
      width: 100,
      render: row => (row.public || row.isPublic ? <NTag type="success">公开</NTag> : <NTag type="warning">私有</NTag>)
    },
    {
      key: 'createdAt',
      title: '上传时间',
      width: 100,
      render: row => dayjs(row.createdAt).format('YYYY-MM-DD')
    },
    {
      key: 'operate',
      title: '操作',
      width: 180,
      render: row => (
        <div class="flex gap-4">
          {renderResumeUploadButton(row)}
          <NButton
            type="primary"
            ghost
            size="small"
            disabled={!row.id || row.status !== UploadStatus.Completed}
            onClick={() => handleFilePreview(row)}
          >
            预览
          </NButton>
          <NPopconfirm onPositiveClick={() => handleDelete(row)}>
            {{
              default: () => row.id ? '确认删除当前文件吗？' : '确认取消此本地上传任务吗？',
              trigger: () => (
                <NButton type="error" ghost size="small" disabled={!canDelete(row)}>
                  {row.id ? '删除' : '取消上传'}
                </NButton>
              )
            }}
          </NPopconfirm>
        </div>
      )
    }
  ]
});

const store = useKnowledgeBaseStore();
const { tasks } = storeToRefs(store);
onMounted(async () => {
  await getList();
});

/** Only the latest request may reconcile server documents with live local uploads. */
async function getList() {
  const current = ++listRequest;
  const userId = authStore.userInfo.id;
  const identity = authStore.getIdentityVersion();
  if (!authStore.token || !userId) {
    loading.value = false;
    return;
  }
  loading.value = true;
  try {
    // Use this request's result, never useHookTable's shared data (older fetches can overwrite it).
    const { data, error } = await apiFn();
    if (current !== listRequest || !authStore.token || identity !== authStore.getIdentityVersion() || userId !== authStore.userInfo.id) return;
    if (error || !Array.isArray(data?.data)) return;
    const localTasks = tasks.value.filter(task => task.localId && (!task.id || task.status !== UploadStatus.Completed));
    const serverRows = data.data.map(item => {
      // MD5 only reconciles this owner's local upload; published documents use SQL ID.
      const existing = tasks.value.find(task => Boolean(item.id) && (task.id === item.id ||
        (!task.id && task.localId && task.userId === item.userId && item.userId === userId && task.fileMd5 === item.fileMd5)));
      return existing ? Object.assign(existing, item) : item;
    });
    // Preserve live objects: in-flight callbacks still reference these upload tasks.
    tasks.value = [...localTasks.filter(task => !serverRows.includes(task)), ...serverRows];
  } finally {
    if (current === listRequest) loading.value = false;
  }
}

function canDelete(row: Api.KnowledgeBase.UploadTask) {
  return Boolean(authStore.token) && authStore.userInfo.id > 0 && Boolean(row.id || row.localId) &&
    (row.userId === authStore.userInfo.id || (Boolean(row.id) && authStore.userInfo.role === 'ADMIN'));
}

async function handleDelete(row: Api.KnowledgeBase.UploadTask) {
  if (!canDelete(row)) return;
  const identity = authStore.getIdentityVersion();
  const userId = authStore.userInfo.id;
  listRequest += 1; // A pre-delete snapshot must never resurrect the deleted row.
  loading.value = false;
  if (!row.id) {
    // No SQL identity exists yet: cancel only this local task, never guess a remote document by MD5.
    tasks.value = tasks.value.filter(task => task.localId !== row.localId);
    row.requestIds?.forEach(requestId => request.cancelRequest(requestId));
    return;
  }

  const documentId = row.id;
  const { error } = await request({ url: `/documents/${documentId}`, method: 'DELETE' });
  if (!authStore.token || identity !== authStore.getIdentityVersion() || userId !== authStore.userInfo.id) return;
  if (!error) {
    tasks.value = tasks.value.filter(task => task.id !== documentId);
    window.$message?.success('删除成功');
    await getList();
  }
}

// #region 文件上传
const uploadVisible = ref(false);
function handleUpload() {
  uploadVisible.value = true;
}
// #endregion

// #region 检索知识库
const searchVisible = ref(false);
function handleSearch() {
  searchVisible.value = true;
}
// #endregion

// 渲染上传状态
function renderStatus(status: UploadStatus, percentage: number) {
  if (status === UploadStatus.Completed) return <NTag type="success">已完成</NTag>;
  else if (status === UploadStatus.Break) return <NTag type="error">上传中断</NTag>;
  return <NProgress percentage={percentage} processing />;
}

// #region 文件续传
function renderResumeUploadButton(row: Api.KnowledgeBase.UploadTask) {
  if (row.status === UploadStatus.Break) {
    if (row.file)
      return (
        <NButton type="primary" size="small" ghost onClick={() => resumeUpload(row)}>
          续传
        </NButton>
      );
    return (
      <NUpload
        show-file-list={false}
        default-upload={false}
        accept={uploadAccept}
        onBeforeUpload={options => onBeforeUpload(options, row)}
        class="w-fit"
      >
        <NButton type="primary" size="small" ghost>
          续传
        </NButton>
      </NUpload>
    );
  }
  return null;
}

// 任务列表存在文件，直接续传
function resumeUpload(row: Api.KnowledgeBase.UploadTask) {
  row.status = UploadStatus.Pending;
  store.startUpload();
}

async function onBeforeUpload(
  options: { file: UploadFileInfo; fileList: UploadFileInfo[] },
  row: Api.KnowledgeBase.UploadTask
) {
  const identity = authStore.getIdentityVersion();
  const userId = authStore.userInfo.id;
  if (!authStore.token || row.userId !== userId) return false;
  const md5 = await calculateMD5(options.file.file!);
  if (!authStore.token || identity !== authStore.getIdentityVersion() || userId !== authStore.userInfo.id) return false;
  if (md5 !== row.fileMd5) {
    window.$message?.error('两次上传的文件不一致');
    return false;
  }
  loading.value = true;
  const { error, data: progress } = await request<Api.KnowledgeBase.Progress>({
    url: '/upload/status',
    params: { file_md5: row.fileMd5 }
  });
  if (!authStore.token || identity !== authStore.getIdentityVersion() || userId !== authStore.userInfo.id) return false;
  if (!error) {
    row.file = options.file.file!;
    row.status = UploadStatus.Pending;
    row.progress = progress.progress;
    row.uploadedChunks = progress.uploaded;
    store.startUpload();
    loading.value = false;
    return true;
  }
  loading.value = false;
  return false;
}
</script>

<template>
  <div class="min-h-500px flex-col-stretch gap-16px overflow-hidden lt-sm:overflow-auto">
    <NCard title="文件列表" :bordered="false" size="small" class="sm:flex-1-hidden card-wrapper">
      <template #header-extra>
        <TableHeaderOperation v-model:columns="columnChecks" :loading="loading" @add="handleUpload" @refresh="getList">
          <template #prefix>
            <NButton size="small" ghost type="primary" @click="handleSearch">
              <template #icon>
                <icon-ic-round-search class="text-icon" />
              </template>
              检索知识库
            </NButton>
          </template>
        </TableHeaderOperation>
      </template>
      <NDataTable
        striped
        :columns="columns"
        :data="tasks"
        size="small"
        :flex-height="!appStore.isMobile"
        :scroll-x="962"
        :loading="loading"
        remote
        :row-key="row => row.localId || row.id"
        :pagination="false"
        class="sm:h-full"
      />
    </NCard>
    <UploadDialog v-model:visible="uploadVisible" />
    <SearchDialog v-model:visible="searchVisible" />
    
    <!-- 文件预览弹窗 -->
    <NModal v-model:show="previewVisible" preset="card" title="文件预览" style="width: 80%; max-width: 1000px;">
      <FilePreview
        :document-id="previewDocumentId"
        :file-name="previewFileName"
        :visible="previewVisible"
        @close="closeFilePreview"
      />
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.file-list-container {
  transition: width 0.3s ease;
}

:deep() {
  .n-progress-icon.n-progress-icon--as-text {
    white-space: nowrap;
  }
}
</style>
