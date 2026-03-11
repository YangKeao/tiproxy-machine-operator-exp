package imds

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	metadataAddr = "http://169.254.169.254"
)

func ResolveMachineID(ctx context.Context, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("TIPROXY_MACHINE_ID"); v != "" {
		return v
	}
	if id, err := queryInstanceID(ctx); err == nil && id != "" {
		return id
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "unknown-machine"
}

func queryInstanceID(ctx context.Context) (string, error) {
	client := &http.Client{Timeout: 1 * time.Second}

	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPut, metadataAddr+"/latest/api/token", nil)
	if err != nil {
		return "", err
	}
	tokenReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	tokenResp, err := client.Do(tokenReq)
	var token string
	if err == nil {
		defer tokenResp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(tokenResp.Body, 4096))
		if tokenResp.StatusCode >= 200 && tokenResp.StatusCode < 300 {
			token = strings.TrimSpace(string(b))
		}
	}

	idReq, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataAddr+"/latest/meta-data/instance-id", nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		idReq.Header.Set("X-aws-ec2-metadata-token", token)
	}
	idResp, err := client.Do(idReq)
	if err != nil {
		return "", err
	}
	defer idResp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(idResp.Body, 4096))
	if err != nil {
		return "", err
	}
	if idResp.StatusCode < 200 || idResp.StatusCode >= 300 {
		return "", &httpStatusError{statusCode: idResp.StatusCode}
	}
	return strings.TrimSpace(string(b)), nil
}

type httpStatusError struct {
	statusCode int
}

func (e *httpStatusError) Error() string {
	return "metadata server returned non-success status"
}
