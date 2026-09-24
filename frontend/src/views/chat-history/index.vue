<script setup lang="ts">
import type { NScrollbar } from 'naive-ui';
import { VueMarkdownItProvider } from 'vue-markdown-shiki';
import { chatMarkdownOptions } from '@/utils/source-citations';
import ChatMessage from '../chat/modules/chat-message.vue';

defineOptions({
  name: 'ChatHistory'
});

const scrollbarRef = ref<InstanceType<typeof NScrollbar>>();

const list = ref<Api.Chat.Message[]>([]);
const loading = ref(false);
const page = ref(1);
const pageSize = 20;
const total = ref(0);
let historyRequest = 0;

const store = useAuthStore();

watch(() => [...list.value], scrollToBottom);

function scrollToBottom() {
  setTimeout(() => {
    scrollbarRef.value?.scrollBy({
      top: 999999999999999,
      behavior: 'auto'
    });
  }, 100);
}

const range = ref<[number, number] | null>([dayjs().subtract(7, 'day').valueOf(), dayjs().add(1, 'day').valueOf()]);
const userId = ref<number>(store.userInfo.id);

const params = computed(() => {
  return {
    userid: userId.value,
    start_date: range.value ? dayjs(range.value[0]).format('YYYY-MM-DD') : undefined,
    end_date: range.value ? dayjs(range.value[1]).format('YYYY-MM-DD') : undefined,
    page: page.value,
    size: pageSize
  };
});

watch([userId, range], () => {
  if (page.value !== 1) page.value = 1;
  else void getList();
}, { deep: true });
watch([page, () => store.token, () => store.userInfo.id], () => {
  void getList();
}, { immediate: true });
onBeforeUnmount(() => {
  historyRequest += 1;
});

async function getList() {
  const current = ++historyRequest;
  const identity = store.getIdentityVersion();
  const token = store.token;
  const currentUserId = store.userInfo.id;
  const requestParams = { ...params.value };
  if (!requestParams.userid || !token) {
    list.value = [];
    total.value = 0;
    loading.value = false;
    return;
  }
  loading.value = true;
  try {
    const { error, data } = await request<{
      content: Api.Chat.Message[];
      totalElements: number;
      totalPages: number;
      size: number;
      number: number;
    }>({
      url: 'admin/conversation',
      params: requestParams
    });
    if (
      current !== historyRequest ||
      identity !== store.getIdentityVersion() ||
      token !== store.token ||
      currentUserId !== store.userInfo.id
    ) return;
    list.value = !error && Array.isArray(data?.content) ? data.content : [];
    total.value = !error && Number.isSafeInteger(data?.totalElements) && data.totalElements >= 0 ? data.totalElements : 0;
    if (!error) scrollToBottom();
  } finally {
    if (current === historyRequest) loading.value = false;
  }
}
</script>

<template>
  <div class="h-full">
    <Teleport defer to="#header-extra">
      <div class="px-10">
        <NForm :model="params" label-placement="left" :show-feedback="false" inline>
          <NFormItem label="用户">
            <TheSelect
              v-model:value="userId"
              url="admin/users/list"
              :params="{ page: 1, size: 999, orgTag: store.userInfo.primaryOrg }"
              key-field="content"
              value-field="userId"
              label-field="username"
              class="clear w-200px!"
              :clearable="false"
            />
          </NFormItem>
          <NFormItem label="时间">
            <NDatePicker v-model:value="range" type="daterange" class="clear" />
          </NFormItem>
        </NForm>
      </div>
    </Teleport>
    <NScrollbar ref="scrollbarRef">
      <NSpin :show="loading" class="h-full">
        <VueMarkdownItProvider :options="chatMarkdownOptions">
          <ChatMessage v-for="(item, index) in list" :key="index" :msg="item" />
        </VueMarkdownItProvider>
        <NEmpty v-if="!list.length" description="暂无数据" class="mt-60" />
        <div v-if="total > pageSize" class="flex justify-center py-4">
          <NPagination v-model:page="page" :page-size="pageSize" :item-count="total" />
        </div>
      </NSpin>
    </NScrollbar>
  </div>
</template>

<style scoped lang="scss"></style>
