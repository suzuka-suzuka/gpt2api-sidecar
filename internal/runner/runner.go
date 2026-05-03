package runner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"gpt2api-sidecar/internal/pool"
	"gpt2api-sidecar/internal/upstream/chatgpt"
	"gpt2api-sidecar/pkg/logger"
)

const (
	ErrUnknown          = "unknown"
	ErrNoAccount        = "no_available_account"
	ErrAuthRequired     = "auth_required"
	ErrRateLimited      = "rate_limited"
	ErrNetworkTransient = "network_transient"
	ErrPOWTimeout       = "pow_timeout"
	ErrPOWFailed        = "pow_failed"
	ErrNoImageTask      = "no_image_task"
	ErrPreviewOnly      = "preview_only"
	ErrPollTimeout      = "poll_timeout"
	ErrDownload         = "download_failed"
	ErrUpstream         = "upstream_error"
)

type ReferenceImage struct {
	Data     []byte
	FileName string
}

type Request struct {
	Prompt         string
	UpstreamModel  string
	References     []ReferenceImage
	AcquireTimeout time.Duration
	TaskTimeout    time.Duration
	PollMaxWait    time.Duration
	MaxImageBytes  int64
}

type Image struct {
	ContentType string
	Bytes       []byte
}

type Result struct {
	ConversationID string
	AccountName    string
	Images         []Image
	IsPreview      bool
	ErrorCode      string
	ErrorMessage   string
}

type Runner struct {
	pool         *pool.Pool
	mu           sync.RWMutex
	cooldown429  time.Duration
	requestLimit int64
}

func New(p *pool.Pool, cooldown429 time.Duration, requestLimit int64) *Runner {
	if requestLimit <= 0 {
		requestLimit = 16 * 1024 * 1024
	}
	return &Runner{
		pool:         p,
		cooldown429:  cooldown429,
		requestLimit: requestLimit,
	}
}

func (r *Runner) Update(cooldown429 time.Duration, requestLimit int64) {
	if requestLimit <= 0 {
		requestLimit = 16 * 1024 * 1024
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cooldown429 = cooldown429
	r.requestLimit = requestLimit
}

func (r *Runner) settings() (time.Duration, int64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cooldown429, r.requestLimit
}

func (r *Runner) markRateLimited(accountName string) {
	cooldown429, _ := r.settings()
	r.pool.MarkRateLimited(accountName, cooldown429)
}

func (r *Runner) Run(ctx context.Context, req Request) *Result {
	if req.UpstreamModel == "" {
		req.UpstreamModel = "auto"
	}
	if req.PollMaxWait <= 0 {
		req.PollMaxWait = 5 * time.Minute
	}
	if req.MaxImageBytes <= 0 {
		_, requestLimit := r.settings()
		req.MaxImageBytes = requestLimit
	}

	var (
		result *Result
		code   string
		err    error
	)
	for attempt := 1; attempt <= 2; attempt++ {
		result = &Result{ErrorCode: ErrUnknown}
		ok, attemptCode, attemptErr := r.runOnce(ctx, req, result)
		if ok {
			result.ErrorCode = ""
			result.ErrorMessage = ""
			return result
		}

		code = attemptCode
		err = attemptErr
		if attempt == 1 && retryableNoImageTask(code, err) {
			logger.L().Warn("retry image request after no-image task failure",
				zap.String("account", result.AccountName),
				zap.String("code", code),
				zap.Error(err),
			)
			continue
		}
		break
	}

	result.ErrorCode = code
	if err != nil {
		result.ErrorMessage = err.Error()
	}

	return result
}

type noImageFailureCooldownError struct {
	reason string
	policy pool.NoImageFailurePolicy
}

func (e *noImageFailureCooldownError) Error() string {
	return fmt.Sprintf("no image returned %d time(s) within %s for %s account; account cooled down for %s",
		e.policy.Count, e.policy.Window, e.policy.Plan, e.policy.Cooldown)
}

func retryableNoImageTask(code string, err error) bool {
	if code == ErrNoImageTask {
		return true
	}

	var cooldownErr *noImageFailureCooldownError
	return code == ErrRateLimited &&
		errors.As(err, &cooldownErr) &&
		cooldownErr.reason == "no_image_task"
}

func (r *Runner) runOnce(ctx context.Context, req Request, result *Result) (bool, string, error) {
	acquireCtx := ctx
	cancel := func() {}
	if req.AcquireTimeout > 0 {
		acquireCtx, cancel = context.WithTimeout(ctx, req.AcquireTimeout)
	}
	defer cancel()

	lease, err := r.pool.Acquire(acquireCtx)
	if err != nil {
		if errors.Is(err, pool.ErrNoAvailable) {
			return false, ErrNoAccount, err
		}
		return false, ErrUnknown, err
	}
	defer lease.Release()

	snap := lease.Snapshot()
	result.AccountName = snap.Name

	taskCtx := ctx
	cancelTask := func() {}
	if req.TaskTimeout > 0 {
		taskCtx, cancelTask = context.WithTimeout(ctx, req.TaskTimeout)
	}
	defer cancelTask()

	client, err := chatgpt.New(chatgpt.Options{
		AuthToken: snap.AuthToken,
		DeviceID:  snap.DeviceID,
		SessionID: snap.SessionID,
		ProxyURL:  snap.ProxyURL,
		Cookies:   snap.Cookies,
	})
	if err != nil {
		return false, ErrUnknown, fmt.Errorf("create chatgpt client: %w", err)
	}

	refs, code, err := r.uploadReferences(taskCtx, client, snap.Name, req.References)
	if err != nil {
		return false, code, err
	}

	res, code, err := r.runConversation(taskCtx, client, snap.Name, req, refs)
	if err != nil {
		return false, code, err
	}

	result.ConversationID = res.ConversationID
	result.IsPreview = res.IsPreview
	result.Images = res.Images
	return true, "", nil
}

type conversationResult struct {
	ConversationID string
	Images         []Image
	IsPreview      bool
}

func (r *Runner) runConversation(
	ctx context.Context,
	client *chatgpt.Client,
	accountName string,
	req Request,
	refs []*chatgpt.UploadedFile,
) (*conversationResult, string, error) {
	var (
		convID       string
		parentID     = uuid.NewString()
		messageID    = uuid.NewString()
		fileRefs     []string
		isPreview    bool
		inputFileIDs = uploadedFileIDSet(refs)
		inputHashes  = referenceImageHashes(req.References)
		isEdit       = len(req.References) > 0
	)

	cr, proofToken, code, err := r.getRequirements(ctx, client, accountName)
	if err != nil {
		return nil, code, err
	}

	convOpt := chatgpt.ImageConvOpts{
		Prompt:        req.Prompt,
		UpstreamModel: req.UpstreamModel,
		ConvID:        convID,
		ParentMsgID:   parentID,
		MessageID:     messageID,
		ChatToken:     cr.Token,
		ProofToken:    proofToken,
		References:    refs,
	}

	if conduitToken, err := client.PrepareFConversation(ctx, convOpt); err == nil {
		convOpt.ConduitToken = conduitToken
	} else {
		code := r.classifyUpstream(err)
		if code == ErrRateLimited {
			r.markRateLimited(accountName)
		}
		if code == ErrAuthRequired {
			r.pool.MarkUnauthorized(accountName)
		}
		return nil, code, err
	}

	stream, err := client.StreamFConversation(ctx, convOpt)
	if err != nil {
		code := r.classifyUpstream(err)
		if code == ErrRateLimited {
			r.markRateLimited(accountName)
		}
		if code == ErrAuthRequired {
			r.pool.MarkUnauthorized(accountName)
		}
		return nil, code, err
	}

	sseResult := chatgpt.ParseImageSSE(stream)
	if sseResult.ConversationID != "" {
		convID = sseResult.ConversationID
	}

	logger.L().Info("sidecar image sse parsed",
		zap.String("account", accountName),
		zap.String("conversation_id", convID),
		zap.Int("file_refs", len(sseResult.FileIDs)),
		zap.Int("sediment_refs", len(sseResult.SedimentIDs)),
		zap.String("image_gen_task_id", sseResult.ImageGenTaskID),
		zap.String("finish_type", sseResult.FinishType),
		zap.Bool("image_edit", isEdit),
	)

	ignoredSedimentIDs := map[string]struct{}{}
	for _, fid := range sseResult.FileIDs {
		if _, isInput := inputFileIDs[fid]; isInput {
			logger.L().Warn("ignore echoed reference image from sse",
				zap.String("account", accountName),
				zap.String("file_id", fid),
			)
			continue
		}
		fileRefs = append(fileRefs, fid)
	}
	for _, sid := range sseResult.SedimentIDs {
		if isEdit {
			ignoredSedimentIDs[sid] = struct{}{}
			logger.L().Info("ignore initial sediment preview from sse for image edit",
				zap.String("account", accountName),
				zap.String("sediment_id", sid),
			)
			continue
		}
		fileRefs = append(fileRefs, "sed:"+sid)
	}

	if len(fileRefs) == 0 && len(sseResult.FileIDs) == 0 && len(sseResult.SedimentIDs) == 0 && strings.TrimSpace(sseResult.ImageGenTaskID) == "" {
		logger.L().Warn("sidecar image task not started",
			zap.String("account", accountName),
			zap.String("persona", cr.Persona),
			zap.String("conversation_id", convID),
			zap.String("finish_type", sseResult.FinishType),
			zap.Bool("image_edit", isEdit),
		)
		if err := r.recordNoImageFailure(accountName, cr.Persona, convID, "no_image_task"); err != nil {
			return nil, ErrRateLimited, err
		}
		return nil, ErrNoImageTask, errors.New("upstream did not start an image generation task")
	}

	if len(fileRefs) == 0 {
		status, fids, sids := client.PollConversationForImages(ctx, convID, chatgpt.PollOpts{
			MaxWait:            req.PollMaxWait,
			IgnoreFileIDs:      inputFileIDs,
			IgnoreSedimentIDs:  ignoredSedimentIDs,
			ExpectedN:          1,
			RequireFileService: false,
		})
		logger.L().Info("sidecar image poll done",
			zap.String("account", accountName),
			zap.String("conversation_id", convID),
			zap.String("status", string(status)),
			zap.Int("file_refs", len(fids)),
			zap.Int("sediment_refs", len(sids)),
		)
		switch status {
		case chatgpt.PollStatusSuccess:
			fileRefs = append(fileRefs, fids...)
			for _, sid := range sids {
				fileRefs = append(fileRefs, "sed:"+sid)
			}

		case chatgpt.PollStatusTimeout:
			if err := r.recordNoImageFailure(accountName, cr.Persona, convID, "poll_timeout"); err != nil {
				return nil, ErrRateLimited, err
			}
			if isEdit && len(sseResult.SedimentIDs) > 0 && len(fids)+len(sids) == 0 {
				return nil, ErrPreviewOnly, errors.New("only initial sediment preview refs returned; no final image ref")
			}
			if ctx.Err() != nil {
				return nil, ErrPollTimeout, fmt.Errorf("poll timeout: %w", ctx.Err())
			}
			return nil, ErrPollTimeout, errors.New("poll timeout")

		default:
			return nil, ErrUpstream, errors.New("poll returned error")
		}
	}

	if len(fileRefs) == 0 {
		if err := r.recordNoImageFailure(accountName, cr.Persona, convID, "no_image_refs"); err != nil {
			return nil, ErrRateLimited, err
		}
		return nil, ErrPollTimeout, errors.New("no image refs produced")
	}

	images := make([]Image, 0, len(fileRefs))
	for _, ref := range fileRefs {
		downloadURL, err := client.ImageDownloadURL(ctx, convID, ref)
		if err != nil {
			logger.L().Warn("sidecar image download url failed",
				zap.String("account", accountName),
				zap.String("ref", ref),
				zap.Error(err),
			)
			continue
		}

		body, contentType, err := client.FetchImage(ctx, downloadURL, req.MaxImageBytes)
		if err != nil {
			logger.L().Warn("sidecar fetch image failed",
				zap.String("account", accountName),
				zap.String("ref", ref),
				zap.Error(err),
			)
			continue
		}
		if isEdit {
			if _, ok := inputHashes[sha256.Sum256(body)]; ok {
				logger.L().Warn("skip downloaded image matching reference input",
					zap.String("account", accountName),
					zap.String("ref", ref),
				)
				continue
			}
		}

		images = append(images, Image{
			ContentType: contentType,
			Bytes:       body,
		})
	}
	if len(images) == 0 {
		return nil, ErrDownload, errors.New("all image downloads failed")
	}

	return &conversationResult{
		ConversationID: convID,
		Images:         images,
		IsPreview:      isPreview,
	}, "", nil
}

func uploadedFileIDSet(files []*chatgpt.UploadedFile) map[string]struct{} {
	out := make(map[string]struct{}, len(files))
	for _, file := range files {
		if file == nil || file.FileID == "" {
			continue
		}
		out[file.FileID] = struct{}{}
	}
	return out
}

func referenceImageHashes(references []ReferenceImage) map[[32]byte]struct{} {
	out := make(map[[32]byte]struct{}, len(references))
	for _, ref := range references {
		if len(ref.Data) == 0 {
			continue
		}
		out[sha256.Sum256(ref.Data)] = struct{}{}
	}
	return out
}

func (r *Runner) recordNoImageFailure(accountName, persona, conversationID, reason string) error {
	result := r.pool.RecordNoImageFailure(accountName, persona)
	fields := []zap.Field{
		zap.String("account", accountName),
		zap.String("persona", result.Persona),
		zap.String("plan", result.Plan),
		zap.String("conversation_id", conversationID),
		zap.String("reason", reason),
		zap.Int("count", result.Count),
		zap.Int("threshold", result.Threshold),
		zap.Duration("window", result.Window),
		zap.Duration("cooldown", result.Cooldown),
		zap.Bool("cooldown_applied", result.CooldownApplied),
	}
	if !result.CooldownUntil.IsZero() {
		fields = append(fields, zap.Time("cooldown_until", result.CooldownUntil))
	}
	logger.L().Warn("sidecar no-image failure recorded", fields...)
	if !result.CooldownApplied {
		return nil
	}
	return &noImageFailureCooldownError{
		reason: reason,
		policy: result,
	}
}

func (r *Runner) uploadReferences(
	ctx context.Context,
	client *chatgpt.Client,
	accountName string,
	references []ReferenceImage,
) ([]*chatgpt.UploadedFile, string, error) {
	if len(references) == 0 {
		return nil, "", nil
	}

	out := make([]*chatgpt.UploadedFile, 0, len(references))
	for idx, ref := range references {
		uploaded, err := client.UploadFile(ctx, ref.Data, ref.FileName)
		if err != nil {
			code := r.classifyUpstream(err)
			if code == ErrRateLimited {
				r.markRateLimited(accountName)
			}
			if code == ErrAuthRequired {
				r.pool.MarkUnauthorized(accountName)
			}
			return nil, code, fmt.Errorf("upload reference %d: %w", idx+1, err)
		}
		out = append(out, uploaded)
	}

	return out, "", nil
}

func (r *Runner) getRequirements(
	ctx context.Context,
	client *chatgpt.Client,
	accountName string,
) (*chatgpt.ChatRequirementsResp, string, string, error) {
	cr, err := client.ChatRequirementsV2(ctx)
	if err != nil {
		code := r.classifyUpstream(err)
		if code == ErrRateLimited {
			r.markRateLimited(accountName)
		}
		if code == ErrAuthRequired {
			r.pool.MarkUnauthorized(accountName)
		}
		return nil, "", code, err
	}
	r.pool.MarkPersona(accountName, cr.Persona)

	var proofToken string
	if cr.Proofofwork.Required {
		proofCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()

		ch := make(chan string, 1)
		go func() {
			ch <- cr.SolveProof(chatgpt.DefaultUserAgent)
		}()

		select {
		case <-proofCtx.Done():
			return nil, "", ErrPOWTimeout, proofCtx.Err()
		case proofToken = <-ch:
			if proofToken == "" {
				return nil, "", ErrPOWFailed, errors.New("pow solver returned empty")
			}
		}
	}

	if cr.Turnstile.Required {
		logger.L().Warn("sidecar turnstile required, continuing with single-step fallback",
			zap.String("account", accountName),
		)
	}

	return cr, proofToken, "", nil
}

func (r *Runner) classifyUpstream(err error) string {
	if err == nil {
		return ""
	}

	var upstreamErr *chatgpt.UpstreamError
	if errors.As(err, &upstreamErr) {
		if upstreamErr.IsRateLimited() {
			return ErrRateLimited
		}
		if upstreamErr.IsUnauthorized() {
			return ErrAuthRequired
		}
		return ErrUpstream
	}

	msg := err.Error()
	if strings.Contains(strings.ToLower(msg), "deadline exceeded") {
		return ErrPollTimeout
	}
	if strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "broken pipe") {
		return ErrNetworkTransient
	}

	return ErrUpstream
}
