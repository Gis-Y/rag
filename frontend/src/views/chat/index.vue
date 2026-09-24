<script setup lang="ts">
import ChatList from './modules/chat-list.vue';
import InputBox from './modules/input-box.vue';
import MemoryPanel from './modules/memory-panel.vue';
</script>

<template>
  <div class="flex-col gap-4">
    <MemoryPanel />
    <ChatList />
    <InputBox />
  </div>
</template>

<style scoped></style>
