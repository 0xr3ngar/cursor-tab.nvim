package cursor

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	aiserverv1 "github.com/bengu3/cursor-tab.nvim/cursor-api/gen/aiserver/v1"
	"github.com/bengu3/cursor-tab.nvim/cursor-api/gen/aiserver/v1/aiserverv1connect"
)

const APIBaseURL = "https://api4.cursor.sh"

type Client struct {
	aiClient      aiserverv1connect.AiServiceClient
	accessToken   string
	machineID     string
	clientVersion string
}

func NewClient() (*Client, error) {
	accessToken, err := GetAccessToken()
	if err != nil {
		return nil, fmt.Errorf("failed to get access token: %w", err)
	}

	machineID, err := GetMachineID()
	if err != nil {
		return nil, fmt.Errorf("failed to get machine ID: %w", err)
	}

	clientVersion, err := GetCursorVersion()
	if err != nil {
		return nil, fmt.Errorf("failed to get Cursor version: %w", err)
	}

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
	}
	aiClient := aiserverv1connect.NewAiServiceClient(httpClient, APIBaseURL)

	return &Client{
		aiClient:      aiClient,
		accessToken:   accessToken,
		machineID:     machineID,
		clientVersion: clientVersion,
	}, nil
}

// generateChecksum creates the x-cursor-checksum header value.
// Matches the Cursor IDE reference implementation.
func (c *Client) generateChecksum() string {
	timestamp := uint64(time.Now().UnixNano() / 1e6)

	timestampBytes := []byte{
		byte(timestamp >> 40),
		byte(timestamp >> 32),
		byte(timestamp >> 24),
		byte(timestamp >> 16),
		byte(timestamp >> 8),
		byte(timestamp),
	}

	// Encrypt bytes using the reference algorithm
	w := byte(165)
	for i := 0; i < len(timestampBytes); i++ {
		timestampBytes[i] = (timestampBytes[i] ^ w) + byte(i%256)
		w = timestampBytes[i]
	}

	base64Encoded := base64.StdEncoding.EncodeToString(timestampBytes)
	return fmt.Sprintf("%s%s", base64Encoded, c.machineID)
}

func (c *Client) StreamCpp(ctx context.Context, req *aiserverv1.StreamCppRequest) (*connect.ServerStreamForClient[aiserverv1.StreamCppResponse], error) {
	// Re-read access token on every call so token refreshes are picked up
	// without restarting the server. The sqlite query is local and sub-ms.
	accessToken, err := GetAccessToken()
	if err != nil {
		return nil, fmt.Errorf("failed to refresh access token: %w", err)
	}

	connectReq := connect.NewRequest(req)
	connectReq.Header().Set("authorization", "Bearer "+accessToken)
	connectReq.Header().Set("x-cursor-client-version", c.clientVersion)
	connectReq.Header().Set("x-cursor-machine-id", c.machineID)
	connectReq.Header().Set("x-cursor-checksum", c.generateChecksum())

	stream, err := c.aiClient.StreamCpp(ctx, connectReq)
	if err != nil {
		return nil, fmt.Errorf("failed to call StreamCpp: %w", err)
	}

	return stream, nil
}
