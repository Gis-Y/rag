import { REQUEST_ID_KEY } from '~/packages/axios/src';
import { nanoid } from '~/packages/utils/src';

export const useKnowledgeBaseStore = defineStore(SetupStoreId.KnowledgeBase, () => {
  const tasks = ref<Api.KnowledgeBase.UploadTask[]>([]);
  const activeUploads = ref<Set<string>>(new Set());
  const authStore = useAuthStore();
  let observedIdentity = authStore.getIdentityVersion();
  let observedUser = authStore.userInfo.id;

  watch(() => [authStore.token, authStore.userInfo.id], () => {
    const identity = authStore.getIdentityVersion();
    if (!authStore.token || identity !== observedIdentity || authStore.userInfo.id !== observedUser) {
      const staleTasks = tasks.value;
      tasks.value = [];
      activeUploads.value.clear();
      staleTasks.forEach(task => task.requestIds?.forEach(id => request.cancelRequest(id)));
    }
    observedIdentity = identity;
    observedUser = authStore.userInfo.id;
  }, { flush: 'sync' });

  function currentTask(task: Api.KnowledgeBase.UploadTask, identity: number) {
    if (!authStore.token || authStore.getIdentityVersion() !== identity || task.userId !== authStore.userInfo.id) return undefined;
    return tasks.value.find(item => task.localId ? item.localId === task.localId : Boolean(task.id) && item.id === task.id);
  }

  async function uploadChunk(task: Api.KnowledgeBase.UploadTask, identity: number): Promise<boolean> {
    if (!currentTask(task, identity)) return false;
    const totalChunks = Math.ceil(task.totalSize / chunkSize);

    const chunkStart = task.chunkIndex * chunkSize;
    const chunkEnd = Math.min(chunkStart + chunkSize, task.totalSize);
    const chunk = task.file.slice(chunkStart, chunkEnd);

    task.chunk = chunk;
    const requestId = nanoid();
    task.requestIds ??= [];
    task.requestIds.push(requestId);
    const { error, data } = await request<Api.KnowledgeBase.Progress>({
      url: '/upload/chunk',
      method: 'POST',
      data: {
        file: task.chunk,
        fileMd5: task.fileMd5,
        chunkIndex: task.chunkIndex,
        totalSize: task.totalSize,
        fileName: task.fileName,
        orgTag: task.orgTag,
        isPublic: task.isPublic ?? false
      },
      headers: {
        'Content-Type': 'multipart/form-data',
        [REQUEST_ID_KEY]: requestId
      },
      timeout: 10 * 60 * 1000
    });

    task.requestIds = task.requestIds.filter(id => id !== requestId);

    if (error || !currentTask(task, identity)) return false;

    // 更新任务状态
    const updatedTask = currentTask(task, identity)!;
    updatedTask.uploadedChunks = data.uploaded;
    updatedTask.progress = Number.parseFloat(data.progress.toFixed(2));

    if (data.uploaded.length === totalChunks) {
      const success = await mergeFile(task, identity);
      if (!success) return false;
    }
    return true;
  }

  async function mergeFile(task: Api.KnowledgeBase.UploadTask, identity: number) {
    if (!currentTask(task, identity)) return false;
    const requestId = nanoid();
    task.requestIds ??= [];
    task.requestIds.push(requestId);
    try {
      const { error } = await request({
        url: '/upload/merge',
        method: 'POST',
        data: { fileMd5: task.fileMd5, fileName: task.fileName },
        headers: { [REQUEST_ID_KEY]: requestId }
      });
      if (error) return false;

      // 更新任务状态为已完成
      const updatedTask = currentTask(task, identity);
      if (!updatedTask) return false;
      updatedTask.status = UploadStatus.Completed;
      return true;
    } catch {
      return false;
    } finally {
      task.requestIds = task.requestIds.filter(id => id !== requestId);
    }
  }

  /**
   * 异步函数：将上传请求加入队列
   *
   * 本函数处理上传任务的排队和初始化工作它首先检查是否存在相同的文件， 如果不存在，则创建一个新的上传任务，并将其添加到任务队列中最后启动上传流程
   *
   * @param form 包含上传信息的表单，包括文件列表和是否公开的标签
   * @returns 返回一个上传任务对象，无论是已存在的还是新创建的
   */
  async function enqueueUpload(form: Api.KnowledgeBase.Form) {
    const userId = authStore.userInfo.id;
    const identity = authStore.getIdentityVersion();
    if (!authStore.token || !userId) return;
    // 获取文件列表中的第一个文件
    const file = form.fileList![0].file!;
    const { data: limits, error } = await request<{ maxFileBytes: number }>({ url: '/upload/supported-types' });
    if (!authStore.token || identity !== authStore.getIdentityVersion() || userId !== authStore.userInfo.id) return;
    if (error) return;
    if (!limits || !Number.isSafeInteger(limits.maxFileBytes) || limits.maxFileBytes <= 0) {
      window.$message?.error('无法获取文档上传大小限制');
      return;
    }
    if (file.size <= 0 || file.size > limits.maxFileBytes) {
      window.$message?.error(`文件不能为空，且不能超过 ${(limits.maxFileBytes / 1024 / 1024).toFixed(1)} MiB`);
      return;
    }
    // 计算文件的MD5值，用于唯一标识文件
    const md5 = await calculateMD5(file);
    if (!authStore.token || identity !== authStore.getIdentityVersion() || userId !== authStore.userInfo.id) return;

    // 检查是否已存在相同文件
    const existingTask = tasks.value.find(t => t.userId === userId && t.fileMd5 === md5);
    if (existingTask) {
      // 如果存在相同文件，直接返回该上传任务
      if (existingTask.status === UploadStatus.Completed) {
        window.$message?.error('文件已存在');
        return;
      } else if (existingTask.status === UploadStatus.Pending || existingTask.status === UploadStatus.Uploading) {
        window.$message?.error('文件正在上传中');
        return;
      } else if (existingTask.status === UploadStatus.Break) {
        existingTask.file = file;
        existingTask.localId ??= nanoid();
        existingTask.status = UploadStatus.Pending;
        startUpload();
        return;
      }
    }

    // 创建新的上传任务对象
    const newTask: Api.KnowledgeBase.UploadTask = {
      localId: nanoid(),
      userId,
      file,
      chunk: null,
      chunkIndex: 0,
      fileMd5: md5,
      fileName: file.name,
      totalSize: file.size,
      isPublic: form.isPublic,
      public: form.isPublic,
      uploadedChunks: [],
      progress: 0,
      status: UploadStatus.Pending,
      orgTag: form.orgTag
    };

    newTask.orgTagName = form.orgTagName ?? null;

    // 将新的上传任务添加到任务队列中
    tasks.value.push(newTask);
    // 启动上传流程
    startUpload();
    // 返回新的上传任务
  }

  /** 启动文件上传的异步函数 该函数负责从待上传队列中启动文件上传任务，并管理并发上传的数量 */
  async function startUpload() {
    if (!authStore.token) return;
    const identity = authStore.getIdentityVersion();
    // 限制可同时上传的文件个数
    if (activeUploads.value.size >= 3) return;
    // 获取待上传的文件
    const pendingTasks = tasks.value.filter(
      t => t.status === UploadStatus.Pending && t.userId === authStore.userInfo.id && !activeUploads.value.has(t.localId ?? '')
    );

    // 如果没有待上传的文件，则直接返回
    if (pendingTasks.length === 0) return;

    // 获取第一个待上传的文件
    const task = pendingTasks[0];
    task.localId ??= nanoid();
    const localId = task.localId;
    task.status = UploadStatus.Uploading;
    activeUploads.value.add(localId);

    // 计算文件总片数
    const totalChunks = Math.ceil(task.totalSize / chunkSize);

    try {
      if (task.uploadedChunks.length === totalChunks) {
        const success = await mergeFile(task, identity);
        if (!success) throw new Error('文件合并失败');
      }
      // const promises = [];
      // 遍历所有片数
      for (let i = 0; i < totalChunks; i += 1) {
        if (!currentTask(task, identity)) return;
        // 如果未上传，则上传
        if (!task.uploadedChunks.includes(i)) {
          task.chunkIndex = i;
          // promises.push(uploadChunk(task))
          // eslint-disable-next-line no-await-in-loop
          const success = await uploadChunk(task, identity);
          if (!success) throw new Error('分片上传失败');
        }
      }
      // await Promise.all(promises)
    } catch (e) {
      console.error('%c [ 👉 upload error 👈 ]-168', 'font-size:16px; background:#94cc97; color:#d8ffdb;', e);
      // 如果上传失败，则将任务状态设置为中断
      const updatedTask = currentTask(task, identity);
      if (updatedTask) updatedTask.status = UploadStatus.Break;
    } finally {
      // 无论成功或失败，都从活跃队列中移除
      activeUploads.value.delete(localId);
      // 继续下一个任务
      startUpload();
    }
  }

  return {
    tasks,
    activeUploads,
    enqueueUpload,
    startUpload
  };
});
