package handler

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/service"
	"pai-smart-go/pkg/llm"
	"pai-smart-go/pkg/log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type chatServiceFunc func(context.Context, string, *model.User, llm.ChunkHandler, func() bool) error

func (f chatServiceFunc) StreamResponse(ctx context.Context, query string, user *model.User, emit llm.ChunkHandler, _ service.SourceHandler, stop func() bool) error {
	return f(ctx, query, user, emit, stop)
}

type chatTestUsers struct {
	service.UserService
	profile  func(context.Context, string) (*model.User, error)
	session  service.WebsocketSession
	validate func(context.Context, service.WebsocketSession) (*model.User, error)
}

func (u chatTestUsers) GetProfile(ctx context.Context, username string) (*model.User, error) {
	if u.profile != nil {
		return u.profile(ctx, username)
	}
	return &model.User{ID: 1, Username: username, Role: "USER"}, nil
}

func (u chatTestUsers) ConsumeWebsocketTicket(context.Context, string) (service.WebsocketSession, error) {
	if u.session.UserID == 0 {
		u.session = service.WebsocketSession{UserID: 1, Username: "alice"}
	}
	return u.session, nil
}

func (u chatTestUsers) ValidateWebsocketSession(ctx context.Context, session service.WebsocketSession) (*model.User, error) {
	if u.validate != nil {
		return u.validate(ctx, session)
	}
	user, err := u.GetProfile(ctx, session.Username)
	if err != nil {
		return nil, err
	}
	if user == nil || user.ID != session.UserID || user.Username != session.Username {
		return nil, service.ErrWebsocketSessionInvalid
	}
	return user, nil
}

var chatTestInit sync.Once

func startChatTest(t *testing.T, chat service.ChatService, users service.UserService) string {
	return startChatTestConfigured(t, chat, users, nil)
}

func startChatTestConfigured(t *testing.T, chat service.ChatService, users service.UserService, configure func(*ChatHandler)) string {
	t.Helper()
	chatTestInit.Do(func() {
		gin.SetMode(gin.TestMode)
		log.Init("error", "json", "")
	})
	router := gin.New()
	handler := NewChatHandler(chat, users)
	if configure != nil {
		configure(handler)
	}
	router.GET("/chat/:ticket", handler.Handle)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/chat/test-ticket"
}

func TestChatRejectsRecreatedUserAndRevokedActiveSession(t *testing.T) {
	chatCalls := atomic.Int32{}
	chat := chatServiceFunc(func(context.Context, string, *model.User, llm.ChunkHandler, func() bool) error {
		chatCalls.Add(1)
		return nil
	})

	t.Run("same username with another user id fails before upgrade", func(t *testing.T) {
		url := startChatTest(t, chat, chatTestUsers{
			session: service.WebsocketSession{UserID: 1, Username: "alice"},
			profile: func(context.Context, string) (*model.User, error) {
				return &model.User{ID: 2, Username: "alice"}, nil
			},
		})
		conn, response, err := websocket.DefaultDialer.Dial(url, nil)
		if conn != nil {
			_ = conn.Close()
		}
		if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("recreated account reused a ticket: response=%v err=%v", response, err)
		}
	})

	t.Run("logout rejects the next round on an active connection", func(t *testing.T) {
		var revoked atomic.Bool
		users := chatTestUsers{validate: func(_ context.Context, session service.WebsocketSession) (*model.User, error) {
			if revoked.Load() {
				return nil, service.ErrWebsocketSessionInvalid
			}
			return &model.User{ID: session.UserID, Username: session.Username}, nil
		}}
		conn := dialChatTest(t, startChatTest(t, chat, users))
		revoked.Store(true)
		sendChatTest(t, conn, "must not run")
		message := readChatTest(t, conn)
		if !strings.Contains(fmt.Sprint(message["error"]), "认证已失效") || chatCalls.Load() != 0 {
			t.Fatalf("revoked session executed a query: message=%v calls=%d", message, chatCalls.Load())
		}
	})
}

func TestChatBoundsIdleAndPerUserConnections(t *testing.T) {
	chat := chatServiceFunc(func(context.Context, string, *model.User, llm.ChunkHandler, func() bool) error { return nil })
	t.Run("per-user connection limit", func(t *testing.T) {
		url := startChatTestConfigured(t, chat, chatTestUsers{}, func(handler *ChatHandler) {
			handler.maxConnectionsPerUser = 1
		})
		first := dialChatTest(t, url)
		second, response, err := websocket.DefaultDialer.Dial(url, nil)
		if second != nil {
			_ = second.Close()
		}
		if err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("connection limit was bypassed: response=%v err=%v", response, err)
		}
		_ = first.Close()
	})

	t.Run("idle peer without pong is closed", func(t *testing.T) {
		url := startChatTestConfigured(t, chat, chatTestUsers{}, func(handler *ChatHandler) {
			handler.pongWait = 80 * time.Millisecond
			handler.pingPeriod = 20 * time.Millisecond
		})
		conn := dialChatTest(t, url)
		time.Sleep(160 * time.Millisecond)
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
					t.Fatal("idle websocket remained open until the client deadline")
				}
				break
			}
		}
	})
}

func dialChatTest(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestChatRejectsCrossOriginUpgrade(t *testing.T) {
	url := startChatTest(t, chatServiceFunc(func(context.Context, string, *model.User, llm.ChunkHandler, func() bool) error {
		return nil
	}), chatTestUsers{})
	conn, response, err := websocket.DefaultDialer.Dial(url, http.Header{"Origin": {"https://attacker.example"}})
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin websocket was accepted: response=%v err=%v", response, err)
	}
}

func sendChatTest(t *testing.T, conn *websocket.Conn, text string) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(text)); err != nil {
		t.Fatal(err)
	}
}

func readChatTest(t *testing.T, conn *websocket.Conn) map[string]interface{} {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var message map[string]interface{}
	if err := conn.ReadJSON(&message); err != nil {
		t.Fatal(err)
	}
	return message
}

func requireCompletion(t *testing.T, conn *websocket.Conn, stopped bool) {
	t.Helper()
	message := readChatTest(t, conn)
	want := "响应已完成"
	if stopped {
		want = "响应已停止"
	}
	if message["type"] != "completion" || message["status"] != "finished" || message["message"] != want {
		t.Fatalf("unexpected terminal frame: %#v", message)
	}
}

func waitChatTest(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for chat worker")
	}
}

func TestChatStopBeforeFirstChunkAndContinue(t *testing.T) {
	for name, command := range map[string]string{
		"json":          `{"type":"stop"}`,
		"rotated-token": `{"type":"stop","_internal_cmd_token":"old-token"}`,
		"legacy-token":  "WSS_STOP_CMD_old-token",
	} {
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			stopped := make(chan struct{})
			var profiles atomic.Int32
			var calls atomic.Int32
			var oldEmit llm.ChunkHandler
			users := chatTestUsers{profile: func(_ context.Context, username string) (*model.User, error) {
				return &model.User{ID: 1, Username: username, PrimaryOrg: fmt.Sprint(profiles.Add(1))}, nil
			}}
			chat := chatServiceFunc(func(ctx context.Context, query string, user *model.User, emit llm.ChunkHandler, shouldStop func() bool) error {
				calls.Add(1)
				if query == "planning" {
					oldEmit = emit
					close(started)
					<-ctx.Done()
					if !shouldStop() {
						t.Error("stop callback did not observe cancellation")
					}
					if err := emit([]byte("stale output")); !errors.Is(err, context.Canceled) {
						t.Errorf("late chunk was not rejected: %v", err)
					}
					close(stopped)
					return ctx.Err()
				}
				return emit([]byte(query + ":" + user.PrimaryOrg))
			})
			conn := dialChatTest(t, startChatTest(t, chat, users))
			sendChatTest(t, conn, "planning")
			waitChatTest(t, started)
			// A duplicate query while busy must not finish the original answer.
			sendChatTest(t, conn, "duplicate")
			sendChatTest(t, conn, command)
			waitChatTest(t, stopped)
			requireCompletion(t, conn, true)
			sendChatTest(t, conn, "next")
			if err := oldEmit([]byte("late previous round")); !errors.Is(err, context.Canceled) {
				t.Fatalf("previous round remained writable: %v", err)
			}
			if message := readChatTest(t, conn); message["chunk"] != "next:3" {
				t.Fatalf("wrong next answer or stale profile: %#v", message)
			}
			requireCompletion(t, conn, false)
			if calls.Load() != 2 {
				t.Fatalf("busy query was executed: %d calls", calls.Load())
			}
		})
	}
}

func TestChatStopIsConnectionLocalAndDisconnectCancels(t *testing.T) {
	started := make(chan string, 2)
	canceled := make(chan string, 2)
	chat := chatServiceFunc(func(ctx context.Context, query string, _ *model.User, _ llm.ChunkHandler, _ func() bool) error {
		started <- query
		<-ctx.Done()
		canceled <- query
		return ctx.Err()
	})
	url := startChatTest(t, chat, chatTestUsers{})
	first := dialChatTest(t, url)
	second := dialChatTest(t, url)
	sendChatTest(t, first, "first")
	sendChatTest(t, second, "second")
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not start")
		}
	}
	sendChatTest(t, first, `{"type":"stop"}`)
	requireCompletion(t, first, true)
	select {
	case got := <-canceled:
		if got != "first" {
			t.Fatalf("stopped another connection: %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not cancel the first connection")
	}
	select {
	case got := <-canceled:
		t.Fatalf("unexpected cancellation: %s", got)
	default:
	}
	_ = second.Close()
	select {
	case got := <-canceled:
		if got != "second" {
			t.Fatalf("wrong disconnected worker: %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not cancel the second connection")
	}
}

func TestChatSerializesConcurrentChunksAndTerminatesOnce(t *testing.T) {
	const count = 64
	chat := chatServiceFunc(func(ctx context.Context, query string, _ *model.User, emit llm.ChunkHandler, _ func() bool) error {
		if query == "next" {
			return emit([]byte(query))
		}
		var workers sync.WaitGroup
		for i := range count {
			workers.Add(1)
			go func() {
				defer workers.Done()
				if err := emit([]byte(fmt.Sprint(i))); err != nil {
					t.Errorf("emit: %v", err)
				}
			}()
		}
		workers.Wait()
		return nil
	})
	conn := dialChatTest(t, startChatTest(t, chat, chatTestUsers{}))
	sendChatTest(t, conn, "concurrent")
	seen := make(map[string]bool)
	for range count {
		message := readChatTest(t, conn)
		chunk, ok := message["chunk"].(string)
		if !ok || seen[chunk] {
			t.Fatalf("missing, duplicate, or early terminal frame: %#v", message)
		}
		seen[chunk] = true
	}
	requireCompletion(t, conn, false)
	sendChatTest(t, conn, "next")
	if message := readChatTest(t, conn); message["chunk"] != "next" {
		t.Fatalf("previous round leaked a frame: %#v", message)
	}
	requireCompletion(t, conn, false)
}

func TestChatErrorsTerminateAndConnectionRemainsUsable(t *testing.T) {
	for name, failure := range map[string]error{
		"service":       errors.New("planner returned invalid JSON"),
		"busy":          service.ErrConversationBusy,
		"save":          fmt.Errorf("%w: private backend details", service.ErrConversationSave),
		"understanding": fmt.Errorf("%w: private backend details", service.ErrQueryUnderstanding),
	} {
		t.Run(name, func(t *testing.T) {
			chat := chatServiceFunc(func(ctx context.Context, query string, _ *model.User, emit llm.ChunkHandler, _ func() bool) error {
				if query == "fail" {
					return failure
				}
				return emit([]byte("recovered"))
			})
			conn := dialChatTest(t, startChatTest(t, chat, chatTestUsers{}))
			sendChatTest(t, conn, "fail")
			message := readChatTest(t, conn)
			if message["error"] == nil {
				t.Fatalf("missing error frame: %#v", message)
			}
			if strings.Contains(fmt.Sprint(message["error"]), "private backend details") {
				t.Fatalf("backend details leaked: %#v", message)
			}
			if name == "save" && !strings.Contains(fmt.Sprint(message["error"]), "未保存") {
				t.Fatalf("missing persistence warning: %#v", message)
			}
			if name == "understanding" && !strings.Contains(fmt.Sprint(message["error"]), "补充完整问题") {
				t.Fatalf("missing understanding recovery guidance: %#v", message)
			}
			requireCompletion(t, conn, false)
			sendChatTest(t, conn, "next")
			if message := readChatTest(t, conn); message["chunk"] != "recovered" {
				t.Fatalf("connection was not reusable: %#v", message)
			}
			requireCompletion(t, conn, false)
		})
	}
}

func TestChatStopDuringProfileRefresh(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	users := chatTestUsers{profile: func(ctx context.Context, username string) (*model.User, error) {
		if calls.Add(1) == 2 {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &model.User{ID: 1, Username: username}, nil
	}}
	chat := chatServiceFunc(func(_ context.Context, query string, _ *model.User, emit llm.ChunkHandler, _ func() bool) error {
		if query != "next" {
			t.Error("canceled profile refresh reached chat service")
		}
		return emit([]byte(query))
	})
	conn := dialChatTest(t, startChatTest(t, chat, users))
	sendChatTest(t, conn, "loading profile")
	waitChatTest(t, started)
	sendChatTest(t, conn, `{"type":"stop"}`)
	requireCompletion(t, conn, true)
	sendChatTest(t, conn, "next")
	if message := readChatTest(t, conn); message["chunk"] != "next" {
		t.Fatalf("connection not reusable after profile cancellation: %#v", message)
	}
	requireCompletion(t, conn, false)
}

func TestChatProfileRefreshFailureDoesNotUseStalePermissions(t *testing.T) {
	var profiles atomic.Int32
	users := chatTestUsers{profile: func(_ context.Context, username string) (*model.User, error) {
		if profiles.Add(1) > 1 {
			return nil, errors.New("profile lookup failed")
		}
		return &model.User{ID: 1, Username: username}, nil
	}}
	chat := chatServiceFunc(func(context.Context, string, *model.User, llm.ChunkHandler, func() bool) error {
		t.Error("query executed using stale profile")
		return nil
	})
	conn := dialChatTest(t, startChatTest(t, chat, users))
	sendChatTest(t, conn, "query")
	if message := readChatTest(t, conn); message["error"] == nil {
		t.Fatalf("missing profile error: %#v", message)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("connection remained usable after its identity could not be refreshed")
	}
}
