package api

import (
	"context"
	"fmt"
	"goprint/config"
	"log"
	"strconv"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkbitable "github.com/larksuite/oapi-sdk-go/v3/service/bitable/v1"
)

const (
	bitableFieldID             = "id"
	bitableFieldJobID          = "job_id"
	bitableFieldPrinterID      = "printer_id"
	bitableFieldFileName       = "file_name"
	bitableFieldStatus         = "status"
	bitableFieldCopies         = "copies"
	bitableFieldPageCount      = "page_count"
	bitableFieldDuplex         = "duplex"
	bitableFieldDuplexHook     = "duplex_hook"
	bitableFieldDuplexExpireAt = "duplex_expire_at"
	bitableFieldUser           = "user"
	bitableFieldSubmittedAt    = "submitted_at"
)

type bitableJobStore struct {
	client   *lark.Client
	appToken string
	tableID  string
	timeout  time.Duration
}

type printJobRecord struct {
	JobID          string
	PrinterID      string
	FileName       string
	Status         string
	Copies         int
	PageCount      int
	Duplex         bool
	DuplexHook     string
	DuplexExpireAt time.Time
	User           feishuUserInfo
}

type trackableJob struct {
	JobID       string
	PrinterID   string
	Status      string
	SubmittedAt time.Time
}

type printerActiveJobWarning struct {
	Type          string    `json:"type"`
	Message       string    `json:"message"`
	JobID         string    `json:"job_id"`
	PrinterID     string    `json:"printer_id"`
	FileName      string    `json:"file_name,omitempty"`
	Status        string    `json:"status"`
	UserName      string    `json:"user_name,omitempty"`
	SubmittedAt   string    `json:"submitted_at,omitempty"`
	HookExpiresAt string    `json:"hook_expires_at,omitempty"`
	SubmittedTime time.Time `json:"-"`
	ExpiresAt     time.Time `json:"-"`
}

func newBitableJobStore(cfg *config.Config) (*bitableJobStore, error) {
	if !cfg.JobStore.Enabled {
		return nil, fmt.Errorf("job_store is disabled")
	}

	appID := strings.TrimSpace(cfg.Auth.Feishu.AppID)
	appSecret := strings.TrimSpace(cfg.Auth.Feishu.AppSecret)
	if appID == "" || appSecret == "" {
		return nil, fmt.Errorf("auth.feishu.app_id/app_secret is required for bitable job store")
	}

	appToken := strings.TrimSpace(cfg.JobStore.Feishu.AppToken)
	tableID := strings.TrimSpace(cfg.JobStore.Feishu.TableID)
	if appToken == "" || tableID == "" {
		return nil, fmt.Errorf("job_store.feishu.app_token/table_id is required")
	}

	timeout, err := time.ParseDuration(cfg.JobStore.Feishu.RequestTimeout)
	if err != nil || timeout <= 0 {
		timeout = 10 * time.Second // 增加默认超时到 10 秒
	}

	return &bitableJobStore{
		client:   lark.NewClient(appID, appSecret),
		appToken: appToken,
		tableID:  tableID,
		timeout:  timeout,
	}, nil
}

func (s *bitableJobStore) SaveJob(ctx context.Context, record printJobRecord) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	fields := map[string]interface{}{
		bitableFieldJobID:     record.JobID,
		bitableFieldPrinterID: record.PrinterID,
		bitableFieldFileName:  record.FileName,
		bitableFieldStatus:    record.Status,
		bitableFieldCopies:    record.Copies,
		bitableFieldDuplex:    record.Duplex,
	}

	if record.PageCount > 0 {
		fields[bitableFieldPageCount] = record.PageCount
	}

	if v := strings.TrimSpace(record.DuplexHook); v != "" {
		fields[bitableFieldDuplexHook] = v
	}
	if !record.DuplexExpireAt.IsZero() {
		fields[bitableFieldDuplexExpireAt] = bitableDateTimeValue(record.DuplexExpireAt)
	}

	openID := strings.TrimSpace(record.User.OpenID)
	if openID == "" {
		return fmt.Errorf("missing open_id for bitable person field")
	}
	fields[bitableFieldUser] = []map[string]string{{"id": openID}}

	req := larkbitable.NewCreateAppTableRecordReqBuilder().
		AppToken(s.appToken).
		TableId(s.tableID).
		UserIdType(larkbitable.UserIdTypeCreateAppTableRecordOpenId).
		AppTableRecord(larkbitable.NewAppTableRecordBuilder().
			Fields(fields).
			Build()).
		Build()

	resp, err := s.client.Bitable.AppTableRecord.Create(ctx, req)
	if err != nil {
		return fmt.Errorf("bitable create record failed: %w", err)
	}
	if resp == nil || !resp.Success() {
		if resp == nil {
			return fmt.Errorf("bitable create record failed: empty response")
		}
		return fmt.Errorf("bitable create record failed: code=%d msg=%s request_id=%s", resp.Code, resp.Msg, resp.RequestId())
	}

	return nil
}

func (s *bitableJobStore) ListTrackableJobs(ctx context.Context) ([]trackableJob, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	out := make([]trackableJob, 0)
	pageToken := ""

	for {
		reqBuilder := larkbitable.NewListAppTableRecordReqBuilder().
			AppToken(s.appToken).
			TableId(s.tableID).
			PageSize(100)
		if pageToken != "" {
			reqBuilder = reqBuilder.PageToken(pageToken)
		}

		resp, err := s.client.Bitable.AppTableRecord.List(ctx, reqBuilder.Build())
		if err != nil {
			return nil, fmt.Errorf("bitable list records failed: %w", err)
		}
		if resp == nil || !resp.Success() || resp.Data == nil {
			if resp == nil {
				return nil, fmt.Errorf("bitable list records failed: empty response")
			}
			return nil, fmt.Errorf("bitable list records failed: code=%d msg=%s", resp.Code, resp.Msg)
		}

		for _, item := range resp.Data.Items {
			if item == nil {
				continue
			}
			jobID := strings.TrimSpace(fieldAsString(item.Fields[bitableFieldJobID]))
			printerID := strings.TrimSpace(fieldAsString(item.Fields[bitableFieldPrinterID]))
			status := normalizedJobStatus(fieldAsString(item.Fields[bitableFieldStatus]))
			if jobID == "" || printerID == "" {
				continue
			}
			switch status {
			case "pending", "held", "processing", "pending_manual_continue":
				submittedAt, _ := fieldAsTime(item.Fields[bitableFieldSubmittedAt])
				out = append(out, trackableJob{
					JobID:       jobID,
					PrinterID:   printerID,
					Status:      status,
					SubmittedAt: submittedAt,
				})
			}
		}

		if resp.Data.HasMore == nil || !*resp.Data.HasMore || resp.Data.PageToken == nil || *resp.Data.PageToken == "" {
			break
		}
		pageToken = *resp.Data.PageToken
	}

	return out, nil
}

func (s *bitableJobStore) ListStaleIncompleteJobs(ctx context.Context, cutoff time.Time) ([]trackableJob, error) {
	jobs, err := s.ListTrackableJobs(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]trackableJob, 0)
	for _, job := range jobs {
		if job.SubmittedAt.IsZero() || !job.SubmittedAt.Before(cutoff) {
			continue
		}
		switch normalizedJobStatus(job.Status) {
		case "pending", "pending_manual_continue":
			out = append(out, job)
		}
	}
	return out, nil
}

func (s *bitableJobStore) ActiveWarningForPrinter(ctx context.Context, printerID string, now time.Time) (*printerActiveJobWarning, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	printerID = strings.TrimSpace(printerID)
	if printerID == "" {
		return nil, fmt.Errorf("printer_id is required")
	}
	if now.IsZero() {
		now = time.Now()
	}

	var printingWarning *printerActiveJobWarning
	var manualWarning *printerActiveJobWarning
	pageToken := ""

	for {
		reqBuilder := larkbitable.NewListAppTableRecordReqBuilder().
			AppToken(s.appToken).
			TableId(s.tableID).
			UserIdType(larkbitable.UserIdTypeListAppTableRecordOpenId).
			PageSize(100)
		if pageToken != "" {
			reqBuilder = reqBuilder.PageToken(pageToken)
		}

		resp, err := s.client.Bitable.AppTableRecord.List(ctx, reqBuilder.Build())
		if err != nil {
			return nil, fmt.Errorf("bitable list records failed: %w", err)
		}
		if resp == nil || !resp.Success() || resp.Data == nil {
			if resp == nil {
				return nil, fmt.Errorf("bitable list records failed: empty response")
			}
			return nil, fmt.Errorf("bitable list records failed: code=%d msg=%s request_id=%s", resp.Code, resp.Msg, resp.RequestId())
		}

		for _, item := range resp.Data.Items {
			if item == nil || item.Fields == nil {
				continue
			}
			if strings.TrimSpace(fieldAsString(item.Fields[bitableFieldPrinterID])) != printerID {
				continue
			}

			status := normalizedJobStatus(fieldAsString(item.Fields[bitableFieldStatus]))
			switch status {
			case "pending":
				warning := mapRecordToActiveJobWarning(item.Fields, "printing")
				if newerActiveWarning(warning, printingWarning) {
					printingWarning = warning
				}
			case "pending_manual_continue":
				duplexHook := fieldAsString(item.Fields[bitableFieldDuplexHook])
				if duplexHook == "" {
					continue
				}
				expiresAt, ok := calcDuplexHookExpiresAt(item.Fields)
				if !ok || !expiresAt.After(now) {
					continue
				}
				warning := mapRecordToActiveJobWarning(item.Fields, "manual_duplex")
				warning.ExpiresAt = expiresAt
				warning.HookExpiresAt = expiresAt.In(time.Local).Format("2006-01-02 15:04")
				if newerActiveWarning(warning, manualWarning) {
					manualWarning = warning
				}
			}
		}

		if resp.Data.HasMore == nil || !*resp.Data.HasMore || resp.Data.PageToken == nil || *resp.Data.PageToken == "" {
			break
		}
		pageToken = *resp.Data.PageToken
	}

	if manualWarning != nil {
		return manualWarning, nil
	}
	return printingWarning, nil
}

func (s *bitableJobStore) ListJobsByUser(ctx context.Context, user feishuUserInfo, limit int) ([]map[string]interface{}, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	if limit <= 0 {
		limit = 200
	}

	jobs := make([]map[string]interface{}, 0)
	pageToken := ""

	for len(jobs) < limit {
		reqBuilder := larkbitable.NewListAppTableRecordReqBuilder().
			AppToken(s.appToken).
			TableId(s.tableID).
			UserIdType(larkbitable.UserIdTypeListAppTableRecordOpenId).
			PageSize(100)
		if pageToken != "" {
			reqBuilder = reqBuilder.PageToken(pageToken)
		}

		resp, err := s.client.Bitable.AppTableRecord.List(ctx, reqBuilder.Build())
		if err != nil {
			return nil, fmt.Errorf("bitable list records failed: %w", err)
		}
		if resp == nil || !resp.Success() || resp.Data == nil {
			if resp == nil {
				return nil, fmt.Errorf("bitable list records failed: empty response")
			}
			return nil, fmt.Errorf("bitable list records failed: code=%d msg=%s request_id=%s", resp.Code, resp.Msg, resp.RequestId())
		}

		for _, item := range resp.Data.Items {
			if item == nil {
				continue
			}
			if !isOwnedByUser(item.Fields, user) {
				continue
			}
			jobs = append(jobs, mapRecordToJob(item))
			if len(jobs) >= limit {
				break
			}
		}

		if resp.Data.HasMore == nil || !*resp.Data.HasMore || resp.Data.PageToken == nil || *resp.Data.PageToken == "" {
			break
		}
		pageToken = *resp.Data.PageToken
	}

	return jobs, nil
}

func isOwnedByUser(fields map[string]interface{}, user feishuUserInfo) bool {
	if fields == nil {
		return false
	}

	personIDs := fieldAsPersonIDs(fields[bitableFieldUser])
	if len(personIDs) == 0 {
		return false
	}
	expectedID := strings.TrimSpace(user.OpenID)
	if expectedID == "" {
		return false
	}
	for _, id := range personIDs {
		if id == expectedID {
			return true
		}
	}
	return false
}

func mapRecordToJob(record *larkbitable.AppTableRecord) map[string]interface{} {
	fields := record.Fields
	submittedAt := fieldAsBitableDateTime(fields[bitableFieldSubmittedAt])
	duplexHook := fieldAsString(fields[bitableFieldDuplexHook])
	hookExpiresAt := ""
	if duplexHook != "" {
		if expiresAt, ok := calcDuplexHookExpiresAt(fields); ok {
			hookExpiresAt = expiresAt.In(time.Local).Format("2006-01-02 15:04")
		}
	}

	job := map[string]interface{}{
		"id":           fieldAsInt(fields[bitableFieldID]),
		"printer_id":   fieldAsString(fields[bitableFieldPrinterID]),
		"file_name":    fieldAsString(fields[bitableFieldFileName]),
		"status":       fieldAsString(fields[bitableFieldStatus]),
		"copies":       fieldAsInt(fields[bitableFieldCopies]),
		"page_count":   fieldAsInt(fields[bitableFieldPageCount]),
		"duplex":       fieldAsBool(fields[bitableFieldDuplex]),
		"submitted_at": submittedAt,
		"duplex_hook":  duplexHook,
	}

	if hookExpiresAt != "" {
		job["hook_expires_at"] = hookExpiresAt
		job["hook_extend_window_seconds"] = manualDuplexExtendWindowSeconds()
	}

	return job
}

func mapRecordToActiveJobWarning(fields map[string]interface{}, warningType string) *printerActiveJobWarning {
	submittedAt, _ := fieldAsTime(fields[bitableFieldSubmittedAt])
	warning := &printerActiveJobWarning{
		Type:          warningType,
		JobID:         fieldAsString(fields[bitableFieldJobID]),
		PrinterID:     fieldAsString(fields[bitableFieldPrinterID]),
		FileName:      fieldAsString(fields[bitableFieldFileName]),
		Status:        normalizedJobStatus(fieldAsString(fields[bitableFieldStatus])),
		UserName:      fieldAsPersonName(fields[bitableFieldUser]),
		SubmittedTime: submittedAt,
	}
	if !submittedAt.IsZero() {
		warning.SubmittedAt = submittedAt.In(time.Local).Format("2006-01-02 15:04")
	}
	warning.Message = formatActiveJobWarningMessage(warning)
	return warning
}

func formatActiveJobWarningMessage(warning *printerActiveJobWarning) string {
	if warning == nil {
		return ""
	}
	name := strings.TrimSpace(warning.UserName)
	if name == "" {
		name = "有人"
	}
	action := "正在打印"
	if warning.Type == "manual_duplex" {
		action = "正在进行手动双面打印翻面"
	}
	return fmt.Sprintf("%s%s。请去对应打印机出纸口观察是否有未打印完成的页面并提醒对方及时拿取文件。这也可能是误判。", name, action)
}

func newerActiveWarning(candidate, current *printerActiveJobWarning) bool {
	if candidate == nil {
		return false
	}
	if current == nil {
		return true
	}
	if !candidate.ExpiresAt.IsZero() || !current.ExpiresAt.IsZero() {
		if current.ExpiresAt.IsZero() {
			return true
		}
		if candidate.ExpiresAt.IsZero() {
			return false
		}
		return candidate.ExpiresAt.After(current.ExpiresAt)
	}
	if current.SubmittedTime.IsZero() {
		return true
	}
	if candidate.SubmittedTime.IsZero() {
		return false
	}
	return candidate.SubmittedTime.After(current.SubmittedTime)
}

func normalizedJobStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	return strings.ReplaceAll(status, "-", "_")
}

func calcDuplexHookExpiresAt(fields map[string]interface{}) (time.Time, bool) {
	if fields == nil {
		return time.Time{}, false
	}

	if expiresAt, ok := fieldAsTime(fields[bitableFieldDuplexExpireAt]); ok {
		return expiresAt, true
	}

	if token := manualDuplexTokenFromHook(fieldAsString(fields[bitableFieldDuplexHook])); token != "" {
		if pending, ok := manualDuplexPendingByToken(token); ok {
			return pending.ExpiresAt, true
		}
	}

	submittedAt, ok := fieldAsTime(fields[bitableFieldSubmittedAt])
	if !ok {
		return time.Time{}, false
	}

	printerID := strings.TrimSpace(fieldAsString(fields[bitableFieldPrinterID]))
	pageCount := fieldAsInt(fields[bitableFieldPageCount])
	copies := fieldAsInt(fields[bitableFieldCopies])
	if copies > 1 {
		pageCount *= copies
	}
	return submittedAt.Add(manualDuplexTimeoutForPrinterID(printerID, pageCount)), true
}

func bitableDateTimeValue(t time.Time) int64 {
	return t.UnixMilli()
}

func manualDuplexTokenFromHook(hook string) string {
	hook = strings.TrimSpace(hook)
	if hook == "" {
		return ""
	}

	if strings.HasPrefix(hook, "bot://manual-duplex/") {
		return strings.TrimSpace(strings.TrimPrefix(hook, "bot://manual-duplex/"))
	}

	const marker = "/manual-duplex-hooks/"
	idx := strings.Index(hook, marker)
	if idx < 0 {
		return ""
	}

	token := hook[idx+len(marker):]
	token = strings.TrimPrefix(token, "/")
	if slash := strings.Index(token, "/"); slash >= 0 {
		token = token[:slash]
	}
	return strings.TrimSpace(token)
}

func fieldAsTime(v interface{}) (time.Time, bool) {
	switch val := v.(type) {
	case float64:
		return time.UnixMilli(int64(val)).In(time.Local), true
	case int64:
		return time.UnixMilli(val).In(time.Local), true
	case int:
		return time.UnixMilli(int64(val)).In(time.Local), true
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return time.Time{}, false
		}
		if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.UnixMilli(ms).In(time.Local), true
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.In(time.Local), true
		}
		if t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func fieldAsBitableDateTime(v interface{}) string {
	const outputLayout = "2006-01-02 15:04"

	switch val := v.(type) {
	case float64:
		return time.UnixMilli(int64(val)).In(time.Local).Format(outputLayout)
	case int64:
		return time.UnixMilli(val).In(time.Local).Format(outputLayout)
	case int:
		return time.UnixMilli(int64(val)).In(time.Local).Format(outputLayout)
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return ""
		}

		if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.UnixMilli(ms).In(time.Local).Format(outputLayout)
		}

		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.In(time.Local).Format(outputLayout)
		}

		return s
	default:
		return ""
	}
}

func fieldAsPersonIDs(v interface{}) []string {
	ids := make([]string, 0)
	switch val := v.(type) {
	case []interface{}:
		for _, item := range val {
			if m, ok := item.(map[string]interface{}); ok {
				id := fieldAsString(m["id"])
				if id != "" {
					ids = append(ids, id)
				}
			}
		}
	case map[string]interface{}:
		if id := fieldAsString(val["id"]); id != "" {
			ids = append(ids, id)
		}
	case []map[string]interface{}:
		for _, m := range val {
			id := fieldAsString(m["id"])
			if id != "" {
				ids = append(ids, id)
			}
		}
	case map[string]string:
		if id := strings.TrimSpace(val["id"]); id != "" {
			ids = append(ids, id)
		}
	case []map[string]string:
		for _, m := range val {
			id := strings.TrimSpace(m["id"])
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func fieldAsPersonName(v interface{}) string {
	switch val := v.(type) {
	case []interface{}:
		for _, item := range val {
			if m, ok := item.(map[string]interface{}); ok {
				if name := fieldAsString(m["name"]); name != "" {
					return name
				}
				if name := fieldAsString(m["en_name"]); name != "" {
					return name
				}
				if id := fieldAsString(m["id"]); id != "" {
					return id
				}
			}
		}
	case map[string]interface{}:
		if name := fieldAsString(val["name"]); name != "" {
			return name
		}
		if name := fieldAsString(val["en_name"]); name != "" {
			return name
		}
		if id := fieldAsString(val["id"]); id != "" {
			return id
		}
	case []map[string]interface{}:
		for _, m := range val {
			if name := fieldAsString(m["name"]); name != "" {
				return name
			}
			if name := fieldAsString(m["en_name"]); name != "" {
				return name
			}
			if id := fieldAsString(m["id"]); id != "" {
				return id
			}
		}
	case map[string]string:
		if name := strings.TrimSpace(val["name"]); name != "" {
			return name
		}
		if name := strings.TrimSpace(val["en_name"]); name != "" {
			return name
		}
		if id := strings.TrimSpace(val["id"]); id != "" {
			return id
		}
	case []map[string]string:
		for _, m := range val {
			if name := strings.TrimSpace(m["name"]); name != "" {
				return name
			}
			if name := strings.TrimSpace(m["en_name"]); name != "" {
				return name
			}
			if id := strings.TrimSpace(m["id"]); id != "" {
				return id
			}
		}
	default:
		return fieldAsString(v)
	}
	return ""
}

func fieldAsString(v interface{}) string {
	switch val := v.(type) {
	case string:
		return strings.TrimSpace(val)
	case fmt.Stringer:
		return strings.TrimSpace(val.String())
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	default:
		return ""
	}
}

func fieldAsInt(v interface{}) int {
	switch val := v.(type) {
	case int:
		return val
	case int64:
		return int(val)
	case float64:
		return int(val)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(val))
		return n
	default:
		return 0
	}
}

func fieldAsBool(v interface{}) bool {
	switch val := v.(type) {
	case bool:
		return val
	case string:
		b, _ := strconv.ParseBool(strings.TrimSpace(val))
		return b
	default:
		return false
	}
}

// UpdateJobStatus 根据 job_id 更新任务状态
func (s *bitableJobStore) UpdateJobStatus(ctx context.Context, jobID string, newStatus string) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	return s.updateJobFields(ctx, jobID, map[string]interface{}{
		bitableFieldStatus: newStatus,
	})
}

func (s *bitableJobStore) UpdateManualDuplexContinued(ctx context.Context, initialJobID string, continuedJobID string) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	continuedJobID = strings.TrimSpace(continuedJobID)
	if continuedJobID == "" {
		return fmt.Errorf("continued job_id is required")
	}

	return s.updateJobFields(ctx, initialJobID, map[string]interface{}{
		bitableFieldJobID:      continuedJobID,
		bitableFieldStatus:     "pending",
		bitableFieldDuplexHook: "",
	})
}

func (s *bitableJobStore) UpdateManualDuplexExpireAt(ctx context.Context, jobID string, expiresAt time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	if expiresAt.IsZero() {
		return fmt.Errorf("duplex expire_at is required")
	}

	return s.updateJobFields(ctx, jobID, map[string]interface{}{
		bitableFieldDuplexExpireAt: bitableDateTimeValue(expiresAt),
	})
}

func (s *bitableJobStore) updateJobFields(ctx context.Context, jobID string, fields map[string]interface{}) error {
	maxRetries := 3
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		recordID, err := s.findRecordIDByJobID(ctx, jobID)
		if err != nil {
			lastErr = err
			if attempt < maxRetries {
				log.Printf("[bitable] find record attempt %d/%d failed job_id=%s err=%v", attempt, maxRetries, jobID, err)
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return fmt.Errorf("failed to find record after %d attempts: %w", maxRetries, err)
		}

		req := larkbitable.NewUpdateAppTableRecordReqBuilder().
			AppToken(s.appToken).
			TableId(s.tableID).
			RecordId(recordID).
			AppTableRecord(larkbitable.NewAppTableRecordBuilder().
				Fields(fields).
				Build()).
			Build()

		updateResp, err := s.client.Bitable.AppTableRecord.Update(ctx, req)
		if err != nil {
			lastErr = err
			if attempt < maxRetries {
				log.Printf("[bitable] update attempt %d/%d failed job_id=%s err=%v", attempt, maxRetries, jobID, err)
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return fmt.Errorf("bitable update record failed after %d attempts: %w", maxRetries, err)
		}
		if updateResp == nil || !updateResp.Success() {
			if updateResp == nil {
				lastErr = fmt.Errorf("empty response")
			} else {
				lastErr = fmt.Errorf("code=%d msg=%s request_id=%s", updateResp.Code, updateResp.Msg, updateResp.RequestId())
			}

			if attempt < maxRetries {
				log.Printf("[bitable] update attempt %d/%d rejected job_id=%s err=%v", attempt, maxRetries, jobID, lastErr)
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return fmt.Errorf("bitable update record rejected after %d attempts: %w", maxRetries, lastErr)
		}

		if attempt > 1 {
			log.Printf("[bitable] update success after retry job_id=%s attempts=%d", jobID, attempt)
		}
		return nil
	}

	return fmt.Errorf("failed after %d attempts: %w", maxRetries, lastErr)
}

func (s *bitableJobStore) findRecordIDByJobID(ctx context.Context, jobID string) (string, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return "", fmt.Errorf("job_id is required")
	}

	pageToken := ""

	for {
		reqBuilder := larkbitable.NewListAppTableRecordReqBuilder().
			AppToken(s.appToken).
			TableId(s.tableID).
			PageSize(100)
		if pageToken != "" {
			reqBuilder = reqBuilder.PageToken(pageToken)
		}

		resp, err := s.client.Bitable.AppTableRecord.List(ctx, reqBuilder.Build())
		if err != nil {
			return "", fmt.Errorf("bitable list records failed: %w", err)
		}
		if resp == nil || !resp.Success() || resp.Data == nil {
			if resp == nil {
				return "", fmt.Errorf("bitable list records failed: empty response")
			}
			return "", fmt.Errorf("bitable list records failed: code=%d msg=%s request_id=%s", resp.Code, resp.Msg, resp.RequestId())
		}

		for _, item := range resp.Data.Items {
			if item == nil {
				continue
			}
			if fieldAsString(item.Fields[bitableFieldJobID]) == jobID {
				if item.RecordId != nil {
					return *item.RecordId, nil
				}
				break
			}
		}

		if resp.Data.HasMore == nil || !*resp.Data.HasMore || resp.Data.PageToken == nil || *resp.Data.PageToken == "" {
			break
		}
		pageToken = *resp.Data.PageToken
	}

	return "", fmt.Errorf("job_id not found in bitable: %s", jobID)
}

// DeleteJobByUserAndJobID 根据当前用户和 job_id 删除多维表中的任务记录。
func (s *bitableJobStore) DeleteJobByUserAndJobID(ctx context.Context, user feishuUserInfo, jobID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return false, fmt.Errorf("job_id is required")
	}

	recordID := ""
	pageToken := ""

	for recordID == "" {
		reqBuilder := larkbitable.NewListAppTableRecordReqBuilder().
			AppToken(s.appToken).
			TableId(s.tableID).
			UserIdType(larkbitable.UserIdTypeListAppTableRecordOpenId).
			PageSize(100)
		if pageToken != "" {
			reqBuilder = reqBuilder.PageToken(pageToken)
		}

		resp, err := s.client.Bitable.AppTableRecord.List(ctx, reqBuilder.Build())
		if err != nil {
			return false, fmt.Errorf("bitable list records failed: %w", err)
		}
		if resp == nil || !resp.Success() || resp.Data == nil {
			if resp == nil {
				return false, fmt.Errorf("bitable list records failed: empty response")
			}
			return false, fmt.Errorf("bitable list records failed: code=%d msg=%s request_id=%s", resp.Code, resp.Msg, resp.RequestId())
		}

		for _, item := range resp.Data.Items {
			if item == nil || item.Fields == nil {
				continue
			}
			if !isOwnedByUser(item.Fields, user) {
				continue
			}
			if fieldAsString(item.Fields[bitableFieldJobID]) != jobID {
				continue
			}
			if item.RecordId != nil {
				recordID = *item.RecordId
			}
			break
		}

		if recordID != "" {
			break
		}

		if resp.Data.HasMore == nil || !*resp.Data.HasMore || resp.Data.PageToken == nil || *resp.Data.PageToken == "" {
			break
		}
		pageToken = *resp.Data.PageToken
	}

	if recordID == "" {
		return false, nil
	}

	delReq := larkbitable.NewDeleteAppTableRecordReqBuilder().
		AppToken(s.appToken).
		TableId(s.tableID).
		RecordId(recordID).
		Build()

	delResp, err := s.client.Bitable.AppTableRecord.Delete(ctx, delReq)
	if err != nil {
		return false, fmt.Errorf("bitable delete record failed: %w", err)
	}
	if delResp == nil || !delResp.Success() {
		if delResp == nil {
			return false, fmt.Errorf("bitable delete record failed: empty response")
		}
		return false, fmt.Errorf("bitable delete record failed: code=%d msg=%s request_id=%s", delResp.Code, delResp.Msg, delResp.RequestId())
	}

	return true, nil
}
