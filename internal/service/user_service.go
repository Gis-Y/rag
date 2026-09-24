// Package service 包含了应用的业务逻辑层。
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"pai-smart-go/internal/model"
	"pai-smart-go/internal/repository"
	"pai-smart-go/pkg/database"
	"pai-smart-go/pkg/hash"
	"pai-smart-go/pkg/log"
	"pai-smart-go/pkg/token"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

var ErrWebsocketSessionInvalid = errors.New("websocket session is invalid")

// WebsocketSession is the immutable identity derived from the access token
// which issued a one-time connection ticket. It never contains the bearer.
type WebsocketSession struct {
	UserID              uint   `json:"user_id"`
	Username            string `json:"username"`
	AccessRevocationKey string `json:"access_revocation_key"`
	AccessExpiresAt     int64  `json:"access_expires_at"`
}

// UserService 接口定义了所有与用户相关的业务操作。
type UserService interface {
	Register(username, password string) (*model.User, error)
	Login(username, password string) (accessToken, refreshToken string, err error)
	GetProfile(ctx context.Context, username string) (*model.User, error)
	Logout(accessToken, refreshToken string) error
	IsTokenRevoked(ctx context.Context, tokenString string) (bool, error)
	IssueWebsocketTicket(ctx context.Context, user *model.User, accessToken string) (string, error)
	ConsumeWebsocketTicket(ctx context.Context, ticket string) (WebsocketSession, error)
	ValidateWebsocketSession(ctx context.Context, session WebsocketSession) (*model.User, error)
	SetUserPrimaryOrg(username, orgTag string) error
	GetUserOrgTags(username string) (map[string]interface{}, error)
	GetUserEffectiveOrgTags(ctx context.Context, user *model.User) ([]string, error)
	RefreshToken(refreshTokenString string) (newAccessToken, newRefreshToken string, err error)
}

// userService 是 UserService 接口的实现。
type userService struct {
	userRepo   repository.UserRepository
	orgTagRepo repository.OrgTagRepository
	jwtManager *token.JWTManager
}

// NewUserService 创建一个新的 UserService 实例。
func NewUserService(userRepo repository.UserRepository, orgTagRepo repository.OrgTagRepository, jwtManager *token.JWTManager) UserService {
	return &userService{
		userRepo:   userRepo,
		orgTagRepo: orgTagRepo,
		jwtManager: jwtManager,
	}
}

// Register 处理用户注册的业务逻辑。
func (s *userService) Register(username, password string) (*model.User, error) {
	if !validUsername(username) {
		return nil, errors.New("用户名只能包含字母、数字、下划线、短横线或点，且长度为 1 到 32 个字符")
	}
	// 1. 检查用户名是否已存在
	_, err := s.userRepo.FindByUsername(context.Background(), username)
	if err == nil {
		return nil, errors.New("用户名已存在")
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	// 2. 对密码进行哈希处理
	hashedPassword, err := hash.HashPassword(password)
	if err != nil {
		return nil, err
	}

	privateTagID := "PRIVATE_" + username
	newUser := &model.User{
		Username:   username,
		Password:   hashedPassword,
		Role:       "USER",
		OrgTags:    privateTagID,
		PrimaryOrg: privateTagID,
	}
	privateTag := &model.OrganizationTag{
		TagID:       privateTagID,
		Name:        username + "的私人空间",
		Description: "用户的私人组织标签，仅用户本人可访问",
	}
	if err := s.userRepo.CreateWithPrivateTag(newUser, privateTag); err != nil {
		return nil, fmt.Errorf("创建用户失败: %w", err)
	}

	return newUser, nil
}

func validUsername(username string) bool {
	length := utf8.RuneCountInString(username)
	if length < 1 || length > 32 {
		return false
	}
	for _, r := range username {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

// Login 处理用户登录的业务逻辑。
func (s *userService) Login(username, password string) (accessToken, refreshToken string, err error) {
	// 1. 查找用户
	user, err := s.userRepo.FindByUsername(context.Background(), username)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", "", errors.New("invalid credentials")
		}
		return "", "", err
	}

	// 2. 验证密码
	if !hash.CheckPasswordHash(password, user.Password) {
		return "", "", errors.New("invalid credentials")
	}

	// 3. 生成 access token 和 refresh token
	accessToken, err = s.jwtManager.GenerateToken(user.ID, user.Username, user.Role)
	if err != nil {
		return "", "", err
	}
	refreshToken, err = s.jwtManager.GenerateRefreshToken(user.ID, user.Username, user.Role)
	if err != nil {
		return "", "", err
	}

	return accessToken, refreshToken, nil
}

// GetProfile 根据用户名获取用户详细信息。
func (s *userService) GetProfile(ctx context.Context, username string) (*model.User, error) {
	user, err := s.userRepo.FindByUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	return user, nil
}

// Logout 处理用户登出逻辑，将 token 加入 Redis 黑名单。
func (s *userService) Logout(tokenString, refreshToken string) error {
	claims, err := s.jwtManager.VerifyAccessToken(tokenString)
	if err != nil {
		return err
	}
	if err := revokeToken(context.Background(), tokenString, claims.ExpiresAt.Time, false); err != nil {
		return err
	}
	if refreshToken == "" {
		return nil
	}
	refreshClaims, err := s.jwtManager.VerifyRefreshToken(refreshToken)
	if err != nil || refreshClaims.UserID != claims.UserID {
		// A stale or unrelated refresh token must not undo logout of the active session.
		return nil
	}
	return revokeToken(context.Background(), refreshToken, refreshClaims.ExpiresAt.Time, false)
}

func (s *userService) IsTokenRevoked(ctx context.Context, tokenString string) (bool, error) {
	if database.RDB == nil {
		return false, errors.New("redis is unavailable")
	}
	count, err := database.RDB.Exists(ctx, revokedTokenKey(tokenString)).Result()
	return count > 0, err
}

func revokedTokenKey(tokenString string) string {
	digest := sha256.Sum256([]byte(tokenString))
	return fmt.Sprintf("auth:revoked:%x", digest)
}

func revokeToken(ctx context.Context, tokenString string, expiresAt time.Time, onlyOnce bool) error {
	if database.RDB == nil {
		return errors.New("redis is unavailable")
	}
	ttl := time.Until(expiresAt)
	if ttl <= 0 {
		return errors.New("token is expired")
	}
	if onlyOnce {
		added, err := database.RDB.SetNX(ctx, revokedTokenKey(tokenString), "1", ttl).Result()
		if err != nil {
			return err
		}
		if !added {
			return errors.New("token has already been used")
		}
		return nil
	}
	return database.RDB.Set(ctx, revokedTokenKey(tokenString), "1", ttl).Err()
}

const websocketTicketTTL = 30 * time.Second

func websocketTicketKey(ticket string) string {
	digest := sha256.Sum256([]byte(ticket))
	return fmt.Sprintf("auth:websocket:%x", digest)
}

func (s *userService) IssueWebsocketTicket(ctx context.Context, user *model.User, accessToken string) (string, error) {
	if database.RDB == nil {
		return "", errors.New("redis is unavailable")
	}
	if user == nil || user.ID == 0 || user.Username == "" || accessToken == "" || s.jwtManager == nil {
		return "", ErrWebsocketSessionInvalid
	}
	claims, err := s.jwtManager.VerifyAccessToken(accessToken)
	if err != nil || claims.UserID != user.ID || claims.Username != user.Username || claims.ExpiresAt == nil {
		return "", ErrWebsocketSessionInvalid
	}
	revocationKey := revokedTokenKey(accessToken)
	revoked, err := database.RDB.Exists(ctx, revocationKey).Result()
	if err != nil {
		return "", err
	}
	if revoked > 0 {
		return "", ErrWebsocketSessionInvalid
	}
	ttl := min(websocketTicketTTL, time.Until(claims.ExpiresAt.Time))
	if ttl <= 0 {
		return "", ErrWebsocketSessionInvalid
	}
	session := WebsocketSession{
		UserID:              claims.UserID,
		Username:            claims.Username,
		AccessRevocationKey: revocationKey,
		AccessExpiresAt:     claims.ExpiresAt.Unix(),
	}
	payload, err := json.Marshal(session)
	if err != nil {
		return "", err
	}
	ticket, err := token.GenerateRandomString(32)
	if err != nil {
		return "", err
	}
	if err := database.RDB.Set(ctx, websocketTicketKey(ticket), payload, ttl).Err(); err != nil {
		return "", err
	}
	return ticket, nil
}

func (s *userService) ConsumeWebsocketTicket(ctx context.Context, ticket string) (WebsocketSession, error) {
	var session WebsocketSession
	if database.RDB == nil {
		return session, errors.New("redis is unavailable")
	}
	const getAndDelete = `local value = redis.call('GET', KEYS[1]); if not value then return false end; redis.call('DEL', KEYS[1]); return value`
	payload, err := database.RDB.Eval(ctx, getAndDelete, []string{websocketTicketKey(ticket)}).Text()
	if errors.Is(err, redis.Nil) {
		return session, ErrWebsocketSessionInvalid
	}
	if err != nil {
		return session, err
	}
	if json.Unmarshal([]byte(payload), &session) != nil || !validWebsocketSession(session) {
		return WebsocketSession{}, ErrWebsocketSessionInvalid
	}
	return session, nil
}

func (s *userService) ValidateWebsocketSession(ctx context.Context, session WebsocketSession) (*model.User, error) {
	if !validWebsocketSession(session) || time.Now().Unix() >= session.AccessExpiresAt {
		return nil, ErrWebsocketSessionInvalid
	}
	if database.RDB == nil {
		return nil, errors.New("redis is unavailable")
	}
	revoked, err := database.RDB.Exists(ctx, session.AccessRevocationKey).Result()
	if err != nil {
		return nil, err
	}
	if revoked > 0 {
		return nil, ErrWebsocketSessionInvalid
	}
	user, err := s.userRepo.FindByUsername(ctx, session.Username)
	if err != nil {
		return nil, err
	}
	if user == nil || user.ID != session.UserID || user.Username != session.Username {
		return nil, ErrWebsocketSessionInvalid
	}
	return user, nil
}

func validWebsocketSession(session WebsocketSession) bool {
	const prefix = "auth:revoked:"
	if session.UserID == 0 || !validUsername(session.Username) || session.AccessExpiresAt <= 0 || !strings.HasPrefix(session.AccessRevocationKey, prefix) {
		return false
	}
	digest := strings.TrimPrefix(session.AccessRevocationKey, prefix)
	_, err := hex.DecodeString(digest)
	return len(digest) == sha256.Size*2 && err == nil
}

// SetUserPrimaryOrg 设置用户的主组织。
func (s *userService) SetUserPrimaryOrg(username, orgTag string) error {
	user, err := s.userRepo.FindByUsername(context.Background(), username)
	if err != nil {
		return err
	}
	if !hasOrgTag(user.OrgTags, orgTag) {
		return errors.New("user does not belong to this organization")
	}
	user.PrimaryOrg = orgTag
	return s.userRepo.Update(user)
}

// GetUserOrgTags 获取用户的组织标签信息。
func (s *userService) GetUserOrgTags(username string) (map[string]interface{}, error) {
	// 这是一个简化版本。在生产环境中，可能会连接查询 organization_tags 表以获取更详细信息。
	user, err := s.userRepo.FindByUsername(context.Background(), username)
	if err != nil {
		return nil, err
	}

	orgTags := parseOrgTags(user.OrgTags)

	var orgTagDetails []map[string]string
	if len(orgTags) > 0 {
		for _, tagID := range orgTags {
			tag, err := s.orgTagRepo.FindByID(tagID)
			if err == nil { // 忽略查找失败的标签
				tagDetail := map[string]string{
					"tagId":       tag.TagID,
					"name":        tag.Name,
					"description": tag.Description,
				}
				orgTagDetails = append(orgTagDetails, tagDetail)
			}
		}
	} else {
		orgTagDetails = make([]map[string]string, 0)
	}

	result := map[string]interface{}{
		"orgTags":       orgTags,
		"primaryOrg":    user.PrimaryOrg,
		"orgTagDetails": orgTagDetails,
	}
	return result, nil
}

// GetUserEffectiveOrgTags 获取用户的所有有效组织标签 (包括层级)。
// 该实现与 Java 项目逻辑一致，会递归查询所有父级组织标签。
func (s *userService) GetUserEffectiveOrgTags(ctx context.Context, user *model.User) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if user.OrgTags == "" {
		return []string{}, nil
	}

	// 1. 获取所有组织标签，以便在内存中构建层级关系，避免循环查询数据库
	allTags, err := s.orgTagRepo.FindAll(ctx)
	if err != nil {
		log.Errorf("[UserService] 获取所有组织标签失败: %v", err)
		return nil, fmt.Errorf("无法获取组织标签列表: %w", err)
	}

	// 2. 构建一个 TagID -> ParentTagID 的快速查找映射
	parentMap := make(map[string]*string)
	for _, tag := range allTags {
		parentMap[tag.TagID] = tag.ParentTag
	}

	// 3. 使用 map 来存储所有有效的标签ID，自动处理重复项
	effectiveTags := make(map[string]struct{})

	// 4. 初始化一个队列，用于广度优先搜索所有父标签
	initialTags := parseOrgTags(user.OrgTags)
	queue := make([]string, 0, len(initialTags))

	for _, tagID := range initialTags {
		if _, exists := effectiveTags[tagID]; !exists {
			effectiveTags[tagID] = struct{}{}
			queue = append(queue, tagID)
		}
	}

	// 5. 开始向上遍历，查找所有父标签
	for len(queue) > 0 {
		// 出队
		currentTagID := queue[0]
		queue = queue[1:]

		// 在映射中查找父标签
		parentTagPtr, ok := parentMap[currentTagID]
		if ok && parentTagPtr != nil {
			parentTagID := *parentTagPtr
			// 如果父标签未被处理过，则加入结果集并入队
			if _, exists := effectiveTags[parentTagID]; !exists {
				effectiveTags[parentTagID] = struct{}{}
				queue = append(queue, parentTagID)
			}
		}
	}

	// 6. 将 map 的键转换为 string 切片
	result := make([]string, 0, len(effectiveTags))
	for tagID := range effectiveTags {
		result = append(result, tagID)
	}

	return result, nil
}

// RefreshToken 验证 refresh token 并签发新的 access token 和 refresh token。
func (s *userService) RefreshToken(refreshTokenString string) (newAccessToken, newRefreshToken string, err error) {
	// 1. 验证 refresh token 是否有效
	claims, err := s.jwtManager.VerifyRefreshToken(refreshTokenString)
	if err != nil {
		return "", "", errors.New("invalid refresh token")
	}
	if revoked, err := s.IsTokenRevoked(context.Background(), refreshTokenString); err != nil || revoked {
		return "", "", errors.New("invalid refresh token")
	}

	// 2. 检查用户是否存在
	user, err := s.userRepo.FindByUsername(context.Background(), claims.Username)
	if err != nil || user == nil || user.ID != claims.UserID {
		return "", "", errors.New("user not found")
	}

	// 3. 签发新的 token
	newAccessToken, err = s.jwtManager.GenerateToken(user.ID, user.Username, user.Role)
	if err != nil {
		return "", "", err
	}
	newRefreshToken, err = s.jwtManager.GenerateRefreshToken(user.ID, user.Username, user.Role)
	if err != nil {
		return "", "", err
	}
	if err := revokeToken(context.Background(), refreshTokenString, claims.ExpiresAt.Time, true); err != nil {
		return "", "", errors.New("invalid refresh token")
	}

	return newAccessToken, newRefreshToken, nil
}
