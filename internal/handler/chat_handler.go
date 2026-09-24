// Package handler 包含了处理 HTTP 请求的控制器逻辑。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/service"
	"pai-smart-go/pkg/log"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{}

const (
	defaultWebsocketPongWait           = 60 * time.Second
	defaultWebsocketPingPeriod         = 45 * time.Second
	defaultWebsocketConnectionsPerUser = 4
)

// ChatHandler 负责处理 WebSocket 聊天连接。
type ChatHandler struct {
	chatService           service.ChatService
	userService           service.UserService
	pongWait              time.Duration
	pingPeriod            time.Duration
	maxConnectionsPerUser int
	connectionsMu         sync.Mutex
	connectionsByUser     map[uint]int
}

// NewChatHandler 创建一个新的 ChatHandler。
func NewChatHandler(chatService service.ChatService, userService service.UserService) *ChatHandler {
	return &ChatHandler{
		chatService:           chatService,
		userService:           userService,
		pongWait:              defaultWebsocketPongWait,
		pingPeriod:            defaultWebsocketPingPeriod,
		maxConnectionsPerUser: defaultWebsocketConnectionsPerUser,
		connectionsByUser:     make(map[uint]int),
	}
}

// GetWebsocketTicket 返回一个短时、一次性的 WebSocket 建连凭证。
func (h *ChatHandler) GetWebsocketTicket(c *gin.Context) {
	userValue, ok := c.Get("user")
	user, valid := userValue.(*model.User)
	if !ok || !valid || user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": http.StatusUnauthorized, "message": "未认证", "data": nil})
		return
	}
	accessToken, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
	if !ok || accessToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": http.StatusUnauthorized, "message": "未认证", "data": nil})
		return
	}
	ticket, err := h.userService.IssueWebsocketTicket(c.Request.Context(), user, accessToken)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": http.StatusServiceUnavailable, "message": "认证服务暂时不可用", "data": nil})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": http.StatusOK, "message": "success", "data": gin.H{"ticket": ticket}})
}

type chatRound struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type chatChunk struct {
	round     *chatRound
	text      string
	sources   []model.SourceCitation
	isSources bool
}

type chatResult struct {
	round      *chatRound
	err        error
	authFailed bool
}

// Handle 处理一个传入的 WebSocket 连接。
func (h *ChatHandler) Handle(c *gin.Context) {
	session, err := h.userService.ConsumeWebsocketTicket(c.Request.Context(), c.Param("ticket"))
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": http.StatusUnauthorized, "message": "无效或已过期的连接凭证", "data": nil})
		return
	}
	if _, err = h.userService.ValidateWebsocketSession(c.Request.Context(), session); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": http.StatusUnauthorized, "message": "连接认证已失效", "data": nil})
		return
	}
	if !h.acquireConnection(session.UserID) {
		c.JSON(http.StatusTooManyRequests, gin.H{"code": http.StatusTooManyRequests, "message": "连接数过多", "data": nil})
		return
	}
	defer h.releaseConnection(session.UserID)

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Error("WebSocket 升级失败", err)
		return
	}
	defer conn.Close()
	conn.SetReadLimit(64 * 1024)
	if err := conn.SetReadDeadline(time.Now().Add(h.pongWait)); err != nil {
		return
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(h.pongWait))
	})
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()

	// 读循环不能被查询理解、检索或生成阻塞，否则停止和断线无法取消它们。
	messages := make(chan []byte)
	go func() {
		defer cancel()
		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if messageType != websocket.TextMessage {
				continue
			}
			select {
			case messages <- message:
			case <-ctx.Done():
				return
			}
		}
	}()

	// 所有业务帧由下面的事件循环串行写入，worker 不直接访问连接。
	write := func(payload interface{}) error {
		if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return err
		}
		return conn.WriteJSON(payload)
	}
	chunks := make(chan chatChunk)
	results := make(chan chatResult)
	var active *chatRound
	pingTicker := time.NewTicker(h.pingPeriod)
	defer pingTicker.Stop()

	log.Infof("WebSocket 连接已建立，用户: %s", session.Username)

	for {
		select {
		case <-ctx.Done():
			return
		case <-pingTicker.C:
			if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case message := <-messages:
			if isChatStop(message) {
				if active != nil {
					active.cancel()
				}
				continue
			}
			// 忙时忽略重复提交，不发送会被错配到活动回答的 completion。
			if active != nil || strings.TrimSpace(string(message)) == "" {
				continue
			}
			roundCtx, roundCancel := context.WithCancel(ctx)
			round := &chatRound{ctx: roundCtx, cancel: roundCancel}
			active = round
			go func(query string) {
				emit := func(data []byte) error {
					if err := round.ctx.Err(); err != nil {
						return err
					}
					select {
					case chunks <- chatChunk{round: round, text: string(data)}:
						return round.ctx.Err()
					case <-round.ctx.Done():
						return round.ctx.Err()
					}
				}
				emitSources := func(sources []model.SourceCitation) error {
					select {
					case chunks <- chatChunk{round: round, sources: sources, isSources: true}:
						return round.ctx.Err()
					case <-round.ctx.Done():
						return round.ctx.Err()
					}
				}
				// 长连接每轮刷新权限，不能一直复用建立连接时的组织与角色。
				currentUser, err := h.userService.ValidateWebsocketSession(round.ctx, session)
				authFailed := err != nil && !errors.Is(err, context.Canceled)
				if err == nil && currentUser == nil {
					err = errors.New("user profile unavailable")
					authFailed = true
				}
				if err == nil {
					err = round.ctx.Err()
				}
				if err == nil {
					err = h.chatService.StreamResponse(round.ctx, query, currentUser, emit, emitSources, func() bool {
						return round.ctx.Err() != nil
					})
				}
				select {
				case results <- chatResult{round: round, err: err, authFailed: authFailed}:
				case <-ctx.Done():
				}
			}(string(message))
		case chunk := <-chunks:
			if chunk.round != active || chunk.round.ctx.Err() != nil {
				continue
			}
			var payload any = map[string]string{"chunk": chunk.text}
			if chunk.isSources {
				payload = map[string]any{"type": "sources", "sources": chunk.sources}
			}
			if err := write(payload); err != nil {
				return
			}
		case result := <-results:
			if result.round != active {
				continue
			}
			if result.authFailed {
				active.cancel()
				active = nil
				_ = write(map[string]string{"error": "连接认证已失效，请重新登录"})
				return
			}
			stopped := active.ctx.Err() != nil || errors.Is(result.err, context.Canceled)
			active.cancel()
			active = nil
			if result.err != nil && !stopped {
				log.Errorf("处理流式响应失败: %v", result.err)
				message := "AI服务暂时不可用，请稍后重试"
				if errors.Is(result.err, service.ErrConversationBusy) {
					message = "当前会话正在处理上一条问题，请稍后重试"
				} else if errors.Is(result.err, service.ErrConversationSave) {
					message = "本轮会话未保存，请勿依赖本轮内容继续指代提问"
				} else if errors.Is(result.err, service.ErrContextBudget) {
					message = "问题、记忆与资料超过上下文预算，请缩小问题范围或调整模型预算配置"
				} else if errors.Is(result.err, service.ErrMemoryPreparation) {
					message = "会话摘要暂未准备完成，原始历史仍保留，请重试"
				} else if errors.Is(result.err, service.ErrQueryUnderstanding) {
					message = "问题理解暂时失败，请补充完整问题或重试"
				}
				if err := write(map[string]string{"error": message}); err != nil {
					return
				}
			}
			message := "响应已完成"
			if stopped {
				message = "响应已停止"
			}
			if err := write(map[string]interface{}{
				"type": "completion", "status": "finished", "message": message,
				"timestamp": time.Now().UnixMilli(), "date": time.Now().Format("2006-01-02T15:04:05"),
			}); err != nil {
				return
			}
		}
	}
}

func (h *ChatHandler) acquireConnection(userID uint) bool {
	h.connectionsMu.Lock()
	defer h.connectionsMu.Unlock()
	// ponytail: this limit is process-local; use a Redis lease only when the
	// deployment needs a strict cross-instance connection cap.
	if userID == 0 || h.connectionsByUser[userID] >= h.maxConnectionsPerUser {
		return false
	}
	h.connectionsByUser[userID]++
	return true
}

func (h *ChatHandler) releaseConnection(userID uint) {
	h.connectionsMu.Lock()
	defer h.connectionsMu.Unlock()
	if h.connectionsByUser[userID] <= 1 {
		delete(h.connectionsByUser, userID)
		return
	}
	h.connectionsByUser[userID]--
}

func isChatStop(message []byte) bool {
	var control struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(message, &control) == nil && control.Type == "stop" {
		return true
	}
	// 旧客户端整条发送的保留命令前缀；令牌不赋予操作其他连接的权限。
	return strings.HasPrefix(string(message), "WSS_STOP_CMD_")
}
