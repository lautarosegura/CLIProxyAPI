package management

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageledger"
	log "github.com/sirupsen/logrus"
)

const defaultUsageHistoryDays = 30

// maxUsageHistoryOffsetMinutes bounds tz_offset to real UTC offsets (UTC-14..UTC+14).
const maxUsageHistoryOffsetMinutes = 14 * 60

type usageHistoryResponse struct {
	StartedAt *time.Time             `json:"started_at"`
	Days      []usageledger.DayTotal `json:"days"`
}

type usageHistoryImportRequest struct {
	Buckets []usageledger.Bucket `json:"buckets"`
}

// GetUsageHistory returns daily token totals from the usage ledger.
// Query: days (1..400, default 30) and tz_offset (minutes east of UTC).
func (h *Handler) GetUsageHistory(c *gin.Context) {
	ledger := usageledger.Active()
	if ledger == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage ledger unavailable"})
		return
	}
	days, errDays := parseBoundedInt(c.Query("days"), defaultUsageHistoryDays, 1, usageledger.MaxQueryDays)
	if errDays != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "days must be an integer between 1 and 400"})
		return
	}
	offset, errOffset := parseBoundedInt(c.Query("tz_offset"), 0, -maxUsageHistoryOffsetMinutes, maxUsageHistoryOffsetMinutes)
	if errOffset != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tz_offset must be minutes between -840 and 840"})
		return
	}
	resp := usageHistoryResponse{Days: ledger.DailyTotals(days, time.Duration(offset)*time.Minute)}
	if startedAt := ledger.StartedAt(); !startedAt.IsZero() {
		resp.StartedAt = &startedAt
	}
	if resp.Days == nil {
		resp.Days = []usageledger.DayTotal{}
	}
	c.JSON(http.StatusOK, resp)
}

// PutUsageHistoryImport replaces all imported (pre-ledger) usage buckets.
// Buckets at or after the ledger start are dropped to avoid double counting.
func (h *Handler) PutUsageHistoryImport(c *gin.Context) {
	ledger := usageledger.Active()
	if ledger == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage ledger unavailable"})
		return
	}
	var req usageHistoryImportRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	accepted, dropped := ledger.ReplaceImported(req.Buckets)
	if errSave := ledger.Save(); errSave != nil {
		log.Errorf("usage history import: %v", errSave)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save usage history"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"accepted": accepted, "dropped": dropped})
}

func parseBoundedInt(raw string, fallback, minValue, maxValue int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	value, errParse := strconv.Atoi(raw)
	if errParse != nil {
		return 0, errParse
	}
	if value < minValue || value > maxValue {
		return 0, strconv.ErrRange
	}
	return value, nil
}
