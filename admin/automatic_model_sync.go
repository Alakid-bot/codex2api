package admin

import (
	"context"
	"log"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

const automaticModelDiscoveryTimeout = 30 * time.Second

// 自动价格轮询先发现模型，价格同步即可包含本轮新增的型号。
// 发现失败不阻断已有模型的价格更新，具体失败同时写入同步状态。
func (h *Handler) runAutomaticModelPricingSync(ctx context.Context, cfg *database.OfficialPricingSyncConfig) (*proxy.OfficialPricingSyncResult, error) {
	var warnings []string
	if cfg.IncludeOpenAI {
		warnings = h.discoverCodexModelsForPricing(ctx)
	}
	return h.runOfficialPricingSyncWithWarnings(ctx, proxy.OfficialPricingSyncOptions{
		IncludeOpenAI: cfg.IncludeOpenAI, IncludeGrok: cfg.IncludeGrok, IncludeClaude: cfg.IncludeClaude,
	}, warnings)
}

func (h *Handler) discoverCodexModelsForPricing(ctx context.Context) []string {
	discoveryCtx, cancel := context.WithTimeout(ctx, automaticModelDiscoveryTimeout)
	defer cancel()
	var warnings []string
	emit := func(event modelRefreshEvent) {
		if event.Error != "" {
			// 套餐探测并行回调；通过日志报告，汇总使用最终结果避免共享切片竞态。
			log.Printf("Codex 模型自动发现失败: plan=%s error=%s", event.Plan, event.Error)
		}
	}
	result := runChannelModelRefresh(discoveryCtx, database.UpstreamChannelCodex, h.refreshCodexChannelModels, emit)
	if result.Error != "" {
		warnings = append(warnings, result.Error)
	}
	if result.Failed > 0 {
		warnings = append(warnings, "部分 Codex 模型来源自动刷新失败，已保留现有模型；详见服务日志")
	}
	log.Printf("Codex 模型自动发现完成: added=%v refreshed=%d failed=%d", result.Added, result.Refreshed, result.Failed)
	return warnings
}
