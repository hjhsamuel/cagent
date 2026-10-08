package httpapi

import (
	"context"
	"encoding/json"
	"io"

	"github.com/gin-gonic/gin"
	"github.com/hjhsamuel/cagent/internal/config"
)

type ModelManagement interface {
	List(context.Context) ([]config.ModelView, error)
	Get(context.Context, string) (config.ModelView, error)
	Put(context.Context, string, config.ModelInput) (config.ModelView, error)
	Delete(context.Context, string) error
}

func registerModelRoutes(admin *gin.RouterGroup, models ModelManagement) {
	admin.GET("/models", func(c *gin.Context) {
		items, err := models.List(c.Request.Context())
		if err != nil {
			respondError(c, err)
			return
		}
		c.JSON(200, gin.H{"models": items})
	})
	admin.GET("/models/:modelID", func(c *gin.Context) {
		item, err := models.Get(c.Request.Context(), c.Param("modelID"))
		if err != nil {
			respondError(c, err)
			return
		}
		c.JSON(200, item)
	})
	admin.PUT("/models/:modelID", func(c *gin.Context) {
		var body config.ModelInput
		dec := json.NewDecoder(c.Request.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			respondSessionInputError(c, err)
			return
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			respondSessionInputError(c, err)
			return
		}
		item, err := models.Put(c.Request.Context(), c.Param("modelID"), body)
		if err != nil {
			respondError(c, err)
			return
		}
		c.JSON(200, item)
	})
	admin.DELETE("/models/:modelID", func(c *gin.Context) {
		if err := models.Delete(c.Request.Context(), c.Param("modelID")); err != nil {
			respondError(c, err)
			return
		}
		c.Status(204)
	})
}
