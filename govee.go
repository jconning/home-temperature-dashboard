package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// GoveeClient talks to the Govee OpenAPI (https://developer.govee.com).
//
// The API only exposes the *current* state of a device; there is no history
// endpoint, which is why the app polls and caches readings in MySQL.
type GoveeClient struct {
	key  string
	base string
	http *http.Client
}

func NewGoveeClient(key string) *GoveeClient {
	return &GoveeClient{
		key:  key,
		base: "https://openapi.api.govee.com/router/api/v1",
		http: &http.Client{Timeout: 20 * time.Second},
	}
}

type GoveeDevice struct {
	SKU          string `json:"sku"`
	Device       string `json:"device"`
	DeviceName   string `json:"deviceName"`
	Type         string `json:"type"`
	Capabilities []struct {
		Type     string `json:"type"`
		Instance string `json:"instance"`
	} `json:"capabilities"`
}

// IsThermometer reports whether the device exposes a temperature reading.
func (d GoveeDevice) IsThermometer() bool {
	for _, c := range d.Capabilities {
		if c.Instance == "sensorTemperature" {
			return true
		}
	}
	return false
}

type DeviceState struct {
	Online   bool
	TempF    *float64 // the API reports °F
	Humidity *float64 // %RH
}

func (c *GoveeClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Govee-API-Key", c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return fmt.Errorf("govee %s: unauthorized (check API key)", path)
	case http.StatusTooManyRequests:
		return fmt.Errorf("govee %s: rate limited (429)", path)
	default:
		return fmt.Errorf("govee %s: HTTP %d: %.200s", path, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("govee %s: decode: %w", path, err)
	}
	return nil
}

func (c *GoveeClient) Devices(ctx context.Context) ([]GoveeDevice, error) {
	var resp struct {
		Code    int           `json:"code"`
		Message string        `json:"message"`
		Data    []GoveeDevice `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/user/devices", nil, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 200 {
		return nil, fmt.Errorf("govee devices: code %d: %s", resp.Code, resp.Message)
	}
	return resp.Data, nil
}

func (c *GoveeClient) State(ctx context.Context, sku, device string) (DeviceState, error) {
	reqBody := map[string]any{
		"requestId": newRequestID(),
		"payload":   map[string]string{"sku": sku, "device": device},
	}
	var resp struct {
		Code    int    `json:"code"`
		Msg     string `json:"msg"`
		Payload struct {
			Capabilities []struct {
				Instance string `json:"instance"`
				State    struct {
					Value json.RawMessage `json:"value"`
				} `json:"state"`
			} `json:"capabilities"`
		} `json:"payload"`
	}
	var st DeviceState
	if err := c.do(ctx, http.MethodPost, "/device/state", reqBody, &resp); err != nil {
		return st, err
	}
	if resp.Code != 200 {
		return st, fmt.Errorf("govee state %s: code %d: %s", device, resp.Code, resp.Msg)
	}
	for _, cp := range resp.Payload.Capabilities {
		switch cp.Instance {
		case "online":
			_ = json.Unmarshal(cp.State.Value, &st.Online)
		case "sensorTemperature":
			var v float64
			if json.Unmarshal(cp.State.Value, &v) == nil {
				st.TempF = &v
			}
		case "sensorHumidity":
			var v float64
			if json.Unmarshal(cp.State.Value, &v) == nil {
				st.Humidity = &v
			}
		}
	}
	return st, nil
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
