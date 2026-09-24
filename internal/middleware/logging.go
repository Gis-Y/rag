// Package middleware 存放 Gin 框架的中间件。
package middleware

import (
	"github.com/gin-gonic/gin"
	"pai-smart-go/pkg/log"
	"time"
)

// RequestLogger records metadata only. Reading a body here would bypass the
// handler's upload limit and expose document content, passwords or response tokens.
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		startTime := time.Now()
		c.Next()

		// Log route templates, not URL paths/query strings containing credentials
		// such as /chat/:ticket. Unmatched paths are also untrusted input.
		path := c.FullPath()
		if path == "" {
			path = "<unmatched>"
		}
		log.Infow("HTTP Request Log",
			"statusCode", c.Writer.Status(),
			"latency", time.Since(startTime).String(),
			"clientIP", c.ClientIP(),
			"method", c.Request.Method,
			"path", path,
			"requestBytes", c.Request.ContentLength,
			"responseBytes", c.Writer.Size(),
		)
	}
}
