package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/integrations/fofa"
	actool "github.com/Autumn-27/norma/tool"
)

func (s *Server) runFOFARequest(ctx context.Context, key string, config db.FOFAConfig, validate func() error, request func() error) (err error) {
	lease, err := s.m.pg.AcquireFOFARequest(ctx, key, time.Duration(config.RequestIntervalSeconds)*time.Second)
	if err != nil {
		var waiting *db.FOFACooldownError
		if errors.As(err, &waiting) {
			return &fofa.RateLimitError{RetryAfter: waiting.RetryAfter}
		}
		return err
	}
	defer func() {
		var cooldown time.Duration
		var limited *fofa.RateLimitError
		if errors.As(err, &limited) {
			cooldown = max(time.Duration(config.RateLimitCooldownSeconds)*time.Second, limited.RetryAfter)
			err = &fofa.RateLimitError{RetryAfter: cooldown}
		}
		if finishErr := lease.Finish(cooldown); finishErr != nil {
			err = errors.Join(err, db.ErrFOFAThrottleUnsettled)
		}
	}()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if validate != nil {
		if err = validate(); err != nil {
			return err
		}
	}
	return request()
}

func fofaRetrySeconds(delay time.Duration) int64 {
	seconds := int64(delay / time.Second)
	if delay%time.Second > 0 {
		seconds++
	}
	return max(seconds, 0)
}

func fofaToolError(err error) actool.Result {
	if errors.Is(err, db.ErrFOFAThrottleUnsettled) {
		return actool.Errorf(db.ErrFOFAThrottleUnsettled.Error())
	}
	var limited *fofa.RateLimitError
	if errors.As(err, &limited) {
		seconds := fofaRetrySeconds(limited.RetryAfter)
		result, _ := jsonResult(map[string]any{"code": "rate_limited", "retry_after_seconds": seconds, "message": fmt.Sprintf("FOFA 当前 Key 处于共享冷却中，请至少等待 %d 秒。不要立即重试、增加并发或更换代理规避冷却。", seconds)})
		result.IsError = true
		return result
	}
	return actool.Errorf(err.Error())
}
