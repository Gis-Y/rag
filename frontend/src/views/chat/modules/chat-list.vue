<script setup lang="ts">
import { NScrollbar } from 'naive-ui';
import { VueMarkdownItProvider } from 'vue-markdown-shiki';
import { chatMarkdownOptions } from '@/utils/source-citations';
import ChatMessage from './chat-message.vue';

defineOptions({
  name: 'ChatList'
});

const chatStore = useChatStore();
const authStore = useAuthStore();
const { list } = storeToRefs(chatStore);

const loading = ref(false);
let historyRequest = 0;
const scrollbarRef = ref<InstanceType<typeof NScrollbar>>();

watch(() => [...list.value], scrollToBottom);

function scrollToBottom() {
  setTimeout(() => {
    scrollbarRef.value?.scrollBy({
      top: 999999999999999,
      behavior: 'auto'
    });
  }, 100);
}

const range = ref<[number, number]>([dayjs().subtract(7, 'day').valueOf(), dayjs().add(1, 'day').valueOf()]);

const params = computed(() => {
  return {
    start_date: dayjs(range.value[0]).format('YYYY-MM-DD'),
    end_date: dayjs(range.value[1]).format('YYYY-MM-DD')
  };
});

watch([params, () => authStore.token], () => getList(), { immediate: true });

async function getList() {
  const requestId = ++historyRequest;
  const token = authStore.token;
  const messages = list.value;
  const messageCount = messages.length;
  if (!token) {
    loading.value = false;
    return;
  }
  loading.value = true;
  const { error, data } = await request<Api.Chat.Message[]>({
    url: 'users/conversation',
    params: params.value
  });
  if (requestId !== historyRequest || authStore.token !== token) return;
  if (!error && list.value === messages && messages.length === messageCount && !chatStore.isSending) {
    list.value = data;
  }
  loading.value = false;
}

onUnmounted(() => {
  historyRequest += 1;
});

onMounted(() => {
  chatStore.scrollToBottom = scrollToBottom;
});
</script>

<template>
  <Suspense>
    <NScrollbar ref="scrollbarRef" class="h-0 flex-auto">
      <Teleport defer to="#header-extra">
        <div class="px-10">
          <NForm :model="params" label-placement="left" :show-feedback="false" inline>
            <NFormItem label="时间">
              <NDatePicker v-model:value="range" type="daterange" />
            </NFormItem>
          </NForm>
        </div>
      </Teleport>
      <NSpin :show="loading">
        <VueMarkdownItProvider :options="chatMarkdownOptions">
          <ChatMessage v-for="(item, index) in list" :key="index" :msg="item" />
        </VueMarkdownItProvider>
      </NSpin>
    </NScrollbar>
  </Suspense>
</template>

<style scoped lang="scss"></style>
