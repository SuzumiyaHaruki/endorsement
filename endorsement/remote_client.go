package endorsement

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/log"
	"github.com/offchainlabs/nitro/endorsementpolicy"
)

type RemoteEndorsementClient struct {
	// EndorserID -> baseURL
	Endpoints map[endorsementpolicy.EndorserID]string

	// 可选；为空时使用默认 client
	HTTPClient *http.Client
}

type remoteEndorsementError struct {
	Error string `json:"error"`
}

func validateRemoteEndorsementResponse(
	req *EndorsementRequest,
	endorser endorsementpolicy.EndorserID,
	resp *EndorsementResponse,
) error {
	if req == nil {
		return fmt.Errorf("nil endorsement request")
	}
	if resp == nil {
		return fmt.Errorf("nil endorsement response")
	}

	switch resp.Decision {
	case EndorsementDecisionAccept:
		if len(resp.Signature) == 0 {
			return fmt.Errorf("accept response missing signature")
		}
	case EndorsementDecisionReject:
		if len(resp.Signature) != 0 {
			return fmt.Errorf("reject response should not include signature")
		}
	default:
		return fmt.Errorf("unknown endorsement decision %q", resp.Decision)
	}

	return nil
}

func (c *RemoteEndorsementClient) RequestEndorsement(
	ctx context.Context,
	endorser endorsementpolicy.EndorserID,
	req *EndorsementRequest,
) (*EndorsementResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("nil endorsement request")
	}
	if c == nil {
		return nil, fmt.Errorf("nil remote endorsement client")
	}
	if len(c.Endpoints) == 0 {
		return nil, fmt.Errorf("empty remote endorsement endpoints")
	}

	baseURL, ok := c.Endpoints[endorser]
	if !ok || strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("no remote endpoint configured for endorser %s", endorser)
	}
	url := strings.TrimRight(baseURL, "/") + "/endorse"

	log.Info("REMOTE_ENDORSEMENT_REQUEST",
		"endorser", endorser,
		"url", url,
		"requestID", req.Envelope.RequestID,
		"txHash", req.Envelope.TxHash,
		"txIndex", req.Envelope.TxIndex,
	)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal endorsement request for %s: %w", endorser, err)
	}

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create remote endorsement request for %s: %w", endorser, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		log.Warn("REMOTE_ENDORSEMENT_HTTP_ERROR",
			"endorser", endorser,
			"url", url,
			"err", err,
		)
		return nil, fmt.Errorf("call remote endorser %s at %s: %w", endorser, url, err)
	}
	defer httpResp.Body.Close()

	respBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read remote response from %s: %w", endorser, err)
	}

	if httpResp.StatusCode != http.StatusOK {
		log.Warn("REMOTE_ENDORSEMENT_NON_200",
			"endorser", endorser,
			"url", url,
			"status", httpResp.StatusCode,
			"body", string(respBytes),
		)

		var remoteErr remoteEndorsementError
		if err := json.Unmarshal(respBytes, &remoteErr); err == nil && remoteErr.Error != "" {
			return nil, fmt.Errorf("remote endorser %s returned %d: %s", endorser, httpResp.StatusCode, remoteErr.Error)
		}
		return nil, fmt.Errorf("remote endorser %s returned %d: %s", endorser, httpResp.StatusCode, string(respBytes))
	}

	var resp EndorsementResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal remote endorsement response from %s: %w", endorser, err)
	}

	if resp.EndorserID == "" {
		log.Debug("REMOTE_ENDORSEMENT_MISSING_ENDORSER_ID",
			"endorser", endorser,
			"requestID", req.Envelope.RequestID,
		)
		resp.EndorserID = endorser
	}

	if resp.RequestID != req.Envelope.RequestID {
		return nil, fmt.Errorf(
			"remote endorser %s returned mismatched request id: got %s want %s",
			endorser, resp.RequestID.Hex(), req.Envelope.RequestID.Hex(),
		)
	}
	if resp.EndorserID != endorser {
		return nil, fmt.Errorf(
			"remote endorser id mismatch: got %s want %s",
			resp.EndorserID, endorser,
		)
	}
	if err := validateRemoteEndorsementResponse(req, endorser, &resp); err != nil {
		return nil, fmt.Errorf("invalid remote endorsement response from %s: %w", endorser, err)
	}

	log.Info("REMOTE_ENDORSEMENT_RESPONSE",
		"endorser", endorser,
		"decision", resp.Decision,
		"reasonCode", resp.ReasonCode,
		"sigLen", len(resp.Signature),
		"requestID", resp.RequestID,
	)

	return &resp, nil
}