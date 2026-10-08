package httpapi

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/hjhsamuel/cagent/internal/config"
)

func registerLoginRoute(api *gin.RouterGroup, cfg config.HTTP) {
	api.POST("/auth/login", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		now := time.Now().UTC()
		claims := Claims{TenantID: uuid.NewString(), RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(cfg.Login.TokenTTL)),
			ID:        uuid.NewString(),
		}}
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(cfg.JWT.Secret))
		if err != nil {
			failure(c, 500, "internal_error")
			return
		}
		c.JSON(200, gin.H{"token": token, "token_type": "Bearer", "expires_in": int64(cfg.Login.TokenTTL / time.Second)})
	})
}
