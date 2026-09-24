<script setup lang="ts">
const chatStore = useChatStore();
const { input, wsStatus, isSending, isStopping } = storeToRefs(chatStore);

const sendable = computed(
  () => wsStatus.value === 'OPEN' && !isStopping.value && (isSending.value || Boolean(input.value.message.trim()))
);

const handleSend = () => {
  if (!sendable.value) return;
  if (isSending.value) {
    chatStore.stopAnswer();
  } else {
    chatStore.sendMessage();
  }
};

const inputRef = ref<HTMLTextAreaElement>();
// 手动插入换行符（确保所有浏览器兼容）
const insertNewline = () => {
  const textarea = inputRef.value;
  if (!textarea) return;
  const start = textarea.selectionStart;
  const end = textarea.selectionEnd;

  // 在光标位置插入换行符
  input.value.message = `${input.value.message.substring(0, start)}\n${input.value.message.substring(end)}`;

  // 更新光标位置（在插入的换行符之后）
  nextTick(() => {
    textarea.selectionStart = start + 1;
    textarea.selectionEnd = start + 1;
    textarea.focus(); // 确保保持焦点
  });
};

// ctrl + enter 换行
// enter 发送
const handShortcut = (e: KeyboardEvent) => {
  if (e.isComposing) return;
  if (e.key === 'Enter') {
    e.preventDefault();

    if (!e.shiftKey && !e.ctrlKey) {
      handleSend();
    } else insertNewline();
  }
};
</script>

<template>
  <div class="relative w-full b-1 b-#1c1c1c20 bg-#fff p-4 card-wrapper dark:bg-#1c1c1c">
    <textarea
      ref="inputRef"
      v-model.trim="input.message"
      placeholder="给 派聪明 发送消息"
      class="min-h-10 w-full cursor-text resize-none b-none bg-transparent color-#333 caret-[rgb(var(--primary-color))] outline-none dark:color-#f1f1f1"
      @keydown="handShortcut"
    />
    <div class="flex items-center justify-between pt-2">
      <div class="flex items-center text-18px color-gray-500">
        <NText class="text-14px">连接状态：</NText>
        <icon-eos-icons:loading v-if="wsStatus === 'CONNECTING'" class="color-yellow" />
        <icon-fluent:plug-connected-checkmark-20-filled v-else-if="wsStatus === 'OPEN'" class="color-green" />
        <icon-tabler:plug-connected-x v-else class="color-red" />
      </div>
      <NButton :disabled="!sendable" strong circle type="primary" @click="handleSend">
        <template #icon>
          <icon-material-symbols:stop-rounded v-if="isSending" />
          <icon-guidance:send v-else />
        </template>
      </NButton>
    </div>
  </div>
</template>

<style scoped></style>
