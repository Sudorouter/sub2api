// sudoapi: Account responses test.

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type adminResponsesTestRepo struct {
	service.AccountRepository
	accounts map[int64]service.Account
	lookups  []int64
}

func (repo *adminResponsesTestRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	repo.lookups = append(repo.lookups, id)
	account, found := repo.accounts[id]
	if !found {
		return nil, service.ErrAccountNotFound
	}
	return &account, nil
}

type adminResponsesTestUpstream struct {
	service.HTTPUpstream
	forward func(*http.Request, int64) *http.Response
}

func (upstream adminResponsesTestUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	return upstream.forward(req, accountID), nil
}

func TestAdminTestResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const prefix = "/api/v1/admin/accounts/"
	const suffix = "/test/v1/messages"
	const requestBody = `{"model":"gpt-5.2","input":"hello"}`
	const jsonResponse = `{"id":"resp_123","object":"response","model":"gpt-5.2","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	const sseResponse = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + jsonResponse + "}\n\n"
	future := time.Now().Add(time.Hour)
	for _, tt := range []struct {
		name          string
		accountID     string
		body          string
		adjust        func(*service.Account)
		oauth         bool
		upstreamCode  int
		upstreamBody  string
		upstreamMIME  string
		wantStatus    int
		wantText      string
		wantAccountID int64
		jwt           bool
	}{
		{name: "json", body: requestBody, upstreamCode: 200, upstreamBody: jsonResponse, upstreamMIME: "application/json", wantStatus: 200, wantText: `"object":"response"`, wantAccountID: 7},
		{name: "stream", body: `{"model":"gpt-5.2","input":"hello","stream":true}`, upstreamCode: 200, upstreamBody: sseResponse, upstreamMIME: "text/event-stream", wantStatus: 200, wantText: "response.completed", wantAccountID: 7},
		{name: "OAuth stream converted to JSON", body: requestBody, oauth: true, upstreamCode: 200, upstreamBody: sseResponse, upstreamMIME: "text/event-stream", wantStatus: 200, wantText: `"object":"response"`, wantAccountID: 7},
		{name: "upstream failure", body: requestBody, upstreamCode: 503, upstreamBody: `{"error":{"message":"Unavailable"}}`, upstreamMIME: "application/json", wantStatus: 502, wantText: "upstream_error", wantAccountID: 7},
		{name: "admin JWT can call", jwt: true, body: requestBody, upstreamCode: 200, upstreamBody: jsonResponse, upstreamMIME: "application/json", wantStatus: 200, wantText: `"object":"response"`, wantAccountID: 7},
		{name: "invalid account id", accountID: "0", body: requestBody, wantStatus: 400, wantText: "Invalid account ID"},
		{name: "missing account", accountID: "99", body: requestBody, wantStatus: 404, wantText: "account not found"},
		{name: "wrong platform", body: requestBody, adjust: func(account *service.Account) { account.Platform = service.PlatformAnthropic }, wantStatus: 400, wantText: "requires an OpenAI account"},
		{name: "disabled account can be tested", body: requestBody, adjust: func(account *service.Account) { account.Status = service.StatusDisabled }, upstreamCode: 200, upstreamBody: jsonResponse, upstreamMIME: "application/json", wantStatus: 200, wantText: `"object":"response"`, wantAccountID: 7},
		{name: "synthetic account cannot reach upstream", body: requestBody, adjust: func(account *service.Account) { account.Extra = map[string]any{"synthetic_ui_test": true} }, wantStatus: 503, wantText: "Synthetic test accounts"},
		{name: "unschedulable account can be tested", body: requestBody, adjust: func(account *service.Account) { account.Schedulable = false }, upstreamCode: 200, upstreamBody: jsonResponse, upstreamMIME: "application/json", wantStatus: 200, wantText: `"object":"response"`, wantAccountID: 7},
		{name: "account with errors can be tested", body: requestBody, adjust: func(account *service.Account) { account.Status = service.StatusError }, upstreamCode: 200, upstreamBody: jsonResponse, upstreamMIME: "application/json", wantStatus: 200, wantText: `"object":"response"`, wantAccountID: 7},
		{name: "locally rate limited account can be tested", body: requestBody, adjust: func(account *service.Account) { account.RateLimitResetAt = &future }, upstreamCode: 200, upstreamBody: jsonResponse, upstreamMIME: "application/json", wantStatus: 200, wantText: `"object":"response"`, wantAccountID: 7},
		{name: "bad stream", body: `{"model":"gpt-5.2","stream":"true"}`, wantStatus: 400, wantText: "invalid_request_error"},
		{name: "ambiguous model", body: `{"model":"gpt-5.2","model":"another"}`, wantStatus: 400, wantText: "more than once"},
		{name: "continuation", body: `{"model":"gpt-5.2","previous_response_id":"resp_live"}`, wantStatus: 400, wantText: "does not support previous_response_id"},
		{name: "conversation", body: `{"model":"gpt-5.2","conversation":"conv_live"}`, wantStatus: 400, wantText: "does not support previous_response_id"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			account := service.Account{
				ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{"api_key": "upstream-key"},
			}
			if tt.oauth {
				account.Type = service.AccountTypeOAuth
				account.Credentials = map[string]any{"access_token": "upstream-key"}
			}
			otherAccount := account
			otherAccount.ID = 8
			if tt.adjust != nil {
				tt.adjust(&account)
			}
			repo := &adminResponsesTestRepo{accounts: map[int64]service.Account{7: account, 8: otherAccount}}
			var calls []int64
			upstream := adminResponsesTestUpstream{forward: func(req *http.Request, accountID int64) *http.Response {
				calls = append(calls, accountID)
				if tt.oauth {
					require.Equal(t, "/backend-api/codex/responses", req.URL.Path)
				} else {
					require.Equal(t, "/v1/responses", req.URL.Path)
				}
				require.Empty(t, req.Header.Get("X-API-Key"))
				require.Equal(t, "Bearer upstream-key", req.Header.Get("Authorization"))
				body, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.Contains(t, gjson.GetBytes(body, "input").Raw, "hello")
				return &http.Response{
					StatusCode: tt.upstreamCode,
					Header:     http.Header{"Content-Type": []string{tt.upstreamMIME}},
					Body:       io.NopCloser(strings.NewReader(tt.upstreamBody)),
				}
			}}
			var slotAttempts int
			cache := &concurrencyCacheMock{acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) {
				slotAttempts++
				return false, nil
			}}
			cfg := &config.Config{}
			gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, service.NewConcurrencyService(cache),
				nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
			handler := &OpenAIGatewayHandler{gatewayService: gateway, cfg: cfg}
			router := gin.New()
			router.POST(prefix+":id"+suffix, func(c *gin.Context) {
				authMethod := service.AuditAuthMethodAdminAPIKey
				if tt.jwt {
					authMethod = "jwt"
				}
				c.Set("auth_method", authMethod)
			}, handler.AdminTestResponses)
			accountID := tt.accountID
			if accountID == "" {
				accountID = "7"
			}
			request := httptest.NewRequest(http.MethodPost, prefix+accountID+suffix, strings.NewReader(tt.body))
			if tt.jwt {
				request.Header.Set("Authorization", "Bearer inbound-secret")
			} else {
				request.Header.Set("X-API-Key", "admin-secret")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(t, tt.wantStatus, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), tt.wantText)
			require.NotContains(t, repo.lookups, int64(8))
			require.NotContains(t, response.Body.String(), "test_complete")
			require.Zero(t, slotAttempts)
			if tt.oauth {
				require.Contains(t, response.Header().Get("Content-Type"), "application/json")
			} else if tt.upstreamCode == 200 {
				require.Contains(t, response.Header().Get("Content-Type"), tt.upstreamMIME)
			}
			if tt.wantAccountID == 0 {
				require.Empty(t, calls)
			} else {
				require.Equal(t, []int64{tt.wantAccountID}, calls)
			}
		})
	}
}
