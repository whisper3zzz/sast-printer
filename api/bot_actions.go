package api

import (
	"context"
	"encoding/json"
	"fmt"
	"goprint/api/conversion"
	"goprint/api/pdfutil"
	"goprint/config"
	"goprint/cups"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func processMessageEvent(cfg *config.Config, event *larkim.P2MessageReceiveV1) {
	msg := event.Event.Message
	if msg == nil {
		return
	}
	chatID := ptrStr(msg.ChatId)
	chatType := ptrStr(msg.ChatType)
	msgType := ptrStr(msg.MessageType)
	contentJSON := ptrStr(msg.Content)
	requesterOpenID := ""
	if event.Event.Sender != nil && event.Event.Sender.SenderId != nil {
		requesterOpenID = ptrStr(event.Event.Sender.SenderId.OpenId)
	}

	// For p2p chats, reply via sender's open_id; for group chats, reply via chat_id.
	if chatType == "p2p" {
		if requesterOpenID != "" {
			chatID = requesterOpenID
		}
	}
	messageID := ptrStr(msg.MessageId)

	var content struct {
		FileKey  string `json:"file_key"`
		FileName string `json:"file_name"`
		Text     string `json:"text"`
	}
	if err := json.Unmarshal([]byte(contentJSON), &content); err != nil {
		_ = sendBotText(context.Background(), cfg, chatID, chatType, requesterOpenID, "无法解析消息", messageID)
		return
	}

	var sourcePath string
	var filename string
	var cleanup func()
	var isCloudDoc bool

	switch msgType {
	case "file":
		if !conversion.IsSupportedUploadFile(cfg, content.FileName) {
			_ = sendBotText(context.Background(), cfg, chatID, chatType, requesterOpenID, "不支持的文件类型，请发送 PDF、Office 文档（doc/docx/ppt/pptx）或图片（jpg/png）", messageID)
			return
		}
		path, fn, cl, err := downloadBotFile(context.Background(), cfg, ptrStr(msg.MessageId), content.FileKey, content.FileName)
		if err != nil {
			log.Printf("[bot] download file failed: %v", err)
			_ = sendBotText(context.Background(), cfg, chatID, chatType, requesterOpenID, fmt.Sprintf("下载文件失败：%v", err), messageID)
			return
		}
		sourcePath, filename, cleanup = path, fn, cl

		// Convert office docs and images to PDF (same pipeline as web flow)
		if conversion.IsOfficeConvertible(cfg, filename) {
			pdfPath, convErr := conversion.ConvertOfficeToPDF(context.Background(), cfg, sourcePath)
			if convErr != nil {
				log.Printf("[bot] office convert failed: %v", convErr)
				_ = sendBotText(context.Background(), cfg, chatID, chatType, requesterOpenID, "文档转换失败，请确保文件格式正确", messageID)
				cleanup()
				return
			}
			oldCleanup := cleanup
			sourcePath = pdfPath
			cleanup = func() { _ = os.Remove(pdfPath); oldCleanup() }
		} else if conversion.IsImageConvertible(filename) {
			pdfPath, convErr := conversion.ConvertImageToPDF(cfg, sourcePath)
			if convErr != nil {
				log.Printf("[bot] image convert failed: %v", convErr)
				_ = sendBotText(context.Background(), cfg, chatID, chatType, requesterOpenID, "图片转换失败", messageID)
				cleanup()
				return
			}
			oldCleanup := cleanup
			sourcePath = pdfPath
			cleanup = func() { _ = os.Remove(pdfPath); oldCleanup() }
		}

	case "text":
		raw := strings.TrimSpace(content.Text)
		if raw == "" {
			return
		}
		client, cliErr := newFeishuClient(cfg)
		if cliErr != nil {
			_ = sendBotText(context.Background(), cfg, chatID, chatType, requesterOpenID, "内部错误", messageID)
			return
		}
		pdfPath, docFilename, exportErr := exportFeishuDocToPDF(context.Background(), cfg, client, "", raw)
		if exportErr != nil {
			_ = sendBotText(context.Background(), cfg, chatID, chatType, requesterOpenID, fmt.Sprintf("导出失败：%v", exportErr), messageID)
			return
		}
		sourcePath, filename = pdfPath, docFilename
		cleanup = func() { _ = os.Remove(pdfPath) }
		isCloudDoc = true

	default:
		_ = sendBotText(context.Background(), cfg, chatID, chatType, requesterOpenID, "请发送 PDF/Office 文档/图片文件，或飞书云文档链接", messageID)
		return
	}
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()

	pages, err := countPDFPages(sourcePath)
	if err != nil {
		_ = sendBotText(context.Background(), cfg, chatID, chatType, requesterOpenID, "无法读取文件页数", messageID)
		return
	}
	if err := validatePDFPageLimit(cfg, pages); err != nil {
		_ = sendBotText(context.Background(), cfg, chatID, chatType, requesterOpenID, fmt.Sprintf("文件页数超出限制：%v", err), messageID)
		return
	}

	printers := buildPrinterOptions(cfg)
	if len(printers) == 0 {
		_ = sendBotText(context.Background(), cfg, chatID, chatType, requesterOpenID, "没有可用的打印机", messageID)
		return
	}

	sessionID := fmt.Sprintf("%s-%d", chatID, time.Now().UnixNano())

	card, err := buildPrinterSelectCard(filename, pages, printers, sessionID)
	if err != nil {
		_ = sendBotText(context.Background(), cfg, chatID, chatType, requesterOpenID, "构建卡片失败", messageID)
		return
	}

	delivery, err := sendBotCard(context.Background(), cfg, chatID, chatType, requesterOpenID, card, messageID)
	if err != nil {
		log.Printf("[bot] send card failed: %v", err)
		_ = sendBotText(context.Background(), cfg, chatID, chatType, requesterOpenID, "发送卡片失败，请重试", messageID)
		return
	}

	persistedPath, persistErr := persistSessionFile(sourcePath)
	if persistErr != nil {
		log.Printf("[bot] persist session file: %v", persistErr)
		_ = sendBotText(context.Background(), cfg, chatID, chatType, requesterOpenID, "保存文件失败，请重试", messageID)
		return
	}
	saveBotSession(sessionID, botCardSession{
		ReplyMessageID:     messageID,
		SourcePath:         persistedPath,
		Filename:           filename,
		IsCloudDoc:         isCloudDoc,
		TotalPages:         pages,
		PrinterID:          printers[0].ID,
		ChatID:             chatID,
		RequesterOpenID:    requesterOpenID,
		CardID:             delivery.CardID,
		EphemeralMessageID: delivery.EphemeralMessageID,
		ChatType:           chatType,
		CreatedAt:          time.Now(),
	})
}

func downloadBotFile(ctx context.Context, cfg *config.Config, messageID, fileKey, fileName string) (string, string, func(), error) {
	client, err := newFeishuClient(cfg)
	if err != nil {
		return "", "", nil, err
	}

	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(fileKey).
		Type("file").
		Build()

	resp, err := client.Im.V1.MessageResource.Get(ctx, req)
	if err != nil {
		return "", "", nil, fmt.Errorf("download: %w", err)
	}
	if !resp.Success() {
		return "", "", nil, fmt.Errorf("download error: code=%d msg=%s", resp.Code, resp.Msg)
	}

	downloadDir := uploadWorkDir(cfg, fileName)
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return "", "", nil, err
	}
	f, err := os.CreateTemp(downloadDir, "bot-*"+strings.ToLower(filepath.Ext(fileName)))
	if err != nil {
		return "", "", nil, err
	}
	outPath := f.Name()
	f.Close()

	if err := resp.WriteFile(outPath); err != nil {
		_ = os.Remove(outPath)
		return "", "", nil, fmt.Errorf("write file: %w", err)
	}
	if info, err := os.Stat(outPath); err != nil {
		_ = os.Remove(outPath)
		return "", "", nil, fmt.Errorf("stat file: %w", err)
	} else if info.Size() > cfg.Printing.MaxUploadBytes {
		_ = os.Remove(outPath)
		return "", "", nil, fmt.Errorf("file exceeds configured size limit of %d bytes", cfg.Printing.MaxUploadBytes)
	}

	return outPath, fileName, func() { _ = os.Remove(outPath) }, nil
}

func processCardAction(_ context.Context, cfg *config.Config, event *callback.CardActionTriggerEvent) *callback.CardActionTriggerResponse {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return nil
	}
	// Merge button callback value with form component values (v2 form container)
	values := make(map[string]interface{}, len(event.Event.Action.Value)+len(event.Event.Action.FormValue))
	for k, v := range event.Event.Action.Value {
		values[k] = v
	}
	for k, v := range event.Event.Action.FormValue {
		values[k] = v
	}
	openID := ""
	if event.Event.Operator != nil {
		openID = event.Event.Operator.OpenID
	}

	switch cardStr(values, "action") {
	case "select_printer":
		resp, ok := buildSelectedPrinterActionResponse(cfg, values, openID)
		if ok {
			updateBotSessionPrinter(cardStr(values, "session_id"), cardStr(values, "printer_id"))
		}
		return resp
	case "cancel":
		var resp *callback.CardActionTriggerResponse
		var ok bool
		if cardStr(values, "stage") == "printer_select" {
			resp, ok = buildPrinterSelectActionResponse(cfg, values, openID, printerSelectCardState{
				Disabled:          true,
				NextButtonText:    "已取消",
				CancelButtonText:  "已取消",
				SelectedPrinterID: cardStr(values, "printer_id"),
				StatusText:        "✅ 已取消本次打印任务",
			})
		} else {
			resp, ok = buildPrintCardActionResponse(cfg, values, openID, printConfigCardState{
				Disabled:         true,
				PrintButtonText:  "已取消",
				CancelButtonText: "已取消",
				StatusText:       "✅ 已取消本次打印任务",
			})
		}
		if ok {
			go handleBotCancel(cfg, values, openID)
		}
		return resp
	case "print":
		resp, ok := buildPrintCardActionResponse(cfg, values, openID, printConfigCardState{
			Disabled:        true,
			PrintButtonText: "处理中...",
			StatusText:      "⏳ 已收到打印请求，正在提交任务",
		})
		if ok {
			go handleBotPrint(cfg, values, openID, false)
		}
		return resp
	case "confirm_print":
		resp, ok := buildPrintConflictActionResponse(values, openID, printConflictCardState{
			Disabled:     true,
			ContinueText: "处理中...",
			CancelText:   "处理中...",
			StatusText:   "⏳ 已确认，正在提交打印任务",
		})
		if ok {
			go handleBotPrint(cfg, values, openID, true)
		}
		return resp
	case "continue_duplex":
		resp, ok := buildDuplexCardActionResponse(values, openID, duplexContinueCardState{
			Disabled:     true,
			ContinueText: "处理中...",
			StatusText:   "⏳ 已收到请求，正在提交第二面",
		})
		if ok {
			go handleBotDuplexContinue(cfg, values, openID)
		}
		return resp
	case "cancel_duplex":
		resp, ok := buildDuplexCardActionResponse(values, openID, duplexContinueCardState{
			Disabled:     true,
			ContinueText: "已取消",
			CancelText:   "已取消",
			StatusText:   "✅ 已取消剩余打印",
		})
		if ok {
			go handleBotDuplexCancel(cfg, values, openID)
		}
		return resp
	}
	return nil
}

func buildPrinterSelectActionResponse(cfg *config.Config, values map[string]interface{}, openID string, state printerSelectCardState) (*callback.CardActionTriggerResponse, bool) {
	sessionID := cardStr(values, "session_id")
	session, ok := getBotSession(sessionID)
	if !ok {
		return cardActionToast("warning", "卡片已失效，请重新发送文件"), false
	}
	if session.RequesterOpenID != "" && openID != "" && openID != session.RequesterOpenID {
		return cardActionToast("error", "这张打印卡片不属于你，请由发起人确认。"), false
	}
	shouldProcess := !session.ActionInProgress
	if session.ActionInProgress {
		state.Disabled = true
		state.NextButtonText = "处理中..."
		state.CancelButtonText = "处理中..."
		state.StatusText = "⏳ 请求正在处理，请勿重复点击"
	}

	pages := session.TotalPages
	if pages <= 0 {
		var err error
		pages, err = countPDFPages(session.SourcePath)
		if err != nil {
			log.Printf("[bot] build printer select card session=%s: %v", sessionID, err)
			return cardActionToast("info", "已收到请求"), shouldProcess
		}
	}
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: "已收到请求"},
		Card:  &callback.Card{Type: "raw", Data: buildPrinterSelectCardData(session.Filename, pages, buildPrinterOptions(cfg), sessionID, state)},
	}, shouldProcess
}

func buildSelectedPrinterActionResponse(cfg *config.Config, values map[string]interface{}, openID string) (*callback.CardActionTriggerResponse, bool) {
	sessionID := cardStr(values, "session_id")
	session, ok := getBotSession(sessionID)
	if !ok {
		return cardActionToast("warning", "卡片已失效，请重新发送文件"), false
	}
	if session.RequesterOpenID != "" && openID != "" && openID != session.RequesterOpenID {
		return cardActionToast("error", "这张打印卡片不属于你，请由发起人确认。"), false
	}
	if session.ActionInProgress {
		return buildPrinterSelectActionResponse(cfg, values, openID, printerSelectCardState{
			Disabled:          true,
			NextButtonText:    "处理中...",
			CancelButtonText:  "处理中...",
			SelectedPrinterID: cardStr(values, "printer_id"),
			StatusText:        "⏳ 请求正在处理，请勿重复点击",
		})
	}

	printer, err := resolveVisibleBotPrinter(cfg, cardStr(values, "printer_id"))
	if err != nil {
		log.Printf("[bot] select printer invalid session=%s: %v", sessionID, err)
		return cardActionToast("error", "请选择可用的打印机"), false
	}

	pages := session.TotalPages
	if pages <= 0 {
		pages, err = countPDFPages(session.SourcePath)
		if err != nil {
			log.Printf("[bot] count pages for selected printer session=%s: %v", sessionID, err)
			return cardActionToast("error", "无法读取文件页数"), false
		}
	}

	defaults := botSessionDefaults(cfg, session)
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: "已选择打印机"},
		Card:  &callback.Card{Type: "raw", Data: buildPrintConfigCardData(session.Filename, pages, printer, defaults, sessionID, printConfigCardState{})},
	}, true
}

func buildPrintCardActionResponse(cfg *config.Config, values map[string]interface{}, openID string, state printConfigCardState) (*callback.CardActionTriggerResponse, bool) {
	sessionID := cardStr(values, "session_id")
	session, ok := getBotSession(sessionID)
	if !ok {
		return cardActionToast("warning", "卡片已失效，请重新发送文件"), false
	}
	if session.RequesterOpenID != "" && openID != "" && openID != session.RequesterOpenID {
		return cardActionToast("error", "这张打印卡片不属于你，请由发起人确认。"), false
	}
	shouldProcess := !session.ActionInProgress
	if session.ActionInProgress {
		state.Disabled = true
		state.PrintButtonText = "处理中..."
		state.CancelButtonText = "处理中..."
		state.StatusText = "⏳ 打印请求正在处理，请勿重复点击"
	}

	card, err := buildDisabledPrintConfigCardData(cfg, session, sessionID, values, state)
	if err != nil {
		log.Printf("[bot] build disabled action card session=%s: %v", sessionID, err)
		return cardActionToast("info", "已收到请求"), shouldProcess
	}
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: "已收到请求"},
		Card:  &callback.Card{Type: "raw", Data: card},
	}, shouldProcess
}

func cardActionToast(toastType, content string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: toastType, Content: content},
	}
}

func botSessionDefaults(cfg *config.Config, session botCardSession) config.FileTypeDefault {
	if session.IsCloudDoc {
		return cfg.CloudDocDefault()
	}
	return cfg.ResolveFileTypeDefault(session.Filename)
}

func resolveVisibleBotPrinter(cfg *config.Config, printerID string) (config.PrinterConfig, error) {
	printerID = strings.TrimSpace(printerID)
	if printerID == "" {
		return config.PrinterConfig{}, fmt.Errorf("printer_id is required")
	}
	printer, ok := cfg.GetPrinterByID(printerID)
	if !ok {
		return config.PrinterConfig{}, fmt.Errorf("printer not configured: %s", printerID)
	}
	if !printer.Visible {
		return config.PrinterConfig{}, fmt.Errorf("printer not visible: %s", printerID)
	}
	return printer, nil
}

func buildDuplexCardActionResponse(values map[string]interface{}, openID string, state duplexContinueCardState) (*callback.CardActionTriggerResponse, bool) {
	token := cardStr(values, "token")
	pending, ok := getManualDuplexPending(token)
	if !ok {
		return cardActionToast("warning", "手动双面任务已失效"), false
	}
	if pending.OpenID != "" && openID != "" && openID != pending.OpenID {
		return cardActionToast("error", "这张打印卡片不属于你，请由发起人确认。"), false
	}
	shouldProcess := !pending.ActionInProgress
	if pending.ActionInProgress {
		state.Disabled = true
		state.ContinueText = "处理中..."
		state.CancelText = "处理中..."
		state.StatusText = "⏳ 请求正在处理，请勿重复点击"
	}
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: "已收到请求"},
		Card:  &callback.Card{Type: "raw", Data: buildDuplexContinueCardData(token, state)},
	}, shouldProcess
}

func buildPrintConflictActionResponse(values map[string]interface{}, openID string, state printConflictCardState) (*callback.CardActionTriggerResponse, bool) {
	sessionID := cardStr(values, "session_id")
	session, ok := getBotSession(sessionID)
	if !ok {
		return cardActionToast("warning", "卡片已失效，请重新发送文件"), false
	}
	if session.RequesterOpenID != "" && openID != "" && openID != session.RequesterOpenID {
		return cardActionToast("error", "这张打印卡片不属于你，请由发起人确认。"), false
	}

	shouldProcess := !session.ActionInProgress
	if session.ActionInProgress {
		state.Disabled = true
		state.ContinueText = "处理中..."
		state.CancelText = "处理中..."
		state.StatusText = "⏳ 打印请求正在处理，请勿重复点击"
	}
	state.PrinterID = strings.TrimSpace(cardStr(values, "printer_id"))
	if state.PrinterID == "" {
		state.PrinterID = session.PrinterID
	}
	if copies, err := strconv.Atoi(cardStr(values, "copies")); err == nil {
		state.Copies = copies
	}
	state.Pages = strings.TrimSpace(cardStr(values, "pages"))
	if nup, err := strconv.Atoi(cardStr(values, "nup")); err == nil {
		state.Nup = nup
	}
	state.Scale = strings.TrimSpace(cardStr(values, "scale"))
	state.Duplex = strings.TrimSpace(cardStr(values, "duplex"))
	if state.Duplex == "" {
		state.Duplex = "off"
	}

	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: "已收到请求"},
		Card:  &callback.Card{Type: "raw", Data: buildPrintConflictCardData(sessionID, state)},
	}, shouldProcess
}

func buildDisabledPrintConfigCardData(cfg *config.Config, session botCardSession, sessionID string, values map[string]interface{}, state printConfigCardState) (map[string]interface{}, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}
	totalPages := session.TotalPages
	if totalPages <= 0 {
		pages, err := countPDFPages(session.SourcePath)
		if err != nil {
			return nil, err
		}
		totalPages = pages
	}

	printerID := strings.TrimSpace(cardStr(values, "printer_id"))
	if printerID == "" {
		printerID = session.PrinterID
	}
	printer, err := resolveVisibleBotPrinter(cfg, printerID)
	if err != nil {
		return nil, err
	}

	defaults := botSessionDefaults(cfg, session)
	if copies, err := strconv.Atoi(cardStr(values, "copies")); err == nil && copies > 0 {
		defaults.Copies = copies
	}
	if nup, err := strconv.Atoi(cardStr(values, "nup")); err == nil && nup > 0 {
		defaults.Nup = nup
	}
	if scale, err := strconv.Atoi(cardStr(values, "scale")); err == nil && scale > 0 {
		defaults.Scale = scale
	}
	if duplex := strings.TrimSpace(cardStr(values, "duplex")); duplex != "" {
		defaults.Duplex = duplex
	}
	state.PagesValue = strings.TrimSpace(cardStr(values, "pages"))
	state.ScaleValue = strings.TrimSpace(cardStr(values, "scale"))

	return buildPrintConfigCardData(session.Filename, totalPages, printer, defaults, sessionID, state), nil
}

func handleBotCancel(cfg *config.Config, values map[string]interface{}, openID string) {
	sessionID := cardStr(values, "session_id")
	session, ok := claimBotSessionAction(sessionID)
	if !ok {
		log.Printf("[bot] cancel session already handled or not found: %s", sessionID)
		return
	}
	if session.RequesterOpenID != "" && openID != "" && openID != session.RequesterOpenID {
		releaseBotSessionAction(sessionID)
		log.Printf("[bot] reject cancel action from non-requester session=%s requester=%s operator=%s",
			sessionID, maskSensitive(session.RequesterOpenID), maskSensitive(openID))
		_ = sendBotText(context.Background(), cfg, session.ChatID, session.ChatType, openID, "这张打印卡片不属于你，请由发起人确认。", "")
		return
	}
	_ = os.Remove(session.SourcePath)
	deleteBotSession(sessionID)
	log.Printf("[bot] print config cancelled session=%s", sessionID)
}

func handleBotPrint(cfg *config.Config, values map[string]interface{}, openID string, confirmed bool) {
	sessionID := cardStr(values, "session_id")
	session, ok := claimBotSessionAction(sessionID)
	if !ok {
		log.Printf("[bot] card session expired, not found, or already processing: %s", sessionID)
		return
	}
	if session.RequesterOpenID != "" && openID != "" && openID != session.RequesterOpenID {
		releaseBotSessionAction(sessionID)
		log.Printf("[bot] reject card action from non-requester session=%s requester=%s operator=%s",
			sessionID, maskSensitive(session.RequesterOpenID), maskSensitive(openID))
		_ = sendBotText(context.Background(), cfg, session.ChatID, session.ChatType, openID, "这张打印卡片不属于你，请由发起人确认。", "")
		return
	}

	chatID := session.ChatID
	idType := receiveIDType(session.ChatType)
	replyMsgID := session.ReplyMessageID

	printerID := strings.TrimSpace(cardStr(values, "printer_id"))
	if printerID == "" {
		printerID = session.PrinterID
	}
	copies, _ := strconv.Atoi(cardStr(values, "copies"))
	if copies <= 0 {
		copies = 1
	}
	pagesStr := strings.TrimSpace(cardStr(values, "pages"))
	nup, _ := strconv.Atoi(cardStr(values, "nup"))
	if nup <= 0 {
		nup = 1
	}
	scalePercent, scaleErr := parseScalePercent(cardStr(values, "scale"))
	duplex := strings.TrimSpace(cardStr(values, "duplex"))
	if duplex == "" {
		duplex = "off"
	}

	// Validate inputs; re-send config card with error hint on invalid input
	if printerID == "" {
		releaseBotSessionAction(sessionID)
		_ = sendSessionText(context.Background(), cfg, session, "请选择打印机")
		return
	}
	if copies < 1 || copies > cfg.Printing.MaxCopies {
		releaseBotSessionAction(sessionID)
		_ = sendSessionText(context.Background(), cfg, session, fmt.Sprintf("份数必须为 1-%d", cfg.Printing.MaxCopies))
		resendPrintConfigCard(cfg, session, sessionID, chatID, idType)
		return
	}
	if !isValidPages(pagesStr) {
		releaseBotSessionAction(sessionID)
		_ = sendSessionText(context.Background(), cfg, session, "页码范围格式无效（如 1-5,7,9-12）")
		resendPrintConfigCard(cfg, session, sessionID, chatID, idType)
		return
	}
	if nup != 1 && !pdfutil.ValidNup(nup) {
		releaseBotSessionAction(sessionID)
		_ = sendSessionText(context.Background(), cfg, session, "无效的缩印选项（支持 1/2/4/6）")
		resendPrintConfigCard(cfg, session, sessionID, chatID, idType)
		return
	}
	if scaleErr != nil {
		releaseBotSessionAction(sessionID)
		_ = sendSessionText(context.Background(), cfg, session, "缩放比例必须为 10-400 的整数百分比")
		resendPrintConfigCard(cfg, session, sessionID, chatID, idType)
		return
	}
	if duplex != "off" && duplex != "auto" && duplex != "manual" {
		releaseBotSessionAction(sessionID)
		_ = sendSessionText(context.Background(), cfg, session, "无效的双面选项")
		resendPrintConfigCard(cfg, session, sessionID, chatID, idType)
		return
	}

	printerCfg, err := resolveVisibleBotPrinter(cfg, printerID)
	if err != nil {
		log.Printf("[bot] resolve printer %s: %v", printerID, err)
		releaseBotSessionAction(sessionID)
		_ = sendSessionText(context.Background(), cfg, session, "请选择可用的打印机")
		resendPrintConfigCard(cfg, session, sessionID, chatID, idType)
		return
	}
	session.PrinterID = printerID
	updateBotSessionPrinter(sessionID, printerID)

	if !isPrinterDuplexOptionSupported(printerCfg, duplex) {
		releaseBotSessionAction(sessionID)
		_ = sendSessionText(context.Background(), cfg, session, "该打印机不支持所选双面模式")
		resendPrintConfigCard(cfg, session, sessionID, chatID, idType)
		return
	}

	if !confirmed && !strings.EqualFold(cardStr(values, "confirmed"), "true") {
		if warning := activeJobWarningForPrinter(context.Background(), cfg, printerID); warning != nil {
			card, err := buildPrintConflictCard(sessionID, printConflictCardState{
				Warning:        warning,
				PrinterID:      printerID,
				Copies:         copies,
				Pages:          pagesStr,
				Nup:            nup,
				Scale:          strconv.Itoa(scalePercent),
				Duplex:         duplex,
				OriginalAction: "print",
				CancelStage:    "print_details",
			})
			if err != nil {
				log.Printf("[bot] build print conflict card session=%s: %v", sessionID, err)
				_ = sendSessionText(context.Background(), cfg, session, warning.Message)
			} else if delivery, sendErr := sendBotCard(context.Background(), cfg, chatID, session.ChatType, session.RequesterOpenID, card, replyMsgID); sendErr != nil {
				log.Printf("[bot] send print conflict card session=%s: %v", sessionID, sendErr)
				_ = sendSessionText(context.Background(), cfg, session, warning.Message)
			} else {
				updateBotSessionDelivery(sessionID, delivery)
			}
			releaseBotSessionAction(sessionID)
			return
		}
	}

	actionFinalized := false
	defer func() {
		if !actionFinalized {
			releaseBotSessionAction(sessionID)
		}
	}()

	printSourcePath := session.SourcePath

	if session.CardID != "" {
		if err := disableCardButtons(context.Background(), cfg, session.CardID); err != nil {
			log.Printf("[bot] disable card buttons card=%s: %v", session.CardID, err)
		}
	} else {
		log.Printf("[bot] cardID is empty, skip disableButtons session=%s", sessionID)
	}

	// Page selection
	pageCleanup := func() {}
	if pagesStr != "" {
		selectedPath, cleanupPages, err := extractPDFPages(printSourcePath, pagesStr)
		if err != nil {
			log.Printf("[bot] extract pages pagesStr=%q source=%s: %v", pagesStr, printSourcePath, err)
			_ = sendSessionText(context.Background(), cfg, session, fmt.Sprintf("页码范围提取失败：%v", err))
			return
		}
		printSourcePath = selectedPath
		pageCleanup = cleanupPages
	}
	defer pageCleanup()

	// N-up
	nupCleanup := func() {}
	if nup > 1 {
		nupPath, cleanupNup, err := applyNupLayout(printSourcePath, nup, "horizontal")
		if err != nil {
			log.Printf("[bot] nup: %v", err)
			_ = sendSessionText(context.Background(), cfg, session, "缩印排版失败")
			return
		}
		if nupPath != printSourcePath {
			printSourcePath = nupPath
			nupCleanup = cleanupNup
		}
	}
	defer nupCleanup()

	scaleCleanup := func() {}
	if scalePercent != 100 {
		scaledPath, cleanupScale, err := applyScalePercent(printSourcePath, scalePercent)
		if err != nil {
			log.Printf("[bot] scale: %v", err)
			_ = sendSessionText(context.Background(), cfg, session, "页面缩放处理失败")
			return
		}
		if scaledPath != printSourcePath {
			printSourcePath = scaledPath
			scaleCleanup = cleanupScale
		}
	}
	defer scaleCleanup()

	cupsClient, printerName, err := newCupsClientForPrinter(printerCfg)
	if err != nil {
		log.Printf("[bot] cups client: %v", err)
		_ = sendSessionText(context.Background(), cfg, session, "打印机连接失败")
		return
	}

	printPageCount := 0
	if pageCount, countErr := countPDFPages(printSourcePath); countErr == nil {
		printPageCount = pageCount
	} else {
		log.Printf("[bot] failed to count pages path=%s err=%v", printSourcePath, countErr)
	}
	duplexMode := botSelectedDuplexMode(duplex, printPageCount)

	finalPath, err := applyCopiesMode(printSourcePath, copies, true)
	if err != nil {
		log.Printf("[bot] copies: %v", err)
		_ = sendSessionText(context.Background(), cfg, session, "份数排版失败")
		return
	}
	if finalPath != printSourcePath {
		defer os.Remove(finalPath)
	}

	if duplexMode == "manual" {
		duplexEdge := extractDuplexEdge(duplex)
		firstPassPath, secondPassPath, cleanupDup, err := prepareManualDuplexFiles(finalPath, printerCfg, duplexEdge)
		if err != nil {
			log.Printf("[bot] manual duplex prepare: %v", err)
			_ = sendSessionText(context.Background(), cfg, session, "手动双面准备失败")
			return
		}
		defer cleanupDup()

		jobID, err := cupsClient.SubmitJob(printerName, firstPassPath, cups.PrintOptions{Copies: 1})
		if err != nil {
			log.Printf("[bot] submit first pass: %v", err)
			_ = sendSessionText(context.Background(), cfg, session, "手动双面提交失败")
			return
		}

		token, expiresAt, err := saveManualDuplexPending(jobID, printerID, secondPassPath, 1, openID, printPageCount*copies)
		if err != nil {
			log.Printf("[bot] save duplex pending: %v", err)
			_ = sendSessionText(context.Background(), cfg, session, "保存双面任务失败")
			return
		}

		persistBotJobRecord(cfg, printJobRecord{
			JobID:          jobID,
			PrinterID:      printerID,
			FileName:       session.Filename,
			Status:         "pending_manual_continue",
			Copies:         copies,
			PageCount:      printPageCount,
			Duplex:         true,
			DuplexHook:     "bot://manual-duplex/" + token,
			DuplexExpireAt: expiresAt,
			User:           feishuUserInfo{OpenID: openID},
		})

		duplexCard, err := buildDuplexContinueCard(token)
		if err != nil {
			log.Printf("[bot] build duplex card: %v", err)
		} else {
			_, _ = sendBotCard(context.Background(), cfg, chatID, session.ChatType, session.RequesterOpenID, duplexCard, replyMsgID)
		}

		_ = os.Remove(session.SourcePath)
		deleteBotSession(sessionID)
		actionFinalized = true
		return
	}

	printPath := finalPath
	if duplexMode == "off" && printerCfg.Reverse {
		reversedPath, reverseErr := prepareReversedPDF(finalPath)
		if reverseErr != nil {
			log.Printf("[bot] reverse pdf: %v", reverseErr)
			_ = sendSessionText(context.Background(), cfg, session, "PDF逆序处理失败")
			return
		}
		if reversedPath != finalPath {
			defer os.Remove(reversedPath)
			printPath = reversedPath
		}
	}

	printOpts := cups.PrintOptions{Copies: 1, Collate: true}
	if duplexMode == "auto" {
		sides, sideErr := chooseAutoDuplexSides(printPath)
		if sideErr != nil {
			printOpts.Sides = "two-sided-long-edge"
		} else {
			printOpts.Sides = sides
		}
	}

	jobID, err := cupsClient.SubmitJob(printerName, printPath, printOpts)
	if err != nil {
		log.Printf("[bot] submit job: %v", err)
		_ = sendSessionText(context.Background(), cfg, session, fmt.Sprintf("打印提交失败：%v", err))
		return
	}

	persistBotJob(cfg, jobID, printerID, session.Filename, copies, printPageCount, duplexMode != "off", openID)
	_ = os.Remove(session.SourcePath)
	deleteBotSession(sessionID)
	actionFinalized = true
	log.Printf("[bot] print job submitted: job_id=%s printer=%s duplex=%s", jobID, printerID, duplexMode)

	duplexLabel := "单面"
	if duplexMode != "off" {
		duplexLabel = "双面（" + duplexMode + "）"
	}
	card, err := buildJobSubmittedCard(jobID, printerID, session.Filename, copies, duplexLabel)
	if err != nil {
		log.Printf("[bot] build submitted card: %v", err)
	} else {
		_, _ = sendBotCard(context.Background(), cfg, chatID, session.ChatType, session.RequesterOpenID, card, replyMsgID)
	}
}

func botSelectedDuplexMode(selectedDuplex string, pageCount int) string {
	selectedDuplex = strings.TrimSpace(selectedDuplex)
	if selectedDuplex == "" {
		selectedDuplex = "off"
	}
	if pageCount == 1 {
		return "off"
	}
	return selectedDuplex
}

func persistBotJob(cfg *config.Config, jobID, printerID, filename string, copies int, pageCount int, duplex bool, openID string) {
	persistBotJobRecord(cfg, printJobRecord{
		JobID:     jobID,
		PrinterID: printerID,
		FileName:  filename,
		Status:    "pending",
		Copies:    copies,
		PageCount: pageCount,
		Duplex:    duplex,
		User:      feishuUserInfo{OpenID: openID},
	})
}

func persistBotJobRecord(cfg *config.Config, record printJobRecord) {
	store, err := newBitableJobStore(cfg)
	if err != nil {
		log.Printf("[bot] bitable store init failed: %v", err)
		return
	}

	if err := store.SaveJob(context.Background(), record); err != nil {
		log.Printf("[bot] bitable persist failed: job_id=%s err=%v", record.JobID, err)
		return
	}

	tracker := initJobStatusPoller(cfg)
	if tracker != nil {
		tracker.AddPendingJobWithStatus(record.JobID, record.PrinterID, record.Status)
	}
}

func handleBotDuplexContinue(cfg *config.Config, values map[string]interface{}, openID string) {
	token := cardStr(values, "token")
	pending, ok := claimManualDuplexPending(token)
	if !ok {
		log.Printf("[bot] duplex hook not found or already processing: %s", token)
		return
	}
	if pending.OpenID != "" && openID != "" && openID != pending.OpenID {
		releaseManualDuplexPending(token)
		log.Printf("[bot] reject duplex continue from non-requester token=%s requester=%s operator=%s",
			token, maskSensitive(pending.OpenID), maskSensitive(openID))
		return
	}

	printerCfg, err := resolvePrinter(pending.PrinterID)
	if err != nil {
		releaseManualDuplexPending(token)
		return
	}

	cupsClient, printerName, err := newCupsClientForPrinter(printerCfg)
	if err != nil {
		releaseManualDuplexPending(token)
		return
	}

	jobID, err := cupsClient.SubmitJob(printerName, pending.RemainingFilePath, cups.PrintOptions{Copies: 1})
	if err != nil {
		log.Printf("[bot] duplex continue submit failed: %v", err)
		releaseManualDuplexPending(token)
		return
	}

	_ = os.Remove(pending.RemainingFilePath)
	deleteManualDuplexPending(token)

	if err := updateBotManualDuplexContinued(cfg, pending.JobID, jobID); err != nil {
		log.Printf("[bot] bitable manual duplex continue update failed initial_job_id=%s continued_job_id=%s err=%v", pending.JobID, jobID, err)
	}
	if tracker := initJobStatusPoller(cfg); tracker != nil {
		tracker.AddPendingJob(jobID, pending.PrinterID)
	}
	log.Printf("[bot] manual duplex continue: job_id=%s", jobID)
}

func handleBotDuplexCancel(cfg *config.Config, values map[string]interface{}, openID string) {
	token := cardStr(values, "token")
	pending, ok := claimManualDuplexPending(token)
	if !ok {
		return
	}
	if pending.OpenID != "" && openID != "" && openID != pending.OpenID {
		releaseManualDuplexPending(token)
		log.Printf("[bot] reject duplex cancel from non-requester token=%s requester=%s operator=%s",
			token, maskSensitive(pending.OpenID), maskSensitive(openID))
		return
	}
	if pending.JobID != "" {
		if err := updateBotJobStatus(cfg, pending.JobID, "cancelled"); err != nil {
			log.Printf("[bot] bitable manual duplex cancel update failed job_id=%s err=%v", pending.JobID, err)
		}
		if tracker := initJobStatusPoller(cfg); tracker != nil {
			tracker.RemovePendingJob(pending.JobID)
		}
	}
	_ = os.Remove(pending.RemainingFilePath)
	deleteManualDuplexPending(token)
	log.Printf("[bot] manual duplex cancelled: token=%s", token)
}

func updateBotManualDuplexContinued(cfg *config.Config, initialJobID, continuedJobID string) error {
	store, err := newBitableJobStore(cfg)
	if err != nil {
		return err
	}
	return store.UpdateManualDuplexContinued(context.Background(), initialJobID, continuedJobID)
}

func updateBotJobStatus(cfg *config.Config, jobID, status string) error {
	store, err := newBitableJobStore(cfg)
	if err != nil {
		return err
	}
	return store.UpdateJobStatus(context.Background(), jobID, status)
}
