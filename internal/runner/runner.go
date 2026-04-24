package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	ErrPreviewOnly      = "preview_only"
	ErrPollTimeout      = "poll_timeout"
	ErrDownload         = "download_failed"
	ErrUpstream         = "upstream_error"
	defaultTransientCooldown = 30 * time.Second
)

type ReferenceImage struct {
	Data     []byte
	FileName string
}

type Request struct {
	Prompt         string
	UpstreamModel  string
	References     []ReferenceImage
	MaxAttempts    int
	AcquireTimeout time.Duration
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
	Attempts       int
}

type Runner struct {
	pool         *pool.Pool
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

func (r *Runner) Run(ctx context.Context, req Request) *Result {
	if req.UpstreamModel == "" {
		req.UpstreamModel = "auto"
	}
	if req.MaxAttempts <= 0 {
		req.MaxAttempts = 2
	}
	if req.PollMaxWait <= 0 {
		req.PollMaxWait = 5 * time.Minute
	}
	if req.MaxImageBytes <= 0 {
		req.MaxImageBytes = r.requestLimit
	}

	result := &Result{ErrorCode: ErrUnknown}
	for attempt := 1; attempt <= req.MaxAttempts; attempt++ {
		result.Attempts = attempt

		ok, code, err := r.runOnce(ctx, req, result)
		if ok {
			result.ErrorCode = ""
			result.ErrorMessage = ""
			return result
		}

		result.ErrorCode = code
		if err != nil {
			result.ErrorMessage = err.Error()
		}

		retryable := code == ErrPreviewOnly || code == ErrRateLimited ||
			code == ErrAuthRequired || code == ErrNetworkTransient ||
			code == ErrPollTimeout || code == ErrDownload || code == ErrUpstream
		if !retryable {
			return result
		}
	}

	return result
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

	refs, code, err := r.uploadReferences(ctx, client, snap.Name, req.References)
	if err != nil {
		return false, code, err
	}

	res, code, err := r.runConversation(ctx, client, snap.Name, req, refs)
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
	const sameConvMax = 3

	var (
		convID        string
		parentID      = uuid.NewString()
		messageID     = uuid.NewString()
		fileRefs      []string
		baselineTools = map[string]struct{}{}
		isPreview     bool
	)

	for turn := 1; turn <= sameConvMax; turn++ {
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
		if turn > 1 {
			convOpt.MessageID = uuid.NewString()
		}

		if conduitToken, err := client.PrepareFConversation(ctx, convOpt); err == nil {
			convOpt.ConduitToken = conduitToken
		} else {
			code := r.classifyUpstream(err)
			if code == ErrRateLimited {
				r.pool.MarkRateLimited(accountName, r.cooldown429)
			}
			if code == ErrAuthRequired {
				r.pool.MarkUnauthorized(accountName)
			}
			if code == ErrNetworkTransient || code == ErrUpstream {
				r.markTransientFailure(accountName)
			}
			return nil, code, err
		}

		stream, err := client.StreamFConversation(ctx, convOpt)
		if err != nil {
			code := r.classifyUpstream(err)
			if code == ErrRateLimited {
				r.pool.MarkRateLimited(accountName, r.cooldown429)
			}
			if code == ErrAuthRequired {
				r.pool.MarkUnauthorized(accountName)
			}
			if code == ErrNetworkTransient || code == ErrUpstream {
				r.markTransientFailure(accountName)
			}
			return nil, code, err
		}

		sseResult := chatgpt.ParseImageSSE(stream)
		if sseResult.ConversationID != "" {
			convID = sseResult.ConversationID
		}

		logger.L().Info("sidecar image sse parsed",
			zap.String("account", accountName),
			zap.Int("turn", turn),
			zap.String("conversation_id", convID),
			zap.Int("file_refs", len(sseResult.FileIDs)),
			zap.Int("sediment_refs", len(sseResult.SedimentIDs)),
		)

		fileRefs = append(fileRefs, sseResult.FileIDs...)
		for _, sid := range sseResult.SedimentIDs {
			fileRefs = append(fileRefs, "sed:"+sid)
		}
		if len(fileRefs) > 0 {
			break
		}

		status, fids, sids := client.PollConversationForImages(ctx, convID, chatgpt.PollOpts{
			MaxWait:         req.PollMaxWait,
			BaselineToolIDs: baselineTools,
			ExpectedN:       1,
		})
		switch status {
		case chatgpt.PollStatusSuccess:
			fileRefs = append(fileRefs, fids...)
			for _, sid := range sids {
				fileRefs = append(fileRefs, "sed:"+sid)
			}
			break

		case chatgpt.PollStatusTimeout:
			r.markTransientFailure(accountName)
			return nil, ErrPollTimeout, errors.New("poll timeout")

		default:
			r.markTransientFailure(accountName)
			return nil, ErrUpstream, errors.New("poll returned error")
		}

		if len(fileRefs) > 0 {
			break
		}
	}

	if len(fileRefs) == 0 {
		r.markTransientFailure(accountName)
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

		images = append(images, Image{
			ContentType: contentType,
			Bytes:       body,
		})
	}
	if len(images) == 0 {
		r.markTransientFailure(accountName)
		return nil, ErrDownload, errors.New("all image downloads failed")
	}

	return &conversationResult{
		ConversationID: convID,
		Images:         images,
		IsPreview:      isPreview,
	}, "", nil
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
				r.pool.MarkRateLimited(accountName, r.cooldown429)
			}
			if code == ErrAuthRequired {
				r.pool.MarkUnauthorized(accountName)
			}
			if code == ErrNetworkTransient || code == ErrUpstream {
				r.markTransientFailure(accountName)
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
			r.pool.MarkRateLimited(accountName, r.cooldown429)
		}
		if code == ErrAuthRequired {
			r.pool.MarkUnauthorized(accountName)
		}
		if code == ErrNetworkTransient || code == ErrUpstream {
			r.markTransientFailure(accountName)
		}
		return nil, "", code, err
	}

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

func (r *Runner) markTransientFailure(accountName string) {
	cooldown := time.Duration(defaultTransientCooldown)
	if r.cooldown429 > 0 && r.cooldown429 < cooldown {
		cooldown = r.cooldown429
	}
	r.pool.MarkCooldown(accountName, cooldown)
}

func buildToolBaseline(mapping map[string]interface{}) map[string]struct{} {
	tools := chatgpt.ExtractImageToolMsgs(mapping)
	if len(tools) == 0 {
		return nil
	}

	out := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		out[tool.MessageID] = struct{}{}
	}
	return out
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
