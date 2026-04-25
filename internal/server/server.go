package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gpt2api-sidecar/internal/config"
	"gpt2api-sidecar/internal/pool"
	"gpt2api-sidecar/internal/queue"
	"gpt2api-sidecar/internal/runner"
	"gpt2api-sidecar/pkg/logger"
)

const (
	maxReferenceImages = 4
	imageJobTimeout    = 3 * time.Minute
	errQueueFull       = "queue_full"
	errQueueTimeout    = "queue_timeout"
	errImageTimeout    = "image_timeout"
)

type Server struct {
	cfg              *config.Config
	pool             *pool.Pool
	runner           *runner.Runner
	apiKeys          map[string]struct{}
	models           map[string]config.ModelConfig
	blobStore        *blobStore
	imageQueue       *queue.Gate
	queueWaitTimeout time.Duration
}

func New(cfg *config.Config, p *pool.Pool, r *runner.Runner, blobTTL time.Duration) *Server {
	models := make(map[string]config.ModelConfig, len(cfg.Models))
	for _, model := range cfg.Models {
		models[model.ID] = model
	}
	apiKeys := make(map[string]struct{}, len(cfg.Auth.APIKeys))
	for _, key := range cfg.Auth.APIKeys {
		apiKeys[key] = struct{}{}
	}
	queueWaitTimeout, _ := cfg.QueueWaitTimeoutDuration()
	queueLimit := len(p.States())
	if queueLimit <= 0 {
		queueLimit = 1
	}
	return &Server{
		cfg:              cfg,
		pool:             p,
		runner:           r,
		apiKeys:          apiKeys,
		models:           models,
		blobStore:        newBlobStore(blobTTL),
		imageQueue:       queue.New(queueLimit, cfg.Server.MaxQueueSize),
		queueWaitTimeout: queueWaitTimeout,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/models", s.withAuth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.withAuth(s.handleChatCompletions))
	mux.HandleFunc("/v1/images/generations", s.withAuth(s.handleImageGenerations))
	mux.HandleFunc("/v1/images/edits", s.withAuth(s.handleImageEdits))
	mux.HandleFunc("/v1/blobs/", s.handleBlob)
	return loggingMiddleware(mux)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":   "ok",
		"models":   len(s.models),
		"accounts": s.pool.States(),
		"queue":    s.imageQueue.Stats(),
	})
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	type modelItem struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
		Type    string `json:"type,omitempty"`
	}

	firstImageModelID := func() string {
		for _, model := range s.cfg.Models {
			if model.Type == config.ModelTypeImage {
				return model.ID
			}
		}
		return ""
	}

	data := make([]modelItem, 0, len(s.models))
	for _, model := range s.cfg.Models {
		data = append(data, modelItem{
			ID:      model.ID,
			Object:  "model",
			Created: 0,
			OwnedBy: "gpt2api-sidecar",
			Type:    model.Type,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object":                 "list",
		"data":                   data,
		"default_image_model_id": firstImageModelID(),
	})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, _ *http.Request) {
	writeOpenAIError(w, http.StatusNotImplemented, "not_implemented",
		"这个轻量 sidecar 当前只开放图片路由：/v1/images/generations 和 /v1/images/edits。")
}

type imageGenerateRequest struct {
	Model           string   `json:"model"`
	Prompt          string   `json:"prompt"`
	N               int      `json:"n"`
	Size            string   `json:"size,omitempty"`
	Quality         string   `json:"quality,omitempty"`
	ResponseFormat  string   `json:"response_format,omitempty"`
	ReferenceImages []string `json:"reference_images,omitempty"`
}

type imageResponseItem struct {
	B64JSON string `json:"b64_json,omitempty"`
	URL     string `json:"url,omitempty"`
}

type imageResponse struct {
	Created int64               `json:"created"`
	Data    []imageResponseItem `json:"data"`
}

func (s *Server) handleImageGenerations(w http.ResponseWriter, req *http.Request) {
	var body imageGenerateRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "请求体不是合法 JSON。")
		return
	}
	if body.Model == "" {
		body.Model = s.defaultImageModelID()
		if body.Model == "" {
			writeOpenAIError(w, http.StatusBadRequest, "model_not_found", "配置中没有可用的 image 模型。")
			return
		}
	}

	if strings.TrimSpace(body.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt 不能为空。")
		return
	}

	model, ok := s.models[body.Model]
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "model_not_found", fmt.Sprintf("未知模型 %q。", body.Model))
		return
	}
	if model.Type != config.ModelTypeImage {
		writeOpenAIError(w, http.StatusBadRequest, "model_type_mismatch",
			fmt.Sprintf("模型 %q 是 %s 类型，不能用于 /v1/images/generations。", body.Model, model.Type))
		return
	}

	refs, err := decodeReferenceInputs(req.Context(), body.ReferenceImages, s.cfg.Server.MaxImageBytes)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_reference_image", err.Error())
		return
	}

	items, err := s.generateImages(req.Context(), model, body.Prompt, refs, normalizeN(body.N), body.ResponseFormat)
	if err != nil {
		s.writeRunnerError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, imageResponse{
		Created: time.Now().Unix(),
		Data:    items,
	})
}

func (s *Server) handleImageEdits(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseMultipartForm(int64(s.cfg.Server.MaxImageBytes) * int64(maxReferenceImages+1)); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "解析 multipart 失败。")
		return
	}

	modelID := req.FormValue("model")
	if modelID == "" {
		modelID = s.defaultImageModelID()
		if modelID == "" {
			writeOpenAIError(w, http.StatusBadRequest, "model_not_found", "配置中没有可用的 image 模型。")
			return
		}
	}
	model, ok := s.models[modelID]
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "model_not_found", fmt.Sprintf("未知模型 %q。", modelID))
		return
	}
	if model.Type != config.ModelTypeImage {
		writeOpenAIError(w, http.StatusBadRequest, "model_type_mismatch",
			fmt.Sprintf("模型 %q 是 %s 类型，不能用于 /v1/images/edits。", modelID, model.Type))
		return
	}

	prompt := strings.TrimSpace(req.FormValue("prompt"))
	if prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "prompt 不能为空。")
		return
	}

	files, err := collectEditFiles(req.MultipartForm)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(files) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "至少需要上传一张 image。")
		return
	}

	refs := make([]runner.ReferenceImage, 0, len(files))
	for _, fileHeader := range files {
		data, err := readMultipartFile(fileHeader)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_reference_image", err.Error())
			return
		}
		if len(data) > int(s.cfg.Server.MaxImageBytes) {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_reference_image",
				fmt.Sprintf("参考图 %q 超过大小限制。", fileHeader.Filename))
			return
		}
		refs = append(refs, runner.ReferenceImage{
			Data:     data,
			FileName: filepath.Base(fileHeader.Filename),
		})
	}

	items, err := s.generateImages(req.Context(), model, prompt, refs, normalizeN(parseFormInt(req.FormValue("n"), 1)), req.FormValue("response_format"))
	if err != nil {
		s.writeRunnerError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, imageResponse{
		Created: time.Now().Unix(),
		Data:    items,
	})
}

func (s *Server) generateImages(
	ctx context.Context,
	model config.ModelConfig,
	prompt string,
	references []runner.ReferenceImage,
	n int,
	responseFormat string,
) ([]imageResponseItem, error) {
	queueLease, err := s.acquireImageQueue(ctx)
	if err != nil {
		return nil, err
	}
	defer queueLease.Release()

	items := make([]imageResponseItem, 0, n)
	jobCount := n
	if jobCount <= 0 {
		jobCount = 1
	}
	jobsCtx, cancelJobs := context.WithCancel(ctx)
	defer cancelJobs()
	results := make(chan imageJobResult, jobCount)
	requestTimeout, _ := s.cfg.RequestTimeoutDuration()
	acquireTimeout, _ := s.cfg.AcquireTimeoutDuration()
	taskTimeout := requestTimeout
	if taskTimeout <= 0 || taskTimeout > imageJobTimeout {
		taskTimeout = imageJobTimeout
	}
	maxAttempts := 2
	if len(references) > 0 {
		maxAttempts = 1
	}

	for i := 0; i < jobCount; i++ {
		go func(idx int) {
			result := s.runner.Run(jobsCtx, runner.Request{
				Prompt:         prompt,
				UpstreamModel:  model.Upstream,
				References:     references,
				MaxAttempts:    maxAttempts,
				AcquireTimeout: acquireTimeout,
				TaskTimeout:    taskTimeout,
				MaxImageBytes:  s.cfg.Server.MaxImageBytes,
			})
			if result.ErrorCode != "" {
				code := result.ErrorCode
				message := result.ErrorMessage
				if code == runner.ErrPollTimeout && strings.Contains(strings.ToLower(message), "deadline exceeded") {
					code = errImageTimeout
					message = "no image returned within 3 minutes after an account was acquired"
				}
				results <- imageJobResult{
					Index:        idx,
					ErrorCode:    code,
					ErrorMessage: message,
				}
				return
			}
			results <- imageJobResult{
				Index:  idx,
				Images: result.Images,
			}
		}(i)
	}

	var lastErr *runnerError
	completed := 0
	for completed < jobCount && len(items) < n {
		select {
		case result := <-results:
			completed++
			if result.ErrorCode != "" {
				lastErr = &runnerError{Code: result.ErrorCode, Message: result.ErrorMessage}
				logger.L().Warn("parallel image job failed",
					zap.Int("job", result.Index+1),
					zap.Int("jobs", jobCount),
					zap.String("code", result.ErrorCode),
					zap.String("message", result.ErrorMessage),
				)
				continue
			}
			for _, image := range result.Images {
				if len(items) >= n {
					break
				}
				items = append(items, s.makeImageResponseItem(image, responseFormat))
			}
		case <-ctx.Done():
			cancelJobs()
			return nil, &runnerError{Code: errImageTimeout, Message: ctx.Err().Error()}
		}
	}
	cancelJobs()

	if len(items) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, &runnerError{
			Code:    runner.ErrDownload,
			Message: "生成流程结束，但没有拿到任何图片字节。",
		}
	}

	return items, nil
}

type imageJobResult struct {
	Index        int
	Images       []runner.Image
	ErrorCode    string
	ErrorMessage string
}

func (s *Server) defaultImageModelID() string {
	for _, model := range s.cfg.Models {
		if model.Type == config.ModelTypeImage {
			return model.ID
		}
	}
	return ""
}

func (s *Server) acquireImageQueue(ctx context.Context) (*queue.Lease, error) {
	waitCtx := ctx
	cancel := func() {}
	if s.queueWaitTimeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, s.queueWaitTimeout)
	}
	defer cancel()

	start := time.Now()
	lease, err := s.imageQueue.Acquire(waitCtx)
	if err != nil {
		switch {
		case errors.Is(err, queue.ErrQueueFull):
			return nil, &runnerError{
				Code:    errQueueFull,
				Message: "image queue is full",
			}
		case errors.Is(err, context.DeadlineExceeded):
			return nil, &runnerError{
				Code:    errQueueTimeout,
				Message: "timed out while waiting in the image queue",
			}
		default:
			return nil, err
		}
	}

	wait := time.Since(start)
	if wait >= 500*time.Millisecond {
		stats := s.imageQueue.Stats()
		logger.L().Info("image queue acquired",
			zap.Duration("wait", wait),
			zap.Int("active", stats.Active),
			zap.Int("pending", stats.Pending),
		)
	}

	return lease, nil
}

func (s *Server) makeImageResponseItem(image runner.Image, responseFormat string) imageResponseItem {
	if strings.EqualFold(responseFormat, "url") {
		blobID := s.blobStore.Put(image.Bytes, image.ContentType)
		return imageResponseItem{
			URL: strings.TrimRight(s.cfg.Server.PublicBaseURL, "/") + "/v1/blobs/" + blobID,
		}
	}

	return imageResponseItem{
		B64JSON: base64.StdEncoding.EncodeToString(image.Bytes),
	}
}

func (s *Server) handleBlob(w http.ResponseWriter, req *http.Request) {
	blobID := strings.TrimPrefix(req.URL.Path, "/v1/blobs/")
	if blobID == "" {
		http.NotFound(w, req)
		return
	}

	item, ok := s.blobStore.Get(blobID)
	if !ok {
		http.NotFound(w, req)
		return
	}

	w.Header().Set("Content-Type", item.ContentType)
	w.Header().Set("Cache-Control", "private, max-age=60")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(item.Data)
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		authHeader := req.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			writeOpenAIError(w, http.StatusUnauthorized, "missing_api_key", "缺少 Bearer API Key。")
			return
		}

		apiKey := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		if _, ok := s.apiKeys[apiKey]; !ok {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "API Key 无效。")
			return
		}

		next(w, req)
	}
}

func normalizeN(v int) int {
	if v <= 0 {
		return 1
	}
	if v > 4 {
		return 4
	}
	return v
}

func parseFormInt(raw string, fallback int) int {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func decodeReferenceInputs(ctx context.Context, inputs []string, maxImageBytes int64) ([]runner.ReferenceImage, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if len(inputs) > maxReferenceImages {
		return nil, fmt.Errorf("最多支持 %d 张参考图", maxReferenceImages)
	}

	out := make([]runner.ReferenceImage, 0, len(inputs))
	for i, input := range inputs {
		data, fileName, err := fetchReferenceBytes(ctx, input, maxImageBytes)
		if err != nil {
			return nil, fmt.Errorf("第 %d 张参考图解析失败: %w", i+1, err)
		}
		out = append(out, runner.ReferenceImage{
			Data:     data,
			FileName: fileName,
		})
	}
	return out, nil
}

func fetchReferenceBytes(ctx context.Context, raw string, maxImageBytes int64) ([]byte, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, "", errors.New("参考图为空")
	}

	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "data:"):
		commaIndex := strings.IndexByte(raw, ',')
		if commaIndex < 0 {
			return nil, "", errors.New("无效 data URL")
		}
		meta := raw[:commaIndex]
		payload := raw[commaIndex+1:]
		if strings.Contains(strings.ToLower(meta), ";base64") {
			data, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				return nil, "", fmt.Errorf("base64 解码失败: %w", err)
			}
			return data, "reference.png", nil
		}
		return []byte(payload), "reference.txt", nil

	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return nil, "", err
		}
		client := &http.Client{Timeout: 20 * time.Second}
		response, err := client.Do(request)
		if err != nil {
			return nil, "", err
		}
		defer response.Body.Close()
		if response.StatusCode >= 400 {
			return nil, "", fmt.Errorf("HTTP %d", response.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, maxImageBytes+1))
		if err != nil {
			return nil, "", err
		}
		if int64(len(data)) > maxImageBytes {
			return nil, "", errors.New("参考图超过大小限制")
		}
		fileName := filepath.Base(request.URL.Path)
		if fileName == "." || fileName == "/" || fileName == "" {
			fileName = "reference.png"
		}
		return data, fileName, nil

	default:
		data, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, "", fmt.Errorf("既不是 URL，也不是可解析的 base64: %w", err)
		}
		return data, "reference.png", nil
	}
}

func collectEditFiles(form *multipart.Form) ([]*multipart.FileHeader, error) {
	if form == nil {
		return nil, errors.New("空 multipart 表单")
	}

	var out []*multipart.FileHeader
	seen := map[string]struct{}{}
	add := func(files []*multipart.FileHeader) {
		for _, file := range files {
			if file == nil {
				continue
			}
			key := fmt.Sprintf("%s|%d", file.Filename, file.Size)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, file)
		}
	}

	for _, key := range []string{"image", "image[]", "images", "images[]", "mask"} {
		add(form.File[key])
	}
	for key, files := range form.File {
		if strings.HasPrefix(key, "image_") {
			add(files)
		}
	}
	return out, nil
}

func readMultipartFile(fileHeader *multipart.FileHeader) ([]byte, error) {
	file, err := fileHeader.Open()
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return io.ReadAll(file)
}

type runnerError struct {
	Code    string
	Message string
}

func (e *runnerError) Error() string {
	return e.Code + ": " + e.Message
}

func (s *Server) writeRunnerError(w http.ResponseWriter, err error) {
	var imageErr *runnerError
	if !errors.As(err, &imageErr) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	status := http.StatusBadGateway
	switch imageErr.Code {
	case runner.ErrNoAccount, runner.ErrRateLimited, runner.ErrNetworkTransient, errQueueFull, errQueueTimeout:
		status = http.StatusServiceUnavailable
	case runner.ErrAuthRequired:
		status = http.StatusUnauthorized
	case errImageTimeout:
		status = http.StatusGatewayTimeout
	case runner.ErrPreviewOnly, runner.ErrPollTimeout, runner.ErrDownload:
		status = http.StatusBadGateway
	}
	writeOpenAIError(w, status, imageErr.Code, localizeImageError(imageErr.Code, imageErr.Message))
}

func localizeRunnerError(code, message string) string {
	switch code {
	case runner.ErrNoAccount:
		return "当前没有可用 ChatGPT 账号。"
	case runner.ErrRateLimited:
		return "上游触发限流，账号已进入冷却。"
	case runner.ErrNetworkTransient:
		return "连接上游时出现瞬态网络错误，请稍后重试。"
	case runner.ErrAuthRequired:
		return "账号鉴权失败，已自动停用该账号。"
	case runner.ErrPreviewOnly:
		return "当前只拿到了 preview-only 结果。"
	case runner.ErrPollTimeout:
		return "等待图片最终结果超时。"
	case runner.ErrDownload:
		return "图片生成成功，但下载图片字节失败。"
	default:
		if strings.TrimSpace(message) != "" {
			return message
		}
		return "图片生成失败。"
	}
}

func localizeImageError(code, message string) string {
	switch code {
	case runner.ErrNoAccount:
		return "No ChatGPT account is available right now."
	case runner.ErrRateLimited:
		return "Upstream rate limiting was triggered and the account was cooled down."
	case runner.ErrNetworkTransient:
		return "A transient upstream network error occurred. Please retry shortly."
	case runner.ErrAuthRequired:
		return "Account authentication failed and the account was disabled."
	case runner.ErrPreviewOnly:
		return "Only a preview result was returned."
	case runner.ErrPollTimeout:
		return "Timed out while waiting for the final image result."
	case runner.ErrDownload:
		return "The image was generated, but downloading the bytes failed."
	case errImageTimeout:
		return "No image was returned within 3 minutes."
	case errQueueFull:
		return "The image request queue is full. Please retry shortly."
	case errQueueTimeout:
		return "Timed out while waiting in the image request queue."
	default:
		if strings.TrimSpace(message) != "" {
			return message
		}
		return "Image generation failed."
	}
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, req)
		logger.L().Info("http request",
			zap.String("method", req.Method),
			zap.String("path", req.URL.Path),
			zap.Duration("duration", time.Since(start)),
		)
	})
}

type blobItem struct {
	Data        []byte
	ContentType string
	ExpiresAt   time.Time
}

type blobStore struct {
	mu    sync.Mutex
	ttl   time.Duration
	items map[string]blobItem
}

func newBlobStore(ttl time.Duration) *blobStore {
	return &blobStore{
		ttl:   ttl,
		items: map[string]blobItem{},
	}
}

func (b *blobStore) Put(data []byte, contentType string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gcLocked()

	rawID := make([]byte, 16)
	if _, err := rand.Read(rawID); err != nil {
		panic(err)
	}
	id := hex.EncodeToString(rawID)
	b.items[id] = blobItem{
		Data:        append([]byte(nil), data...),
		ContentType: contentType,
		ExpiresAt:   time.Now().Add(b.ttl),
	}
	return id
}

func (b *blobStore) Get(id string) (blobItem, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gcLocked()
	item, ok := b.items[id]
	return item, ok
}

func (b *blobStore) gcLocked() {
	now := time.Now()
	for id, item := range b.items {
		if now.After(item.ExpiresAt) {
			delete(b.items, id)
		}
	}
}
