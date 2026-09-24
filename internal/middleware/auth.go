// Package middleware 提供了处理 HTTP 请求的中间件。
package middleware

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"pai-smart-go/internal/service"
	"pai-smart-go/pkg/token"
	"strings"
)

// AuthMiddleware 创建一个 Gin 中间件，用于 JWT 认证。
// 它会从请求头中提取 token，验证其有效性，并将完整的 User 对象存入 Gin 的上下文中。
func AuthMiddleware(jwtManager *token.JWTManager, userService service.UserService) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 从 Authorization 请求头中获取 token
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			// 如果请求头为空，则中止请求，返回未授权状态
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "请求未包含授权头"})
			return
		}

		// Token 通常以 "Bearer <token>" 的形式提供，我们需要提取出 token 本身
		const bearerPrefix = "Bearer "
		if !strings.HasPrefix(authHeader, bearerPrefix) {
			// 如果 token 格式不正确，则返回错误
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效的授权头格式"})
			return
		}
		tokenString := strings.TrimPrefix(authHeader, bearerPrefix)

		claims, err := jwtManager.VerifyAccessToken(tokenString)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "无效或已过期的 token"})
			return
		}
		revoked, err := userService.IsTokenRevoked(c.Request.Context(), tokenString)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "认证服务暂时不可用"})
			return
		}
		if revoked {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token 已失效"})
			return
		}

		// 使用 claims 中的用户名从数据库获取完整的用户信息
		user, err := userService.GetProfile(c.Request.Context(), claims.Username)
		if err != nil || user == nil || user.ID != claims.UserID {
			// 如果根据 token 中的用户信息无法找到用户，说明该用户可能已被删除
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "用户不存在"})
			return
		}

		// 将完整的 User 对象存储在 context 中，供后续处理函数使用
		c.Set("user", user)

		// 为了向后兼容或特殊用途，仍然可以存储 claims
		c.Set("claims", claims)

		// 继续处理请求链中的下一个处理器
		c.Next()
	}
}
