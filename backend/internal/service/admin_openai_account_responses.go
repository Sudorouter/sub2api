// sudoapi: Account responses test.

package service

import (
	"context"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func (s *OpenAIGatewayService) GetAccountForResponsesTest(ctx context.Context, accountID int64) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, ErrAccountNotFound
	}
	if !account.IsOpenAI() {
		return nil, infraerrors.BadRequest("RESPONSES_TEST_NOT_OPENAI", "Responses testing requires an OpenAI account")
	}
	if account.IsSyntheticUITest() {
		return nil, infraerrors.ServiceUnavailable("RESPONSES_TEST_ACCOUNT_UNAVAILABLE", "Synthetic test accounts cannot send upstream responses requests")
	}
	return account, nil
}
