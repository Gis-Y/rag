import { useWebSocket } from '@vueuse/core';
import { fetchWebsocketTicket } from '@/service/api';
import { getServiceBaseURL } from '@/utils/service';
import { sourceCitations } from '@/utils/source-citations';

const useServiceProxy = import.meta.env.DEV && import.meta.env.VITE_HTTP_PROXY === 'Y';
const websocketBaseURL = getServiceBaseURL(import.meta.env, useServiceProxy).otherBaseURL.ws || '';

function websocketURL(ticket: string) {
  const url = new URL(websocketBaseURL || '/', window.location.href);
  url.protocol = url.protocol === 'https:' ? 'wss:' : url.protocol === 'http:' ? 'ws:' : url.protocol;
  url.pathname = `${url.pathname.replace(/\/+$/, '')}/chat/${encodeURIComponent(ticket)}`;
  url.search = '';
  url.hash = '';
  return url.toString();
}

export const useChatStore = defineStore(SetupStoreId.Chat, () => {
  const conversationId = ref<string>('');
  const input = ref<Api.Chat.Input>({ message: '' });
  const list = ref<Api.Chat.Message[]>([]);
  const store = useAuthStore();
  const activeAssistant = shallowRef<Api.Chat.Message>();
  const isSending = computed(() => Boolean(activeAssistant.value));
  const isStopping = ref(false);
  const scrollToBottom = ref<null | (() => void)>(null);
  const wsTicket = ref('');
  let connectionEpoch = 0;
  let reconnectEnabled = false;
  let reconnectTimer: number | undefined;

  const {
    status: wsStatus,
    data: wsData,
    send,
    open,
    close,
    ws
  } = useWebSocket<string>(() => (wsTicket.value ? websocketURL(wsTicket.value) : undefined), {
    immediate: false,
    autoConnect: false,
    autoReconnect: false,
    onMessage(socket, event) {
      if (socket !== ws.value || !store.token) return;
      const assistant = activeAssistant.value;
      if (!assistant) return;

      let data: unknown;
      try {
        data = JSON.parse(event.data);
      } catch {
        void wsOpen();
        return;
      }
      if (!data || typeof data !== 'object' || Array.isArray(data)) {
        void wsOpen();
        return;
      }

      if ('type' in data && data.type === 'sources' && 'sources' in data) {
        assistant.sources = sourceCitations(data.sources);
      } else if ('type' in data && data.type === 'completion' && 'status' in data && data.status === 'finished') {
        if (assistant.status !== 'error') assistant.status = 'finished';
        activeAssistant.value = undefined;
        isStopping.value = false;
      } else if ('error' in data && typeof data.error === 'string') {
        assistant.status = 'error';
        window.$message?.error(data.error);
      } else if ('chunk' in data && typeof data.chunk === 'string' && assistant.status !== 'error') {
        assistant.status = 'loading';
        assistant.content += data.chunk;
      } else {
        void wsOpen();
      }
    },
    onDisconnected(socket) {
      if (socket !== ws.value) return;
      finishInterrupted();
      if (!reconnectEnabled || !store.token) return;
      reconnectEnabled = false;
      const epoch = connectionEpoch;
      reconnectTimer = window.setTimeout(() => {
        if (epoch === connectionEpoch && store.token) void wsOpen();
      }, 1000);
    }
  });

  function finishInterrupted() {
    if (activeAssistant.value) activeAssistant.value.status = 'error';
    activeAssistant.value = undefined;
    isStopping.value = false;
  }

  function closeCurrentSocket() {
    if (reconnectTimer !== undefined) window.clearTimeout(reconnectTimer);
    reconnectTimer = undefined;
    // VueUse callbacks from an old connection must not overwrite the new session's state.
    if (ws.value) {
      ws.value.onopen = null;
      ws.value.onmessage = null;
      ws.value.onerror = null;
      ws.value.onclose = null;
    }
    close();
    wsStatus.value = 'CLOSED';
    finishInterrupted();
  }

  function wsClose() {
    connectionEpoch += 1;
    reconnectEnabled = false;
    wsTicket.value = '';
    closeCurrentSocket();
  }

  async function wsOpen() {
    const epoch = ++connectionEpoch;
    const identity = store.getIdentityVersion();
    reconnectEnabled = false;
    wsTicket.value = '';
    closeCurrentSocket();
    if (!store.token) return;

    const { data, error } = await fetchWebsocketTicket();
    if (error || epoch !== connectionEpoch || identity !== store.getIdentityVersion() || !store.token) return;
    wsTicket.value = data.ticket;
    reconnectEnabled = true;
    open();
  }

  function resetStore() {
    wsClose();
    conversationId.value = '';
    input.value = { message: '' };
    list.value = [];
    wsData.value = null;
  }

  watch(
    () => store.token,
    token => {
      resetStore();
      if (token) void wsOpen();
    },
    { immediate: true, flush: 'sync' }
  );

  function sendMessage() {
    const message = input.value.message.trim();
    if (!message || isSending.value || wsStatus.value !== 'OPEN' || !ws.value) return;

    list.value.push({ role: 'user', content: message }, { role: 'assistant', content: '', status: 'pending', sources: [] });
    activeAssistant.value = list.value[list.value.length - 1];
    try {
      // Never buffer a query: reconnecting must not silently replay it in another session.
      if (!send(message, false)) {
        finishInterrupted();
        return;
      }
      input.value.message = '';
    } catch {
      void wsOpen();
    }
  }

  function stopAnswer() {
    if (!isSending.value || isStopping.value || wsStatus.value !== 'OPEN') return;
    isStopping.value = true;
    try {
      if (!send(JSON.stringify({ type: 'stop' }), false)) wsOpen();
      // Keep this round active until completion; trailing chunks still belong to this assistant.
    } catch {
      void wsOpen();
    }
  }

  return {
    input,
    conversationId,
    list,
    wsStatus,
    wsData,
    wsOpen,
    wsClose,
    isSending,
    isStopping,
    sendMessage,
    stopAnswer,
    resetStore,
    scrollToBottom
  };
});
