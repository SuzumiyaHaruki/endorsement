package endorsement

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"errors"
	"github.com/offchainlabs/nitro/endorsementpolicy"
)

type HTTPEndorserServer struct {
	EndorserID endorsementpolicy.EndorserID
	Signer     *BLSEndorsementClient
	PublicKey  []byte
}

type endorserPublicKeyResponse struct {
	EndorserID   endorsementpolicy.EndorserID `json:"endorser_id"`
	PublicKeyHex string                       `json:"public_key_hex"`
}

type endorserErrorResponse struct {
	Error string `json:"error"`
}


// NewHTTPEndorserHandler 修复空 server 风险
func NewHTTPEndorserHandler(server *HTTPEndorserServer) (http.Handler, error) {
	if server == nil {
		return nil, errors.New("server cannot be nil")
	}
	if server.Signer == nil {
		return nil, errors.New("server.Signer cannot be nil")
	}
	if len(server.PublicKey) == 0 {
		return nil, errors.New("server.PublicKey cannot be empty")
	}

	mux := http.NewServeMux()

	// 健康检查
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"endorser_id": server.EndorserID,
		})
	})

	// 获取公钥
	mux.HandleFunc("/pubkey", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, endorserPublicKeyResponse{
			EndorserID:   server.EndorserID,
			PublicKeyHex: hex.EncodeToString(server.PublicKey),
		})
	})

	// 背书请求
	mux.HandleFunc("/endorse", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		// 再次兜底，保证 Signer 不为 nil
		if server.Signer == nil {
			writeJSONError(w, http.StatusInternalServerError, "endorser signer not initialized")
			return
		}

		var req EndorsementRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid endorsement request: "+err.Error())
			return
		}

		resp, err := server.Signer.RequestEndorsement(r.Context(), server.EndorserID, &req)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "endorsement failed: "+err.Error())
			return
		}

		writeJSON(w, http.StatusOK, resp)
	})

	return mux, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, endorserErrorResponse{Error: msg})
}