package fofa

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/url"
)

// AccountInfo 仅公开余额和额度；缺失或未知值保留为 null，不能当作零余额。
type AccountInfo struct {
	FofaPoint       *float64 `json:"fofa_point"`
	RemainFreePoint *float64 `json:"remain_free_point"`
	RemainAPIQuery  *int64   `json:"remain_api_query"`
	RemainAPIData   *int64   `json:"remain_api_data"`
}

func (c *Client) Account(ctx context.Context) (*AccountInfo, error) {
	data, err := c.get(ctx, "/info/my", url.Values{})
	if err != nil {
		return nil, err
	}
	// 使用精确顶层键名取值，额外字段和大小写别名均不进入公开结构。
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, malformedResponse()
	}
	account := &AccountInfo{}
	for _, field := range []struct {
		name   string
		target **float64
	}{
		{"fofa_point", &account.FofaPoint},
		{"remain_free_point", &account.RemainFreePoint},
	} {
		value, err := accountPoint(fields[field.name])
		if err != nil {
			return nil, err
		}
		*field.target = value
	}
	for _, field := range []struct {
		name   string
		target **int64
	}{
		{"remain_api_query", &account.RemainAPIQuery},
		{"remain_api_data", &account.RemainAPIData},
	} {
		value, err := accountCount(fields[field.name])
		if err != nil {
			return nil, err
		}
		*field.target = value
	}
	return account, nil
}

func accountPoint(raw json.RawMessage) (*float64, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, malformedResponse()
	}
	if value == 0 {
		// 极小的非零数字不能因浮点下溢被误显示成真实零余额。
		for _, char := range raw {
			if char == 'e' || char == 'E' {
				break
			}
			if char >= '1' && char <= '9' {
				return nil, malformedResponse()
			}
		}
		value = 0
	}
	return &value, nil
}

func accountCount(raw json.RawMessage) (*int64, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	// 官方 SDK 将这两项定义为整数；直接解析避免大整数先转浮点后被舍入。
	var value int64
	if json.Unmarshal(raw, &value) != nil || value < 0 || value > 9007199254740991 {
		return nil, malformedResponse()
	}
	return &value, nil
}
