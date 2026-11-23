// sudoapi: Account responses test.

package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// AdminTestResponses
// POST /api/v1/admin/accounts/:id/test/v1/messages
func (h *OpenAIGatewayHandler) AdminTestResponses(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Invalid account ID")
		return
	}
	reqLog := requestLogger(c, "handler.admin.account_responses_test")
	maxBodySize := gatewayMaxBodySize(h.cfg)
	if maxBodySize <= 0 {
		maxBodySize = 32 << 20
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodySize)
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if !handleRequestBodyReadError(c, reqLog, err) {
			status, message := requestBodyReadFailure(reqLog, c.Request, err)
			h.errorResponse(c, status, "invalid_request_error", message)
		}
		return
	}
	if !utf8.Valid(body) || !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body must be a valid JSON object")
		return
	}
	if field, conflict := requestmodel.ConflictingField("application/json", body); conflict {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Request body declares field %.64q more than once", field))
		return
	}
	model := gjson.GetBytes(body, "model")
	if model.Type != gjson.String || strings.TrimSpace(model.String()) == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	stream, ok := parseOpenAICompatibleStream(body)
	if !ok {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", invalidStreamFieldTypeMessage)
		return
	}
	if hasResponsesTestContinuation(body) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Responses testing does not support previous_response_id or conversation; send the context in input")
		return
	}
	setOpenAIClientTransportHTTP(c)
	setOpsRequestContext(c, model.String(), stream)
	ctx := c.Request.Context()
	account, err := h.gatewayService.GetAccountForResponsesTest(ctx, accountID)
	if err != nil {
		if failoverClientGone(c) {
			return
		}
		status := infraerrors.Code(err)
		switch status {
		case http.StatusBadRequest, http.StatusNotFound:
			h.errorResponse(c, status, "invalid_request_error", infraerrors.Message(err))
		case http.StatusServiceUnavailable:
			h.errorResponse(c, status, "server_error", infraerrors.Message(err))
		default:
			h.errorResponse(c, http.StatusInternalServerError, "server_error", "Unable to prepare the specified account")
		}
		return
	}
	setOpsSelectedAccount(c, account.ID, account.Platform)
	request := c.Request
	c.Request = request.Clone(ctx)
	defer func() { c.Request = request }()
	c.Request.Header.Del("X-API-Key")
	c.Request.Header.Del("Authorization")
	_, err = h.gatewayService.Forward(ctx, c, account, body)
	if err == nil || failoverClientGone(c) {
		return
	}
	if h.handleForwardRequestBodyMemoryOverload(c, err, stream && c.Writer.Written()) {
		return
	}
	var failoverErr *service.UpstreamFailoverError
	if errors.As(err, &failoverErr) {
		h.handleFailoverExhausted(c, failoverErr, stream && c.Writer.Written())
		return
	}
	h.ensureForwardErrorResponse(c, stream && c.Writer.Written())
}

func hasResponsesTestContinuation(body []byte) bool {
	continuation := false
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		continuation = (strings.EqualFold(name, "previous_response_id") || strings.EqualFold(name, "conversation")) && value.Type != gjson.Null
		return !continuation
	})
	return continuation
}
