package service

import (
	"context"
	"errors"
	"fmt"
	"pai-smart-go/internal/model"
	"pai-smart-go/pkg/database"
	"pai-smart-go/pkg/token"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

type websocketRedisHook struct {
	mu     sync.Mutex
	values map[string]string
	fail   error
}

func (h *websocketRedisHook) BeforeProcess(ctx context.Context, _ redis.Cmder) (context.Context, error) {
	return ctx, errors.New("intercept redis command")
}

func (h *websocketRedisHook) AfterProcess(_ context.Context, cmd redis.Cmder) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	cmd.SetErr(nil)
	if h.fail != nil {
		cmd.SetErr(h.fail)
		return h.fail
	}
	args := cmd.Args()
	switch cmd.Name() {
	case "exists":
		_, exists := h.values[fmt.Sprint(args[1])]
		cmd.(*redis.IntCmd).SetVal(map[bool]int64{false: 0, true: 1}[exists])
	case "set":
		key := fmt.Sprint(args[1])
		value := fmt.Sprint(args[2])
		if bytes, ok := args[2].([]byte); ok {
			value = string(bytes)
		}
		if conditional, ok := cmd.(*redis.BoolCmd); ok {
			nx := false
			for _, arg := range args[3:] {
				nx = nx || strings.EqualFold(fmt.Sprint(arg), "nx")
			}
			if !nx {
				cmd.SetErr(fmt.Errorf("expected atomic SET NX: %v", args))
				break
			}
			_, exists := h.values[key]
			if !exists {
				h.values[key] = value
			}
			conditional.SetVal(!exists)
		} else {
			h.values[key] = value
			cmd.(*redis.StatusCmd).SetVal("OK")
		}
	case "eval":
		key := fmt.Sprint(args[3])
		value, exists := h.values[key]
		if !exists {
			cmd.SetErr(redis.Nil)
			break
		}
		delete(h.values, key)
		cmd.(*redis.Cmd).SetVal(value)
	default:
		cmd.SetErr(fmt.Errorf("unexpected redis command: %v", args))
	}
	return cmd.Err()
}

func (*websocketRedisHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, errors.New("unexpected pipeline")
}

func (*websocketRedisHook) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }

func useWebsocketTestRedis(t *testing.T) *websocketRedisHook {
	t.Helper()
	previousRedis := database.RDB
	hook := &websocketRedisHook{values: make(map[string]string)}
	client := redis.NewClient(&redis.Options{Addr: "unused:0"})
	client.AddHook(hook)
	database.RDB = client
	t.Cleanup(func() {
		database.RDB = previousRedis
		_ = client.Close()
	})
	return hook
}

func TestWebsocketTicketBindsAccessAndCurrentUserIdentity(t *testing.T) {
	hook := useWebsocketTestRedis(t)
	currentID := uint(7)
	users := contextUserRepo{find: func(context.Context) (*model.User, error) {
		return &model.User{ID: currentID, Username: "alice", Role: "USER"}, nil
	}}
	manager := token.NewJWTManager("websocket-session-test", 1, 1)
	service := NewUserService(users, nil, manager)
	access, err := manager.GenerateToken(7, "alice", "USER")
	if err != nil {
		t.Fatal(err)
	}
	user := &model.User{ID: 7, Username: "alice", Role: "USER"}
	ticket, err := service.IssueWebsocketTicket(context.Background(), user, access)
	if err != nil {
		t.Fatal(err)
	}
	stored := hook.values[websocketTicketKey(ticket)]
	if stored == "" || strings.Contains(stored, access) || !strings.Contains(stored, revokedTokenKey(access)) {
		t.Fatalf("ticket stored bearer or lost its revocation identity: %q", stored)
	}

	session, err := service.ConsumeWebsocketTicket(context.Background(), ticket)
	if err != nil || session.UserID != 7 || session.Username != "alice" {
		t.Fatalf("consume: %+v %v", session, err)
	}
	if _, err := service.ConsumeWebsocketTicket(context.Background(), ticket); !errors.Is(err, ErrWebsocketSessionInvalid) {
		t.Fatalf("ticket was reusable: %v", err)
	}
	if _, err := service.ValidateWebsocketSession(context.Background(), session); err != nil {
		t.Fatalf("active session rejected: %v", err)
	}

	currentID = 8
	if _, err := service.ValidateWebsocketSession(context.Background(), session); !errors.Is(err, ErrWebsocketSessionInvalid) {
		t.Fatalf("same-name recreated account reused the session: %v", err)
	}
	currentID = 7
	if err := service.Logout(access, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ValidateWebsocketSession(context.Background(), session); !errors.Is(err, ErrWebsocketSessionInvalid) {
		t.Fatalf("logout did not revoke the websocket session: %v", err)
	}

	hook.fail = errors.New("redis unavailable")
	if _, err := service.ValidateWebsocketSession(context.Background(), session); err == nil {
		t.Fatal("redis outage did not fail closed")
	}
}

func TestRefreshTokenIsConsumedOnceAcrossConcurrentRequests(t *testing.T) {
	useWebsocketTestRedis(t)
	manager := token.NewJWTManager("refresh-rotation-test", 1, 1)
	refresh, err := manager.GenerateRefreshToken(7, "alice", "USER")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var workers sync.WaitGroup
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		workers.Wait()
	})
	users := contextUserRepo{find: func(context.Context) (*model.User, error) {
		// Both requests must pass the revocation pre-check before either SET NX.
		entered <- struct{}{}
		<-release
		return &model.User{ID: 7, Username: "alice", Role: "USER"}, nil
	}}
	service := NewUserService(users, nil, manager)
	type result struct {
		access, refresh string
		err             error
	}
	results := make(chan result, 2)
	workers.Add(2)
	for range 2 {
		go func() {
			defer workers.Done()
			access, rotated, err := service.RefreshToken(refresh)
			results <- result{access, rotated, err}
		}()
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for range 2 {
		select {
		case <-entered:
		case <-deadline.C:
			t.Fatal("concurrent refresh requests did not reach the rotation barrier")
		}
	}
	releaseOnce.Do(func() { close(release) })
	successes := 0
	for range 2 {
		select {
		case got := <-results:
			if got.err != nil {
				if got.access != "" || got.refresh != "" {
					t.Fatal("losing refresh request exposed newly generated tokens")
				}
				continue
			}
			successes++
			if _, err := manager.VerifyAccessToken(got.access); err != nil {
				t.Fatalf("winner returned invalid access token: %v", err)
			}
			if _, err := manager.VerifyRefreshToken(got.refresh); err != nil || got.refresh == refresh {
				t.Fatalf("winner did not rotate the refresh token: %v", err)
			}
		case <-deadline.C:
			t.Fatal("concurrent refresh requests did not finish")
		}
	}
	if successes != 1 {
		t.Fatalf("refresh token was consumed %d times, want exactly once", successes)
	}
	if access, rotated, err := service.RefreshToken(refresh); err == nil || access != "" || rotated != "" {
		t.Fatal("consumed refresh token was accepted again")
	}
}

func TestLogoutRevokesAccessDespiteExpiredOrInvalidRefresh(t *testing.T) {
	hook := useWebsocketTestRedis(t)
	const secret = "logout-refresh-test"
	manager := token.NewJWTManager(secret, 1, 1)
	expired, err := token.NewJWTManager(secret, 1, -1).GenerateRefreshToken(7, "alice", "USER")
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := manager.GenerateRefreshToken(7, "alice", "USER")
	if err != nil {
		t.Fatal(err)
	}
	hook.values[revokedTokenKey(revoked)] = "1"
	service := NewUserService(nil, nil, manager)
	for name, refresh := range map[string]string{
		"expired":         expired,
		"malformed":       "not-a-jwt",
		"already revoked": revoked,
	} {
		t.Run(name, func(t *testing.T) {
			access, err := manager.GenerateToken(7, "alice", "USER")
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Logout(access, refresh); err != nil {
				t.Fatalf("stale refresh prevented logout: %v", err)
			}
			if revoked, err := service.IsTokenRevoked(context.Background(), access); err != nil || !revoked {
				t.Fatalf("access token survived logout: revoked=%v err=%v", revoked, err)
			}
		})
	}
}
