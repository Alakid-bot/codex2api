package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// Explicit response types prevent persistence fields and image caches from
// leaking into this lightweight polling API when the database model grows.
type externalImageJobResult struct {
	ID           int64                         `json:"id"`
	Status       string                        `json:"status"`
	Assets       []externalImageJobResultAsset `json:"assets"`
	ErrorMessage string                        `json:"error_message"`
	Warning      string                        `json:"warning,omitempty"`
	DurationMs   int                           `json:"duration_ms"`
	CreatedAt    time.Time                     `json:"created_at"`
	StartedAt    *time.Time                    `json:"started_at,omitempty"`
	CompletedAt  *time.Time                    `json:"completed_at,omitempty"`
}

type externalImageJobResultAsset struct {
	ID           int64  `json:"id"`
	ProxyURL     string `json:"proxy_url"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
	MimeType     string `json:"mime_type"`
	Bytes        int    `json:"bytes"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Model        string `json:"model"`
	OutputFormat string `json:"output_format"`
}

// GetExternalImageJobResult returns only status and output metadata. Inputs and
// cached output Base64 are omitted, including when include_cache=1 is supplied.
func (h *Handler) GetExternalImageJobResult(c *gin.Context) {
	apiKey := proxy.APIKeyRowFromContext(c)
	if apiKey == nil {
		writeExternalImageError(c, http.StatusUnauthorized, "Missing or invalid API key")
		return
	}
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeExternalImageError(c, http.StatusBadRequest, "Invalid request: invalid job id")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	job, err := h.db.GetImageGenerationJobResult(ctx, id, apiKey.ID)
	if errors.Is(err, sql.ErrNoRows) {
		writeExternalImageError(c, http.StatusNotFound, "Image job not found")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	decorateImageJobAssets(job)
	c.JSON(http.StatusOK, gin.H{"job": imageJobResultPayload(job)})
}

func imageJobResultPayload(job *database.ImageGenerationJob) externalImageJobResult {
	assets := make([]externalImageJobResultAsset, 0, len(job.Assets))
	for _, asset := range job.Assets {
		assets = append(assets, externalImageJobResultAsset{
			ID: asset.ID, ProxyURL: asset.ProxyURL, ThumbnailURL: asset.ThumbnailURL,
			MimeType: asset.MimeType, Bytes: asset.Bytes, Width: asset.Width, Height: asset.Height,
			Model: asset.Model, OutputFormat: asset.OutputFormat,
		})
	}
	return externalImageJobResult{
		ID: job.ID, Status: job.Status, Assets: assets, ErrorMessage: job.ErrorMessage,
		Warning: job.Warning, DurationMs: job.DurationMs, CreatedAt: job.CreatedAt,
		StartedAt: job.StartedAt, CompletedAt: job.CompletedAt,
	}
}
